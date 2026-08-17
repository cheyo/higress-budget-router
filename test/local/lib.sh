#!/usr/bin/env bash
# 本地联调公共变量与工具函数。其它脚本 source 本文件。
#
# 依赖：git-bash / WSL / Linux；docker CLI 可用。
# Windows git-bash 下 docker.exe 常不在 PATH，这里自动补上 Docker Desktop 的默认路径。

set -euo pipefail

if ! command -v docker >/dev/null 2>&1; then
  export PATH="/c/Program Files/Docker/Docker/resources/bin:$PATH"
fi
# 阻止 git-bash 把 /data 这类容器内路径改写成 Windows 路径
export MSYS_NO_PATHCONV=1
export MSYS2_ARG_CONV_EXCL='*'

GW=${GW:-higress-gw}                     # Higress all-in-one 容器
REDIS_CTR=${REDIS_CTR:-higress-redis}    # Redis 容器
MOCK_CTR=${MOCK_CTR:-higress-mock-llm}   # 假上游容器

GATEWAY=${GATEWAY:-http://127.0.0.1:8080}
VHOST=${VHOST:-budget.local}             # 测试路由的 Host
INGRESS_NAME=${INGRESS_NAME:-budget-mock}

TENANT=${TENANT:-acme}                   # 租户标识，随 x-tenant-id 头传入
RULE_NAME=${RULE_NAME:-tenant-daily-budget}
PERIOD=${PERIOD:-3600}
PREFIX=${PREFIX:-higress-ai-budget}

QUOTA_MICRO=${QUOTA_MICRO:-1000000}      # quota: 1 货币单位 = 1_000_000 微单位

# 预算计数键；租户名外层的 {} 是 Redis Cluster hash tag，插件里硬编码了这个格式
BUDGET_KEY="${PREFIX}:{${TENANT}}:${RULE_NAME}:${PERIOD}"

WASM_IN_GW=/opt/plugins/budget-router/plugin.wasm

# ---- 工具函数 ---------------------------------------------------------------

redis_cli() { docker exec "$REDIS_CTR" redis-cli "$@"; }

# 把水位直接设成指定的「已用微单位」，用于精确构造某一档
set_used() {
  redis_cli SET "$BUDGET_KEY" "$1" EX "$PERIOD" >/dev/null
  echo "used=$1  remain=$(awk "BEGIN{printf \"%.2f\", ($QUOTA_MICRO-$1)/$QUOTA_MICRO}")"
}

reset_budget() { redis_cli DEL "$BUDGET_KEY" >/dev/null; echo "budget key deleted"; }

show_budget() {
  local used ttl
  used=$(redis_cli GET "$BUDGET_KEY" || true)
  ttl=$(redis_cli TTL "$BUDGET_KEY" || true)
  echo "key=$BUDGET_KEY used=${used:-<nil>} ttl=${ttl}"
}

# 提示词保持纯 ASCII：git-bash 传中文到 curl -d 会被编码破坏，
# 上游收到的是非法 JSON，表现为 400 invalid json body——与插件无关的假故障。
PROMPT=${PROMPT:-"Explain the CAP theorem in one short paragraph."}

# 发一次非流式请求，返回响应体
ask() {
  local model=$1; shift
  curl -s --max-time 20 "$GATEWAY/v1/chat/completions" \
    -H "Host: $VHOST" \
    -H "Content-Type: application/json" \
    -H "x-tenant-id: $TENANT" \
    -d "{\"model\":\"$model\",\"messages\":[{\"role\":\"user\",\"content\":\"$PROMPT\"}]}" \
    "$@"
}

# 发一次流式请求（带 include_usage，否则上游不回 usage 帧，插件无从扣减）
ask_stream() {
  local model=$1; shift
  curl -s --max-time 30 -N "$GATEWAY/v1/chat/completions" \
    -H "Host: $VHOST" \
    -H "Content-Type: application/json" \
    -H "x-tenant-id: $TENANT" \
    -d "{\"model\":\"$model\",\"stream\":true,\"stream_options\":{\"include_usage\":true},\"messages\":[{\"role\":\"user\",\"content\":\"$PROMPT\"}]}" \
    "$@"
}

# 故意不带 include_usage：上游不会补 usage 帧，插件应当跳过扣减
ask_stream_no_usage() {
  local model=$1; shift
  curl -s --max-time 30 -N "$GATEWAY/v1/chat/completions" \
    -H "Host: $VHOST" \
    -H "Content-Type: application/json" \
    -H "x-tenant-id: $TENANT" \
    -d "{\"model\":\"$model\",\"stream\":true,\"messages\":[{\"role\":\"user\",\"content\":\"$PROMPT\"}]}" \
    "$@"
}

# 打开插件自身的日志（Envoy 默认 -l warning，插件的 Info/Debug 全被吞掉）
enable_plugin_log() { docker exec "$GW" curl -s -X POST "http://127.0.0.1:15000/logging?wasm=debug" >/dev/null; echo "wasm log level = debug"; }
plugin_log() { docker exec "$GW" grep -a "ai-budget-router:" /var/log/higress/gateway.log | tail -"${1:-10}"; }

# 从响应里取实际生效的模型名：上游 mock 会原样回显收到的 model，
# 因此响应里的 model 就是「插件改写后真正发出去的模型」
actual_model() { grep -o '"model"[[:space:]]*:[[:space:]]*"[^"]*"' | head -1 | sed 's/.*"\([^"]*\)"$/\1/'; }
