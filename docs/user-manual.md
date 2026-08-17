# 用户手册：生产级接入 Higress Budget Router

本文面向使用 `higress-budget-router` 的网关运维、平台工程师和业务接入方，目标是把插件接入一套生产级 Higress AI Gateway 链路。

## 1. 生产链路长什么样

```text
Client
  │ OpenAI-compatible request
  ▼
Higress Gateway
  │
  ├─ 认证插件：识别 consumer / tenant
  ├─ model-router / AI route：从 body.model 得到目标模型
  ├─ higress-budget-router：查预算，必要时改写 model
  ├─ ai-token-ratelimit：极限兜底
  └─ ai-proxy：协议转换、模型提供者转发、fallback
       │
       ├─ OpenAI / Azure OpenAI
       ├─ Qwen / DashScope
       ├─ DeepSeek
       └─ 其它 LLM Provider

Redis
  └─ 保存 tenant 周期预算已用额度
```

本插件只负责预算判断、模型降级、429 拒绝和预算扣减。真实模型 API Key、模型提供者、模型路由由 Higress 原生 AI 网关能力配置。

## 2. 生产前置条件

| 项 | 要求 |
|---|---|
| Higress | Kubernetes + Helm 部署的标准 Higress 环境 |
| 控制台 | 能进入 Higress Console，并能看到 AI 服务提供者、AI 路由、插件市场等菜单 |
| Redis | 网关 Pod 可访问，建议生产使用高可用 Redis |
| 插件镜像 | `main.wasm` 已构建并推送到可被集群拉取的 OCI 镜像仓库 |
| LLM Provider | 至少配置原始模型和降级模型对应的 provider / API Key |
| 租户识别 | 推荐使用 Higress consumer；也可用业务 header，例如 `x-tenant-id` |
| 验证 | 按 `docs/production-verification.md` 完成 dry_run、降级、429、Redis 扣费验证 |

## 3. 规划生产策略

先明确这几件事，再写配置：

| 决策项 | 示例 |
|---|---|
| 对外域名 | `api.example.com` |
| API 路径 | `/v1/chat/completions`、`/v1/responses` |
| 原始高价模型 | `gpt-4o` |
| 第一档降级模型 | `qwen-plus` |
| 第二档降级模型 | `deepseek-v3` |
| 租户来源 | `consumer` 或 header `x-tenant-id` |
| 预算周期 | 86400 秒，即一天 |
| 周期预算 | 例如每租户每天 200 元 |
| Redis key 前缀 | `higress-ai-budget` |
| 首次上线模式 | `dry_run: true` |

预算默认按以下维度统计：

```text
redis_key_prefix + tenant + rule_name + budget_period
```

例如：

```text
higress-ai-budget:{tenant-a}:tenant-daily-budget:86400
```

同一个租户在同一个 `rule_name` 下，无论请求哪个模型，默认都会扣在同一份预算里。要拆不同业务预算，就拆 route 或使用不同 `rule_name`。

## 4. 配置 Redis

生产建议使用独立 Redis 或 Redis Cluster。插件需要 Higress 能通过 McpBridge 访问 Redis。

GitOps 方式示例：

```yaml
apiVersion: networking.higress.io/v1
kind: McpBridge
metadata:
  name: default
  namespace: higress-system
spec:
  registries:
    - name: redis
      type: dns
      domain: redis-master.redis.svc.cluster.local
      port: 6379
```

插件配置里使用：

```yaml
redis:
  service_name: redis.dns
  service_port: 6379
  timeout: 1000
  username: default
  password: ${REDIS_PASSWORD}
```

生产注意：

- Redis 密码不要提交到公开仓库。
- 建议通过 Higress / Kubernetes 的密钥能力管理敏感信息。
- `fail_open: true` 表示 Redis 异常时放行；金融、强成本控制场景可以评估 `fail_open: false`。

## 5. 配置模型提供者

在 Higress Console 中进入 **LLM Provider Management / AI 服务提供者管理**：

1. 添加原始高价模型的 provider，例如 OpenAI / Azure OpenAI。
2. 添加第一档降级模型 provider，例如 Qwen / DashScope。
3. 添加第二档降级模型 provider，例如 DeepSeek。
4. 为每个 provider 配置 API Key、Base URL、模型名称映射。
5. 确认 provider 连通性测试通过。

示例规划：

| 对外请求模型 | Provider | 实际上游模型 | 用途 |
|---|---|---|---|
| `gpt-4o` | OpenAI / Azure OpenAI | `gpt-4o` | 正常预算下使用 |
| `qwen-plus` | DashScope / Qwen | `qwen-plus` | 预算低于 30% 时使用 |
| `deepseek-v3` | DeepSeek | `deepseek-v3` | 预算低于 10% 时使用 |

这里的 provider API Key 不属于本插件配置。本插件只会把请求里的 `model` 改成目标模型名，后续由 Higress 原生 AI 路由和 `ai-proxy` 转发。

## 6. 配置 Higress 路由

在 Higress Console 中进入 **AI Route Config / AI 路由配置** 或普通路由配置：

1. 创建对外域名，例如 `api.example.com`。
2. 创建 chat 路由，例如 `/v1/chat/completions`。
3. 将该路由接入 Higress AI 网关的 `ai-proxy`。
4. 配置模型到 provider 的映射：
   - `gpt-4o` → OpenAI / Azure OpenAI
   - `qwen-plus` → Qwen / DashScope
   - `deepseek-v3` → DeepSeek
5. 如需高可用，给每个模型配置 fallback。
6. 记录这条路由对应的 Ingress 名称，后面 WasmPlugin 的 `matchRules.ingress` 要用。

生产建议按流量能力拆路由：

| 路由 | 流量类型 | 可降级到 |
|---|---|---|
| `llm-text` | 纯文本 / tools | `qwen-plus`、`deepseek-v3` |
| `llm-vision` | 图片输入 / vision | 支持 vision 的低价模型，例如 `gpt-4o-mini` |
| `llm-audio` | audio / multipart | 不建议接入当前预算降级链路，单独设计 |

原因：插件运行时不检查请求内容。vision 流量不能降到不支持 vision 的模型，audio/multipart 当前也不建议放进同一条 JSON 降级链路。

## 7. 配置插件镜像

构建并推送插件镜像：

```bash
go test ./... -count=1
$env:GOOS="wasip1"
$env:GOARCH="wasm"
go vet ./...
go build -buildmode=c-shared -o main.wasm ./
docker build -t ghcr.io/<OWNER>/higress-budget-router:<VERSION> .
docker push ghcr.io/<OWNER>/higress-budget-router:<VERSION>
```

镜像内容是 Wasm 插件，不是普通业务容器：

```dockerfile
FROM scratch
COPY main.wasm plugin.wasm
```

如果镜像是 private，需要确保 Higress gateway 所在集群有拉取权限。生产更推荐使用受控的企业镜像仓库或设置好 GHCR package 权限。

## 8. 创建生产 WasmPlugin

下面是一份生产级配置模板。首次上线请保持 `dry_run: true`。

```yaml
apiVersion: extensions.higress.io/v1alpha1
kind: WasmPlugin
metadata:
  name: higress-budget-router
  namespace: higress-system
spec:
  phase: UNSPECIFIED_PHASE
  priority: 800
  url: oci://ghcr.io/<OWNER>/higress-budget-router:<VERSION>
  defaultConfigDisable: true
  matchRules:
    - ingress:
        - higress-system/llm-text
      configDisable: false
      config:
        rule_name: tenant-daily-budget
        redis:
          service_name: redis.dns
          service_port: 6379
          timeout: 1000

        tenant_source:
          type: consumer

        budget_period: 86400
        quota: 200

        traffic_profile: [tools]

        model_prices:
          gpt-4o:      { input: 2.5,  output: 10   }
          qwen-plus:   { input: 0.8,  output: 2    }
          deepseek-v3: { input: 0.14, output: 0.28 }
        default_price: { input: 1, output: 1 }

        model_capabilities:
          gpt-4o:      { tools: true, vision: true,  audio: false, json_schema: true  }
          qwen-plus:   { tools: true, vision: false, audio: false, json_schema: true  }
          deepseek-v3: { tools: true, vision: false, audio: false, json_schema: false }

        degrade_levels:
          - name: warn
            threshold: 0.30
            model: qwen-plus
          - name: degrade
            threshold: 0.10
            model: deepseek-v3
            max_request_bytes: 120000
          - name: exhausted
            threshold: 0.0
            reject: true

        model_key: model
        model_to_header: x-higress-llm-model
        enable_on_path_suffix:
          - /completions
          - /messages
          - /responses
          - /generateContent

        rejected_code: 429
        rejected_message: '{"error":{"code":"budget_exhausted","message":"tenant AI budget exhausted"}}'

        dry_run: true
        fail_open: true
        record_attribute: true
        max_body_bytes: 10485760
```

应用：

```bash
kubectl apply -f budget-router-wasmplugin.yaml
kubectl get wasmplugin -n higress-system
kubectl describe wasmplugin higress-budget-router -n higress-system
```

关键点：

- `priority: 800` 必须大于 `ai-token-ratelimit`、`ai-proxy` 等后续插件。
- `defaultConfigDisable: true` 表示默认不全局启用，只在 `matchRules` 指定路由启用。
- `matchRules.ingress` 必须写真实路由的 `namespace/name`。
- `tenant_source.type: consumer` 依赖认证插件产生 `x-mse-consumer`。
- 如果没有 consumer，可改成：

```yaml
tenant_source:
  type: header
  key: x-tenant-id
```

## 9. 多路由生产配置

如果同时有纯文本和 vision 流量，不要混在同一条 route。推荐这样拆：

```yaml
matchRules:
  - ingress:
      - higress-system/llm-text
    configDisable: false
    config:
      rule_name: tenant-daily-budget
      tenant_source: { type: header, key: x-tenant-id }
      redis: { service_name: redis.dns, service_port: 6379, timeout: 1000 }
      budget_period: 86400
      quota: 500
      traffic_profile: [tools]
      model_prices:
        gpt-4o:      { input: 2.5,  output: 10 }
        qwen-plus:   { input: 0.8,  output: 2 }
        deepseek-v3: { input: 0.14, output: 0.28 }
      model_capabilities:
        qwen-plus:   { tools: true, vision: false }
        deepseek-v3: { tools: true, vision: false }
      degrade_levels:
        - { name: warn, threshold: 0.40, model: qwen-plus }
        - { name: degrade, threshold: 0.15, model: deepseek-v3, max_request_bytes: 120000 }
        - { name: exhausted, threshold: 0.0, reject: true }
      dry_run: true
      fail_open: true
      record_attribute: true

  - ingress:
      - higress-system/llm-vision
    configDisable: false
    config:
      rule_name: tenant-daily-budget
      tenant_source: { type: header, key: x-tenant-id }
      redis: { service_name: redis.dns, service_port: 6379, timeout: 1000 }
      budget_period: 86400
      quota: 500
      traffic_profile: [vision, tools]
      model_prices:
        gpt-4o:      { input: 2.5,  output: 10 }
        gpt-4o-mini: { input: 0.15, output: 0.6 }
      model_capabilities:
        gpt-4o-mini: { tools: true, vision: true }
      degrade_levels:
        - { name: degrade, threshold: 0.30, model: gpt-4o-mini, max_request_bytes: 200000 }
        - { name: exhausted, threshold: 0.0, reject: true }
      dry_run: true
      fail_open: true
      record_attribute: true
```

两条规则使用相同 `rule_name` 和同一租户时，共用一份预算。想拆开预算，就把 `rule_name` 改成不同值。

## 10. 配置字段参考

### 10.1 租户与预算

| 字段 | 类型 | 默认 | 说明 |
|---|---|---|---|
| `rule_name` | string | 必填 | 参与 Redis key 构造。相同租户、相同 `rule_name`、相同 `budget_period` 共用一份预算 |
| `tenant_source.type` | enum | `consumer` | `consumer` / `header` / `param` / `cookie` / `fixed` |
| `tenant_source.key` | string | `x-mse-consumer` | 非 `consumer` 类型必填 |
| `budget_period` | int | `86400` | 预算周期，单位秒。Redis TTL 到期后自动重置 |
| `quota` | float | 必填 | 周期总预算，单位必须和 `model_prices` 一致 |
| `redis_key_prefix` | string | `higress-ai-budget` | Redis key 前缀 |

`tenant_source.type` 建议：

| 类型 | 适用场景 |
|---|---|
| `consumer` | 推荐。通过 Higress consumer 识别租户 |
| `header` | 网关前已有 BFF 或业务网关注入租户 header |
| `fixed` | 单租户或本地验证 |
| `param` / `cookie` | 特定浏览器或调试场景 |

### 10.2 Redis

| 字段 | 类型 | 默认 | 说明 |
|---|---|---|---|
| `redis.service_name` | string | 必填 | McpBridge 注册的服务名 |
| `redis.service_port` | int | `6379` | `.static` 服务默认 `80` |
| `redis.timeout` | int | `1000` | 毫秒 |
| `redis.username` | string | 空 | Redis 用户名 |
| `redis.password` | string | 空 | Redis 密码 |
| `redis.database` | int | `0` | Redis database |

### 10.3 模型与价格

| 字段 | 类型 | 默认 | 说明 |
|---|---|---|---|
| `model_prices.<model>.input` | float | 无 | 每 100 万输入 token 的价格 |
| `model_prices.<model>.output` | float | 无 | 每 100 万输出 token 的价格 |
| `default_price.input` | float | `1` | 未知模型输入 token 兜底价格 |
| `default_price.output` | float | `1` | 未知模型输出 token 兜底价格 |
| `model_key` | string | `model` | 请求体中模型字段路径 |
| `model_to_header` | string | `x-higress-llm-model` | 同步改写的路由头，空串表示不改 |

所有降级目标模型都必须出现在 `model_prices` 中。否则配置会被拒绝，避免使用 `default_price` 记出错误账。

### 10.4 能力声明

| 字段 | 类型 | 默认 | 说明 |
|---|---|---|---|
| `traffic_profile` | []string | 空 | 本 route 会使用的能力 |
| `model_capabilities` | object | 空 | 各模型支持的能力表 |

`traffic_profile` 可选值：

| 值 | 含义 |
|---|---|
| `tools` | 工具 / 函数调用 |
| `vision` | 图片输入 |
| `audio` | 音频输入 |
| `json_schema` | 结构化输出 |

建议填写 `model_capabilities`。插件会在配置生效时校验降级目标是否能承接该 route 的能力要求。

### 10.5 降级阶梯

| 字段 | 类型 | 说明 |
|---|---|---|
| `degrade_levels[].name` | string | 档位名，写入 `budget_level` |
| `degrade_levels[].threshold` | float | 剩余预算比例，[0,1] |
| `degrade_levels[].model` | string | 目标模型。空表示只打标不改写 |
| `degrade_levels[].reject` | bool | 直接返回拒绝响应 |
| `degrade_levels[].max_request_bytes` | int | 请求体超过此大小时不降级 |

匹配规则：按 `threshold` 升序排列，取第一条满足 `remain <= threshold` 的档位。

### 10.6 运行开关

| 字段 | 类型 | 默认 | 说明 |
|---|---|---|---|
| `enable_on_path_suffix` | []string | `/completions`、`/messages`、`/responses`、`/generateContent` | 插件生效路径。当前不建议随意扩展到 embeddings/audio 等接口 |
| `rejected_code` | int | `429` | 预算拒绝状态码 |
| `rejected_message` | string | JSON 错误体 | 返回给客户端的错误内容 |
| `dry_run` | bool | `false` | 只做决策和记录，不改写、不拒绝 |
| `fail_open` | bool | `true` | Redis 异常时是否放行 |
| `record_attribute` | bool | `true` | 是否写入 AI 访问日志字段 |
| `max_body_bytes` | int | `10485760` | 请求体缓冲上限 |

## 11. 校准 max_request_bytes

`max_request_bytes` 用来避免把长上下文请求降级到上下文窗口更小的模型后失败。这个值不建议拍脑袋，要用 dry_run 数据校准。

### 11.1 收集数据

dry_run 期间记录两类数据：

| 字段 | 来源 | 含义 |
|---|---|---|
| `budget_request_bytes` | 插件日志 | 请求体字节数 |
| `usage.prompt_tokens` | 上游响应 usage | 真实输入 token 数 |

### 11.2 计算字节/token

```text
字节/token = budget_request_bytes ÷ prompt_tokens
```

取观测到的最小值，不取平均值。平均值会低估一部分请求，低估会导致“以为降级后装得下，实际请求失败”。

参考量级：

| 语言构成 | 常见字节/token |
|---|---:|
| 纯英文 | 约 4 |
| 中英混杂 | 约 3 |
| 纯中文 | 约 2.5 |

### 11.3 计算阈值

```text
max_request_bytes = 目标模型窗口 × 安全比例 × 最小字节/token
```

安全比例建议从 `0.5` 开始。比如目标模型窗口 64K，最小字节/token 为 2：

```text
64000 × 0.5 × 2 = 64000 字节
```

上线后观察 `budget_degrade_blocked_by=request_too_large`：

| 占比 | 判断 |
|---|---|
| 接近 0 | 阈值偏保守，可以适当放宽 |
| 5% 到 15% | 合理 |
| 大于 30% | 阈值太紧，或目标模型窗口太小 |

## 12. 配置期校验

插件在配置生效时做校验。任何一条不通过，整份配置不生效，Higress 保留当前有效配置。

| 检查 | 目的 |
|---|---|
| 降级目标必须在 `model_prices` 中 | 防止用 `default_price` 记假账 |
| 预算越紧张，目标模型必须越便宜 | 防止阶梯配反 |
| `threshold` 不能重复 | 防止后面的档位永远匹配不到 |
| `reject` 档必须是 threshold 最小的一档 | 保证预算耗尽才拒绝 |
| `max_request_bytes` 不能超过 `max_body_bytes` | 防止永远触发不了 |
| 目标模型满足 `traffic_profile` | 防止降级后能力不兼容 |
| 能力项拼写必须合法 | 防止配置静默失效 |
| `threshold` 在 `[0,1]`，`max_request_bytes >= 0` | 防止明显配置错误 |

常见错误示例：

```text
degrade_levels[degrade] 的目标模型 "qwen-plus" 不满足 traffic_profile：
需要 "vision"，但 model_capabilities.qwen-plus.vision = false。
```

处理方式：更换目标模型，或把不同能力的流量拆到独立 route。

## 13. 灰度上线流程

### 阶段一：dry_run 观察

配置：

```yaml
dry_run: true
record_attribute: true
fail_open: true
```

效果：

- 插件会查 Redis 水位。
- 插件会计算命中哪个预算档位。
- 插件会记录日志属性。
- 插件会根据响应 usage 扣 Redis。
- 插件不会真的改写模型。
- 插件不会真的返回 429。

观察 1 到 3 天：

| 字段 | 看什么 |
|---|---|
| `budget_level` | `normal` / `warn` / `degrade` / `exhausted` 分布 |
| `budget_degraded` | dry_run 下应为 false |
| `budget_remain_ratio` | 阈值是否合理 |
| `budget_request_bytes` | 用于估算 `max_request_bytes` |
| `budget_cost_micro` | 成本是否和 provider usage 接近 |

### 阶段二：小流量真降级

只对低风险路由或少量租户改成：

```yaml
dry_run: false
```

验证：

```bash
curl -i -X POST "http://<gateway>/v1/chat/completions" \
  -H "Host: api.example.com" \
  -H "Content-Type: application/json" \
  -H "x-tenant-id: tenant-a" \
  --data-raw '{"model":"gpt-4o","messages":[{"role":"user","content":"hello"}]}'
```

检查：

```bash
kubectl logs -n higress-system deploy/higress-gateway --since=10m | grep budget_
```

Redis：

```bash
redis-cli GET "higress-ai-budget:{tenant-a}:tenant-daily-budget:86400"
redis-cli TTL "higress-ai-budget:{tenant-a}:tenant-daily-budget:86400"
```

### 阶段三：全量

满足以下条件再全量：

- `budget_cost_micro` 和 provider 账单趋势一致。
- `budget_degraded=true` 请求没有明显增加 4xx/5xx。
- `budget_level=exhausted` 很少出现。
- Redis 无明显超时。
- V1 路由重选验证通过。

## 14. 429 拒绝策略

`reject: true` 是降级阶梯的最后兜底：

```yaml
degrade_levels:
  - { name: warn, threshold: 0.30, model: qwen-plus }
  - { name: degrade, threshold: 0.10, model: deepseek-v3 }
  - { name: exhausted, threshold: 0.0, reject: true }
```

它和模型降级是同一个机制：

- 预算还剩 30%：降到 `qwen-plus`。
- 预算还剩 10%：降到 `deepseek-v3`。
- 预算为 0：返回 429。

默认响应：

```json
{"error":{"code":"budget_exhausted","message":"tenant AI budget exhausted"}}
```

可按业务改成中文或业务错误码：

```yaml
rejected_code: 429
rejected_message: '{"error":{"code":"budget_exhausted","message":"本租户今日 AI 预算已用尽"}}'
```

## 15. input/output 与扣费

`model_prices.input/output` 是每百万 token 的价格，不是文本长度。

```yaml
model_prices:
  gpt-4o: { input: 2.5, output: 10 }
```

含义：

| 字段 | 含义 |
|---|---|
| `input` | 每 100 万输入 token 的价格 |
| `output` | 每 100 万输出 token 的价格 |

token 数来自上游响应里的 `usage` 字段。插件在响应阶段计算：

```text
cost_micro = round(input_tokens * input_price + output_tokens * output_price)
```

例如：

| input tokens | output tokens | 单价 | cost_micro |
|---:|---:|---|---:|
| 10 | 5 | input=2.5, output=10 | 75 |

如果上游没有返回 usage，本次请求不会扣减预算。

## 16. 日志与监控

开启：

```yaml
record_attribute: true
```

Higress AI 访问日志会出现：

| 字段 | 含义 |
|---|---|
| `budget_tenant` | 租户 |
| `budget_level` | 命中的预算档 |
| `budget_remain_ratio` | 请求阶段剩余比例 |
| `budget_original_model` | 客户端原始模型 |
| `budget_actual_model` | 实际转发模型 |
| `budget_degraded` | 是否真的发生降级 |
| `budget_degrade_blocked_by` | 命中档位但未降级的原因 |
| `budget_request_bytes` | 请求体大小 |
| `budget_cost_micro` | 本次请求成本 |
| `budget_billed_model` | 计费模型 |
| `budget_rejected_reason` | 拒绝原因 |

建议监控：

- 降级率：`budget_degraded=true` / 总请求数。
- 拒绝数：`budget_level=exhausted`。
- Redis 错误：`read water level failed`、`deduct failed`。
- 降级后错误率：按 `budget_degraded=true` 切分 4xx/5xx。
- 成本趋势：Redis 已用预算 vs provider 账单。

## 17. 回滚

最快回滚方式：

```yaml
configDisable: true
```

或删除插件：

```bash
kubectl delete wasmplugin higress-budget-router -n higress-system
```

回滚后：

- 网关不再执行预算降级。
- Redis 里的预算 key 不需要手工清理，会按 TTL 过期。
- 如果要立即清空某租户预算，可删除对应 Redis key。

## 18. 排错

### 插件完全不生效

按顺序排查：

1. `priority` 是否为 `800`，且大于 `ai-token-ratelimit`、`ai-proxy` 等后续插件。
2. `phase` 是否为 `UNSPECIFIED_PHASE`。
3. `dry_run` 是否仍为 `true`。
4. 请求路径是否命中 `enable_on_path_suffix`。
5. 请求 `content-type` 是否包含 `application/json`。
6. `tenant_source` 是否能解析出租户。
7. `matchRules.ingress` 是否指向真实路由。

### 配置 apply 后未生效

查看 Higress gateway 日志中 `higress-budget-router` 相关行。配置校验失败会输出具体原因，此时 Higress 保留当前有效配置。

```bash
kubectl logs -n higress-system deploy/higress-gateway --since=10m | grep higress-budget-router
```

### 降级率是 0

| 现象 | 可能原因 |
|---|---|
| `budget_level` 一直是 `normal` | 预算没到阈值，`quota` 太大或阈值太低 |
| 命中档位但 `budget_degraded=false` | 看 `budget_degrade_blocked_by` |
| `request_too_large` 很多 | `max_request_bytes` 太小 |
| `target_not_cheaper` 很多 | 阶梯目标模型不比原模型便宜 |

### 账目对不上

| 现象 | 可能原因 |
|---|---|
| 记得比实际少 | 上游没有返回 usage，或响应压缩导致 usage 读取失败 |
| 某类接口完全没记 | 当前默认只覆盖 chat/generation JSON 路径 |
| 模型名记错 | fallback 后上游没有透传真实模型名 |
| Redis 有值但 TTL 异常 | 检查 Redis key 是否被外部系统修改 |

### 降级后请求报错

先把对应规则改回：

```yaml
dry_run: true
```

然后排查：

1. `maximum context length`：重新校准 `max_request_bytes`。
2. 不支持 tools / image / json schema：补充 `traffic_profile` 和 `model_capabilities`，或拆 route。
3. 上游 unknown model：检查 Higress 模型路由和 provider 映射。
4. 只改了 body 没进目标上游：按生产验证手册确认路由重选。

## 19. 生产检查清单

上线前逐项确认：

| 检查项 | 状态 |
|---|---|
| Redis 已注册到 McpBridge，网关可访问 | ☐ |
| Provider API Key 已在 Higress 控制台配置 | ☐ |
| 模型路由能按 `model` 进入正确 provider | ☐ |
| `matchRules.ingress` 指向真实生产路由 | ☐ |
| `priority: 800` | ☐ |
| `dry_run: true` 已观察 1 到 3 天 | ☐ |
| `dry_run: false` 已小流量验证 | ☐ |
| 预算扣减与 provider usage 基本一致 | ☐ |
| 429 拒绝响应符合业务预期 | ☐ |
| fallback 场景下 `budget_billed_model` 已验证 | ☐ |
| Redis 异常时 `fail_open` 策略符合业务要求 | ☐ |
| 回滚方式已演练 | ☐ |
