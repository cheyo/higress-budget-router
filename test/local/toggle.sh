#!/usr/bin/env bash
# 开关插件：./toggle.sh on|off|dry-run
#
#   off      configDisable: true  —— 请求完全不经过本插件，用来取基线
#   on       正常生效，会真的改写 model / 429 拒绝
#   dry-run  走完整决策链并打标、照常扣减，但不改写 model —— 灰度模式
#
# 三种模式共用同一份 WasmPlugin，只改 configDisable 与 dry_run 两个字段，
# 保证对比时除了插件行为本身没有其它变量。

cd "$(dirname "$0")"
source ./lib.sh

MODE=${1:-on}
case "$MODE" in
  off)     DISABLE=true;  DRY=false ;;
  on)      DISABLE=false; DRY=false ;;
  dry-run) DISABLE=false; DRY=true  ;;
  *) echo "用法: $0 on|off|dry-run"; exit 1 ;;
esac

# 落在脚本目录而不是 /tmp：git-bash 的 /tmp 映射成 D:\tmp，docker cp 找不到
TMP=./.wasmplugin.tmp.yaml
trap 'rm -f "$TMP"' EXIT

cat > "$TMP" <<EOF
apiVersion: extensions.higress.io/v1alpha1
kind: WasmPlugin
metadata:
  name: ai-budget-router-0.1.0
  namespace: higress-system
  labels:
    higress.io/resource-definer: higress
    higress.io/wasm-plugin-name: ai-budget-router
    higress.io/wasm-plugin-version: 0.1.0
spec:
  # priority 越大越先执行；必须大于 ai-token-ratelimit(600) 与 ai-proxy(100)
  phase: UNSPECIFIED_PHASE
  priority: 800
  # 本地联调用 file:// 直接读容器内文件，不需要推镜像仓库
  url: file://$WASM_IN_GW
  defaultConfigDisable: true
  matchRules:
  # 注意是【裸 ingress 名】，不能写成 higress-system/budget-mock，
  # 否则匹配不上、插件对该路由静默不生效（内置 ai-proxy 也是这么写的）
  - ingress:
    - $INGRESS_NAME
    configDisable: $DISABLE
    config:
      rule_name: $RULE_NAME
      redis_key_prefix: $PREFIX

      redis:
        service_name: redis.dns
        service_port: 6379
        timeout: 1000

      # 用请求头带租户，免去为联调再配 key-auth consumer
      tenant_source:
        type: header
        key: x-tenant-id

      budget_period: $PERIOD
      quota: 1                       # 1 货币单位 = 1_000_000 微单位

      # 升序匹配：取第一条满足 remain <= threshold 的档
      degrade_levels:
      - { name: warn,      threshold: 0.50, model: mock-chat-backup }
      - { name: degrade,   threshold: 0.20, model: mock-chat-mini }
      - { name: exhausted, threshold: 0.00, reject: true }

      # 单价差拉大，便于肉眼观察扣减差异；也让 isCheaper() 判定明确
      model_prices:
        mock-chat:        { input: 100, output: 100 }
        mock-chat-backup: { input: 10,  output: 10  }
        mock-chat-mini:   { input: 1,   output: 1   }

      dry_run: $DRY
      fail_open: true
      record_attribute: true
EOF

docker cp "$TMP" "$GW:/data/wasmplugins/ai-budget-router-0.1.0.yaml"

echo "plugin mode = $MODE  (configDisable=$DISABLE dry_run=$DRY)"
# apiserver 监听 /data 文件变更 → controller 转 xDS → Envoy 重建 filter chain，
# 实测需要 10s 上下，急着发请求会打到旧配置上
echo "等待配置下发（约 15s）..."
sleep 15
