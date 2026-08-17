# 当前限制与生产前验证项

本文只记录当前版本需要使用者知道的限制和上线前必须确认的事项。

## 1. 当前限制

- **接口覆盖范围**：默认只处理 `/completions`、`/messages`、`/responses`、`/generateContent` 这几类 chat/generation 路径。`/embeddings`、`/rerank`、`/moderations` 等接口默认不参与预算统计。
- **请求体类型**：当前只支持 JSON 请求体的模型改写和计费链路。非 JSON、multipart、音频上传等请求不会被安全改写，也不建议纳入同一条预算规则。
- **计费依赖 usage**：插件按上游响应里的 `usage` 字段计算 input/output token 成本。上游不返回 usage 时，本次请求不会扣减预算。
- **响应压缩需实测**：如果上游或网关对 LLM 响应启用 `content-encoding`，需要确认 Proxy-Wasm 响应体 Hook 仍能读取 usage；否则可能漏账。
- **能力兼容由配置保证**：插件运行时不检查请求里是否使用 tools、vision、audio、json schema 等能力。请通过 `traffic_profile` 和 `model_capabilities` 在配置生效时校验降级目标是否能承接该路由的流量。
- **路由重选需验证**：插件会在请求体阶段改写模型字段和 `x-higress-llm-model` 路由头。目标 Higress 版本必须验证该改写能触发正确上游选择。

## 2. 生产前必须验证

| 项 | 目标 | 通过标准 |
|---|---|---|
| 插件加载 | WasmPlugin 已挂载到目标 Ingress | 网关日志出现预算字段，Redis 能看到扣减 |
| 模型降级 | 预算水位低于阈值时改写模型 | 上游收到的是降级后的模型 |
| 路由选择 | 改写路由头后进入目标上游 | 响应和上游日志都指向降级目标 |
| 预算扣减 | 响应 usage 能写入 Redis | Redis 累计值等于本次成本 |
| 429 拒绝 | 命中 `reject: true` 档时拒绝 | 返回配置的 `rejected_code` 和错误体 |
| gzip/压缩 | 响应压缩场景不漏账 | Redis 仍能记录本次 usage 成本 |
| fallback 计费 | 上游 fallback 后模型归属明确 | `budget_billed_model` 与实际执行模型一致 |
| 压测 | Redis 与网关容量足够 | 高并发下无大量 Redis 超时或 fail-open |

详细步骤见 [`production-verification.md`](production-verification.md)。

## 3. 上线建议

- 首次接入使用 `dry_run: true`，观察日志中的 `budget_level`、`budget_degraded`、`budget_request_bytes`。
- 确认水位、降级率、Redis 扣减都符合预期后，再切到 `dry_run: false`。
- `ai-token-ratelimit` 适合作为极限兜底，阈值应比本插件的降级和拒绝策略更宽松。
- 对 embeddings、rerank、audio、multipart 等流量，建议拆成独立路由和独立预算策略。
