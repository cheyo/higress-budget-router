# 生产验证手册

本文用于在标准 **Kubernetes + Helm Higress** 环境中验证 `higress-budget-router` 是否可以进入生产流量。

用户手册说明“怎么部署和配置”，本文只说明“怎么证明它真的工作”。

## 1. 验证目标

上线前至少确认以下链路：

| 编号 | 目标 | 是否必须 |
|---|---|---|
| T0 | Higress、Redis、路由、插件对象都存在 | 必须 |
| T1 | 插件已加载并能写入预算日志 | 必须 |
| T2 | 正常请求能按 usage 写 Redis | 必须 |
| T3 | 预算低水位时能降级模型 | 必须 |
| T4 | 降级后能进入正确上游 | 必须 |
| T5 | 预算耗尽时返回 429 | 必须 |
| T6 | fallback / gzip / usage 场景风险明确 | 建议 |
| T7 | Redis 与网关容量满足预期并发 | 建议 |

T1 到 T5 不通过，不建议进入生产流量。

## 2. 前置条件

确认当前环境已经具备：

```bash
kubectl get pods -n higress-system
helm status higress -n higress-system
kubectl get wasmplugin -A
```

期望：

- `higress-console`、`higress-controller`、`higress-gateway` 正常 Running。
- Higress Helm release 状态为 `deployed`。
- `WasmPlugin` CRD 可查询。

确认业务测试命名空间：

```bash
kubectl get namespace
```

示例命名空间：

```text
budget-router-test
```

## 3. 验证对象

推荐先用 mock LLM 验证网关链路，再切真实 LLM provider。

| 对象 | 用途 |
|---|---|
| Redis | 保存预算已用额度 |
| mock LLM | 返回固定 OpenAI-compatible usage，便于验证扣费 |
| Higress Ingress / AI Route | 暴露 `/v1/chat/completions` |
| WasmPlugin | 挂载 `higress-budget-router` |
| Higress gateway 日志 | 判断预算字段和降级结果 |

生产验证也可以直接使用真实 LLM，但要注意真实 provider 的 usage、fallback、压缩行为可能不稳定，不适合第一轮排错。

## 4. T0 基础状态检查

### 4.1 Higress 状态

```bash
kubectl get pods -n higress-system -o wide
kubectl get svc -n higress-system
kubectl get ingressclass
```

通过标准：

- gateway Pod Running。
- `higress-gateway` Service 存在。
- `IngressClass` 中有 `higress`。

### 4.2 Redis 状态

```bash
kubectl get pods -n budget-router-test
kubectl get svc -n budget-router-test
```

通过标准：

- Redis Pod Running。
- Redis Service 可在集群内访问，例如 `redis.budget-router-test.svc.cluster.local:6379`。

如需验证 Redis 密码和连通性：

```bash
kubectl exec -n budget-router-test deploy/redis -- \
  redis-cli -a <REDIS_PASSWORD> PING
```

期望：

```text
PONG
```

### 4.3 LLM 上游状态

```bash
kubectl get pods -n budget-router-test
kubectl get svc -n budget-router-test
```

如果使用 mock LLM，集群内验证：

```bash
kubectl run curl-test -n budget-router-test --rm -it --image=curlimages/curl -- \
  curl -i http://mock-llm.budget-router-test.svc.cluster.local:8000/
```

通过标准：返回 `200 OK`。

## 5. T1 插件加载验证

应用或确认 WasmPlugin：

```bash
kubectl apply -f <budget-router-wasmplugin.yaml>
kubectl get wasmplugin -n higress-system
kubectl describe wasmplugin higress-budget-router -n higress-system
```

通过标准：

- `higress-budget-router` 存在。
- `spec.url` 指向正确 OCI 镜像。
- `matchRules.ingress` 指向真实路由。
- `configDisable: false`。
- `priority: 800`。

查看 gateway 是否有插件相关日志：

```bash
kubectl logs -n higress-system deploy/higress-gateway --since=10m | findstr /i "higress-budget-router budget_"
```

Linux/macOS 使用：

```bash
kubectl logs -n higress-system deploy/higress-gateway --since=10m | grep -Ei "higress-budget-router|budget_"
```

## 6. T2 正常请求计费验证

目标：预算充足时不降级，但响应 usage 能写入 Redis。

### 6.1 清空预算

```bash
kubectl exec -n budget-router-test deploy/redis -- \
  redis-cli -a <REDIS_PASSWORD> DEL "higress-ai-budget:{test-tenant}:tenant-daily-budget:86400"
```

### 6.2 发请求

确保 gateway 端口可访问。示例使用本机端口转发：

```bash
kubectl -n higress-system port-forward svc/higress-gateway 18080:80
```

另开一个终端：

```bash
curl.exe --noproxy "*" -i -X POST "http://127.0.0.1:18080/v1/chat/completions" `
  -H "Host: llm.local" `
  -H "Content-Type: application/json" `
  --data-raw '{"model":"gpt-4o","messages":[{"role":"user","content":"hello"}]}'
```

Linux/macOS：

```bash
curl --noproxy "*" -i -X POST "http://127.0.0.1:18080/v1/chat/completions" \
  -H "Host: llm.local" \
  -H "Content-Type: application/json" \
  --data-raw '{"model":"gpt-4o","messages":[{"role":"user","content":"hello"}]}'
```

### 6.3 检查 Redis

```bash
kubectl exec -n budget-router-test deploy/redis -- \
  redis-cli -a <REDIS_PASSWORD> GET "higress-ai-budget:{test-tenant}:tenant-daily-budget:86400"
```

通过标准：

- 返回整数。
- 该整数等于本次 usage 按 `model_prices` 折算后的 `cost_micro`。

例如 mock LLM 返回：

```json
"usage": {"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15}
```

若计费模型价格为：

```yaml
mock-model: { input: 1, output: 1 }
```

则本次成本为：

```text
10 × 1 + 5 × 1 = 15
```

## 7. T3 预算降级验证

目标：预算低于阈值时，请求模型被改写为降级模型。

### 7.1 预置预算水位

假设：

```yaml
quota: 0.00004
degrade_levels:
  - { name: warn, threshold: 0.50, model: mock-model }
```

`quota: 0.00004` 等于 40 微单位。预置已用 30 微单位，剩余比例为 25%，会命中 `warn`。

```bash
kubectl exec -n budget-router-test deploy/redis -- \
  redis-cli -a <REDIS_PASSWORD> SET "higress-ai-budget:{test-tenant}:tenant-daily-budget:86400" 30 EX 86400
```

### 7.2 发请求

```bash
curl.exe --noproxy "*" -i -X POST "http://127.0.0.1:18080/v1/chat/completions" `
  -H "Host: llm.local" `
  -H "Content-Type: application/json" `
  --data-raw '{"model":"gpt-4o","messages":[{"role":"user","content":"hello"}]}'
```

通过标准：

- HTTP 返回 200。
- mock LLM 收到的 body 中 `model` 已变成降级模型。
- gateway 日志中：
  - `budget_original_model` 为原始模型。
  - `budget_actual_model` 为降级模型。
  - `budget_degraded=true`。
  - `budget_level` 为命中档位。

查看日志：

```bash
kubectl logs -n higress-system deploy/higress-gateway --since=10m | findstr /i "budget_level budget_degraded budget_original_model budget_actual_model ai_log"
```

## 8. T4 路由重选验证

目标：插件改写 `model` 和 `x-higress-llm-model` 后，请求进入目标上游。

这是最关键的生产验证。仅看到响应 200 不够，必须证明流量打到了降级后的上游。

推荐方式：

1. 配两条 Higress 路由：
   - `x-higress-llm-model = gpt-4o` → upstream A
   - `x-higress-llm-model = mock-model` 或低价模型 → upstream B
2. 让 upstream A / B 返回不同的 `model` 或不同响应标记。
3. 预置 Redis 水位触发降级。
4. 发 `gpt-4o` 请求。
5. 判断请求是否进入 upstream B。

通过标准：

| 现象 | 结论 |
|---|---|
| 响应来自 upstream B，upstream B 收到降级后模型 | 通过 |
| body 被改写，但请求仍进入 upstream A | 不通过，不能进生产 |
| body 没被改写 | 插件未生效，回到 T1/T3 排查 |

不通过时的处理方向：

- 检查 `model_to_header` 是否为 `x-higress-llm-model`。
- 检查 Higress 路由是否真的按该 header 分流。
- 检查插件 phase / priority。
- 如目标 Higress 不支持请求体阶段改写后的路由重选，需要调整实现策略。

## 9. T5 预算耗尽 429 验证

目标：预算耗尽时插件直接拒绝，不再请求 LLM 上游。

### 9.1 预置预算耗尽

```bash
kubectl exec -n budget-router-test deploy/redis -- \
  redis-cli -a <REDIS_PASSWORD> SET "higress-ai-budget:{test-tenant}:tenant-daily-budget:86400" <QUOTA_MICRO> EX 86400
```

例如 `quota: 0.00004`，则 `<QUOTA_MICRO>` 为 `40`。

### 9.2 发请求

```bash
curl.exe --noproxy "*" -i -X POST "http://127.0.0.1:18080/v1/chat/completions" `
  -H "Host: llm.local" `
  -H "Content-Type: application/json" `
  --data-raw '{"model":"gpt-4o","messages":[{"role":"user","content":"hello"}]}'
```

通过标准：

```text
HTTP/1.1 429 Too Many Requests
x-higress-budget-level: exhausted
```

响应体符合 `rejected_message`。

Redis 已用值不应因为被拒绝请求而增加。

## 10. T6 fallback / gzip / usage 风险验证

这些不是每个环境都必须首日完成，但生产前必须明确结论。

### 10.1 usage 缺失

让上游返回不含 `usage` 的响应。

通过标准：

- 请求正常返回。
- Redis 不扣费。
- 日志能看到没有 usage 的排查线索。

结论要写入生产说明：没有 usage 的模型或接口不会被本插件扣费。

### 10.2 gzip / content-encoding

让上游或网关返回带 `content-encoding` 的响应。

通过标准：

- 如果 Redis 正常扣费，记录为已支持。
- 如果 Redis 不扣费，需要禁用该路由响应压缩，或在当前限制中明确说明。

### 10.3 fallback 计费模型

让低价模型上游返回错误，触发 `ai-proxy` fallback。

通过标准：

- `budget_billed_model` 应尽量等于真实执行模型。
- 如果只能拿到降级目标模型，需要在生产说明中写明计费误差边界。

## 11. T7 Redis 和高并发验证

目标：确认 Redis 调用不会成为生产瓶颈。

推荐用真实压测工具，例如 `hey` 或 `wrk`：

```bash
hey -n 5000 -c 200 -m POST \
  -H "Host: llm.local" \
  -H "Content-Type: application/json" \
  -d '{"model":"gpt-4o","messages":[{"role":"user","content":"hello"}]}' \
  http://127.0.0.1:18080/v1/chat/completions
```

观察：

```bash
kubectl logs -n higress-system deploy/higress-gateway --since=10m | findstr /i "redis eval dispatch failed read water level failed deduct failed"
```

通过标准：

- 无大量 Redis 超时。
- Redis 计数与请求量趋势一致。
- P99 延迟增量在业务可接受范围内。

## 12. 验证结果记录

| 项 | 日期 | 结果 | 备注 |
|---|---|---|---|
| Higress 版本 | | | |
| 插件镜像 | | | |
| T0 基础状态 | | ☐ 通过 ☐ 不通过 | |
| T1 插件加载 | | ☐ 通过 ☐ 不通过 | |
| T2 正常计费 | | ☐ 通过 ☐ 不通过 | |
| T3 预算降级 | | ☐ 通过 ☐ 不通过 | |
| T4 路由重选 | | ☐ 通过 ☐ 不通过 | |
| T5 429 拒绝 | | ☐ 通过 ☐ 不通过 | |
| T6 usage/gzip/fallback | | ☐ 通过 ☐ 有限制 | |
| T7 高并发 | | ☐ 通过 ☐ 不通过 | |

发现新的限制时，同步更新：

- [`known-issues.md`](known-issues.md)
- [`user-manual.md`](user-manual.md)

## 13. 附录：本地 mock LLM 验证

如果没有真实 provider，推荐用 mock LLM 完成第一轮链路验证。

mock LLM 要满足：

- 支持 `/v1/chat/completions`。
- 返回 OpenAI-compatible JSON。
- 响应中包含固定 `usage`。
- 可以回显收到的请求体，便于确认模型是否被改写。

示例响应：

```json
{
  "id": "chatcmpl-mock",
  "object": "chat.completion",
  "model": "mock-model",
  "choices": [
    {
      "index": 0,
      "message": {
        "role": "assistant",
        "content": "hello from mock llm"
      },
      "finish_reason": "stop"
    }
  ],
  "usage": {
    "prompt_tokens": 10,
    "completion_tokens": 5,
    "total_tokens": 15
  }
}
```

mock 验证通过后，再替换为真实 LLM provider 做最终验证。
