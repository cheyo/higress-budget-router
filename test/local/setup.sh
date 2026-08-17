#!/usr/bin/env bash
# 一次性把本地联调链路搭起来：
#   1. 把 redis / mock-llm 注册进 McpBridge（Envoy 才有对应 cluster）
#   2. 下发一条指向 mock-llm 的测试 Ingress（Host: budget.local）
#   3. 把 main.wasm 拷进网关容器
#   4. 下发 WasmPlugin（默认 off，用 toggle.sh 切换）
#
# 幂等：重复执行不会重复注册。
# 所有写入都走 docker cp 而不是 docker exec + 重定向——后者在容器里没有
# 可用的 shell 重定向权限时会失败，且改坏了不好回滚。

cd "$(dirname "$0")"
source ./lib.sh

WASM_SRC=${WASM_SRC:-../../main.wasm}
TMPDIR_LOCAL=./.setup.tmp
trap 'rm -rf "$TMPDIR_LOCAL"' EXIT
mkdir -p "$TMPDIR_LOCAL"

echo "==> 0. 前置检查"
[ -f "$WASM_SRC" ] || { echo "找不到 $WASM_SRC，先执行: bash test/local/build.sh"; exit 1; }
docker inspect "$GW" >/dev/null || { echo "$GW 容器不在，先起 docker-compose.native.yml"; exit 1; }
echo "    wasm: $(wc -c < "$WASM_SRC") bytes"

echo "==> 1. 注册 redis / mock-llm 到 McpBridge"
docker cp "$GW:/data/mcpbridges/default.yaml" "$TMPDIR_LOCAL/mcpbridge.yaml"
if grep -q "name: mock-llm" "$TMPDIR_LOCAL/mcpbridge.yaml"; then
  echo "    已注册，跳过"
else
  cp "$TMPDIR_LOCAL/mcpbridge.yaml" "./mcpbridge.backup.yaml"
  echo "    原文件已备份到 test/local/mcpbridge.backup.yaml"
  # 在 registries: 之后插入两条 dns 注册。
  # 必须用带点的 FQDN——McpBridge 的 dns 类型会拒绝裸主机名，
  # 且错误只出现在 controller.log，数据面表现为静默 503。
  awk '
    /^  registries:/ {
      print
      print "  # BUDGET_ROUTER_TEST_START"
      print "  - domain: redis.local"
      print "    name: redis"
      print "    port: 6379"
      print "    type: dns"
      print "  - domain: mock-llm.local"
      print "    name: mock-llm"
      print "    port: 8000"
      print "    type: dns"
      print "  # BUDGET_ROUTER_TEST_END"
      next
    }
    { print }
  ' "$TMPDIR_LOCAL/mcpbridge.yaml" > "$TMPDIR_LOCAL/mcpbridge.new.yaml"
  docker cp "$TMPDIR_LOCAL/mcpbridge.new.yaml" "$GW:/data/mcpbridges/default.yaml"
  echo "    registered: redis.dns:6379 / mock-llm.dns:8000"
fi

echo "==> 2. 下发测试 Ingress（Host: $VHOST -> mock-llm.dns）"
cat > "$TMPDIR_LOCAL/ingress.yaml" <<EOF
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  annotations:
    higress.io/destination: mock-llm.dns
    higress.io/ignore-path-case: "false"
  labels:
    higress.io/resource-definer: higress
  name: $INGRESS_NAME
  namespace: higress-system
spec:
  ingressClassName: higress
  rules:
  - host: $VHOST
    http:
      paths:
      - backend:
          resource:
            apiGroup: networking.higress.io
            kind: McpBridge
            name: default
        path: /
        pathType: Prefix
EOF
docker cp "$TMPDIR_LOCAL/ingress.yaml" "$GW:/data/ingresses/$INGRESS_NAME.yaml"
echo "    /data/ingresses/$INGRESS_NAME.yaml"

echo "==> 3. 拷贝 wasm 进网关容器"
docker exec "$GW" mkdir -p "$(dirname $WASM_IN_GW)"
docker cp "$WASM_SRC" "$GW:$WASM_IN_GW"
echo "    $WASM_IN_GW"

echo "==> 4. 打开插件日志（Envoy 默认 -l warning，否则插件日志全看不到）"
enable_plugin_log

echo "==> 5. 下发 WasmPlugin（初始为 off）"
./toggle.sh off

cat <<'EOS'

完成。对比测试三步：
  bash test/local/toggle.sh off     && bash test/local/cases.sh baseline
  bash test/local/toggle.sh dry-run && bash test/local/cases.sh dry-run
  bash test/local/toggle.sh on      && bash test/local/cases.sh plugin
EOS
