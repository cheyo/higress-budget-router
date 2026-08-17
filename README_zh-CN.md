# higress-budget-router

[![CI](https://github.com/OWNER/higress-budget-router/actions/workflows/ci.yml/badge.svg)](https://github.com/OWNER/higress-budget-router/actions/workflows/ci.yml)
[![License](https://img.shields.io/badge/license-Apache%202.0-blue.svg)](LICENSE)
[![Go](https://img.shields.io/badge/go-1.24-00ADD8.svg)](go.mod)

**[Higress](https://higress.cn) AI 网关的预算感知降级插件 —— 在请求发出之前主动切到更便宜的模型，而不是等钱花完了再用 429 把用户挡在门外。**

[English](README.md) · **[用户手册](docs/user-manual.md)** · [生产验证](docs/production-verification.md) · [技术方案](docs/technical-design.md)

---

## 它填补的空白

Higress 原生 AI 插件很完整，但没有一个是按**预算状态**做路由的：

| 层 | 插件 | 触发时机 | 决策依据 |
|---|---|---|---|
| ① 主动降级 | **higress-budget-router** | 请求发出**前** | **租户实时预算水位（动态）** |
| ② 协议路由 | `ai-proxy` | 请求发出前 | 模型名 → provider（静态） |
| ③ 兜底拦截 | `ai-token-ratelimit` | 请求发出前 | 累计 Token（事后统计） |
| ④ 故障切换 | `ai-proxy` fallback | 上游 4xx/5xx **之后** | 上游失败信号（被动） |

原生 Token 限流只能**事后统计、事后拦截**：预算一旦耗尽，用户拿到的是硬 429，业务直接中断。本插件补上缺的那一层——预算见底的过程中，请求被透明地移到更便宜的模型上，是软着陆而不是撞墙。

```
                    请求方向 ──────────────────────────────────────►
┌──────────────┐  ┌───────────────────┐  ┌────────────────┐  ┌──────────────┐
│ model-router │→ │  budget-router    │→ │ai-token-ratelim│→ │   ai-proxy   │
│  AUTHN 900   │  │   DEFAULT 800     │  │  DEFAULT 600   │  │  DEFAULT 100 │
│ 提取 model   │  │ ① 查水位改 model  │  │ ③ 429 兜底     │  │ ② 协议转换   │
│ 写路由头     │  │                   │  │                │  │ ④ 故障切换   │
└──────────────┘  └───────────────────┘  └────────────────┘  └──────────────┘
                          ▲                                          │
                          │  ⑤ SSE usage 解析 + Redis 原子扣减       │
                          └──────────────────────────────────────────┘
                    ◄────────────────────────────────── 响应方向
```

## ⚠️ 部署前必读：priority 的方向

Higress 中**同 phase 内 priority 值越大越先执行**。这个方向搞反是让插件「静默不生效」的头号原因——插件能正常加载、日志看不出异常，只是 `ai-proxy` 早就把请求转发出去了。

```yaml
spec:
  phase: UNSPECIFIED_PHASE
  priority: 800          # 必须 > ai-quota(750) > ai-token-ratelimit(600) > ai-proxy(100)
```

选 `DEFAULT` 而不是 `AUTHN` 阶段是刻意的：让 AUTHN 阶段的认证插件（`key-auth`、`jwt-auth`）先跑完，本插件才能读到 `x-mse-consumer` 作为租户标识。

## 快速开始

```bash
# 1. 构建
make build                    # go test + vet + wasm 编译

# 2. 推 OCI 镜像
make push REGISTRY=ghcr.io/OWNER VERSION=0.1.0

# 3. 部署
kubectl apply -f examples/basic.yaml
```

最小配置——按 consumer 维度的日预算 200 单位，两级降级，最后拒绝：

```yaml
rule_name: tenant-daily-budget
redis:
  service_name: redis.dns
  service_port: 6379
tenant_source:
  type: consumer                # 读 x-mse-consumer
budget_period: 86400
quota: 200
degrade_levels:
  - { name: warn,      threshold: 0.30, model: qwen-plus }
  - { name: degrade,   threshold: 0.10, model: deepseek-v3 }
  - { name: exhausted, threshold: 0.0,  reject: true }
model_prices:                   # 每【百万】Token 的单价
  gpt-4o:      { input: 2.5,  output: 10 }
  qwen-plus:   { input: 0.8,  output: 2 }
  deepseek-v3: { input: 0.14, output: 0.28 }
dry_run: true                   # 从这里起步：只打标和记日志，不真改写
```

更多场景见 [`examples/`](examples/)。

## 用户必须理解的策略模型

`degrade_levels` 是插件最核心的用户配置：它同时表达“什么时候降级”和“什么时候拒绝”。`threshold` 是**剩余预算比例**，不是已使用比例；命中带 `model` 的档位时插件会改写请求模型，命中 `reject: true` 的档位时插件会直接返回 429。

预算按 `tenant + rule_name + budget_period` 统计，默认不是按模型单独统计。`model_prices.input/output` 是每百万输入 / 输出 token 的单价，真实 token 数来自上游响应里的 `usage` 字段。完整说明见 [用户手册](docs/user-manual.md)。

## 工作原理

**请求头阶段** —— 解析租户、构造 Redis 键、异步发一条只读 `EVAL` 查水位，用 `HeaderStopAllIterationAndWatermark` 把请求挂起，等回调唤醒。

**请求体阶段** —— 用剩余预算比例匹配降级阶梯，`sjson` 改写 `body.model`，同步改写 `x-higress-llm-model` 路由头保持一致。

**响应体阶段（流式）** —— 逐块调 SDK 的 `tokenusage.GetTokenUsage` 累积 usage（它已经处理了 SSE 分片以及 OpenAI / Anthropic / Gemini / 豆包的字段差异），在最后一块折算成本并原子 `INCRBY` 进 Redis。

### 刻意不做请求阶段预扣

请求阶段只读不写。预扣需要预估输出 Token，而同一个 prompt 的输出长度方差极大——估高则预算被虚占、用户提前被降级；估低则形同虚设。事后精确扣减的代价只是一个请求的滞后，换来的是准确的账。

### 「微单位」整数记账

单价按**每百万 Token** 配置，分母天然抵消，成本直接落在整数上：

```
inputTokens/1e6 × price_in 货币单位  ==  inputTokens × price_in 微单位
∴ cost_micro = round(in × price_in + out × price_out)
```

这就是整条链路能用整数 `INCRBY` 而不是会漂移的 `INCRBYFLOAT` 的原因。1 货币单位 = 1e6 微单位。

## 配置项

| 字段 | 类型 | 默认值 | 说明 |
|---|---|---|---|
| `rule_name` | string | **必填** | 参与 Redis key 构造 |
| `redis.service_name` | string | **必填** | McpBridge 注册的服务名 |
| `redis.service_port` | int | 6379（`.static` 为 80） | |
| `redis.timeout` | int | 1000 | 毫秒 |
| `redis.username` / `password` / `database` | | | |
| `redis_key_prefix` | string | `higress-ai-budget` | |
| `tenant_source.type` | enum | `consumer` | `consumer`/`header`/`param`/`cookie`/`fixed` |
| `tenant_source.key` | string | `x-mse-consumer` | 非 `consumer` 类型必填 |
| `budget_period` | int | 86400 | 秒；靠 Redis TTL 滚动重置 |
| `quota` | float | **必填** | 周期总预算（货币单位） |
| `degrade_levels[].name` | string | `level-N` | 出现在日志属性里 |
| `degrade_levels[].threshold` | float | **必填** | **剩余**比例，[0,1] |
| `degrade_levels[].model` | string | | 目标模型；空 = 只打标 |
| `degrade_levels[].reject` | bool | false | 直接返回 `rejected_code`。必须是 threshold 最小的一档 |
| `degrade_levels[].max_request_bytes` | int | 0（不限） | 请求体超过此大小则不降级。**用 dry_run 数据校准**，见[用户手册 §11](docs/user-manual.md#11-校准-max_request_bytes) |
| `traffic_profile` | []string | - | 声明本 route 会用到的能力：`tools`/`vision`/`audio`/`json_schema` |
| `model_capabilities` | object | - | 各模型支持哪些能力。填了就在**配置生效时**校验降级目标，零运行时开销 |
| `model_prices.<model>.input/output` | float | | 每**百万** Token 单价 |
| `default_price.input/output` | float | 1 / 1 | 未知模型兜底 |
| `model_key` | string | `model` | 模型字段的 gjson 路径 |
| `model_to_header` | string | `x-higress-llm-model` | 空串表示不改路由头 |
| `enable_on_path_suffix` | []string | `/completions` `/messages` `/responses` `/generateContent` | |
| `rejected_code` | int | 429 | |
| `rejected_message` | string | JSON 错误体 | |
| `dry_run` | bool | false | 走完决策但不改写 |
| `fail_open` | bool | true | Redis 不可达时放行 |
| `record_attribute` | bool | true | 写 AI 访问日志属性 |
| `max_body_bytes` | int | 10485760 | 请求体缓冲上限 |

**阶梯匹配规则**：档位按 `threshold` **升序**排列，取第一条满足 `剩余比例 <= threshold` 的——也就是最严格的那档。阶梯 `[0.00, 0.10, 0.30]` 下，剩余 `0.05` 命中 `0.10` 档而不是 `0.30` 档。

## 可观测性

写入 Higress AI 访问日志（`wrapper.AILogKey`）：

`budget_tenant` · `budget_level` · `budget_remain_ratio` · `budget_original_model` · `budget_actual_model` · `budget_degraded` · `budget_degrade_blocked_by` · `budget_request_bytes` · `budget_cost_micro` · `budget_billed_model`

值得建的看板：

- **降级率** = `budget_degraded=true` / 总数，按租户切分
- **档位分布随时间变化** —— 判断阈值是不是设激进了
- **`exhausted` 计数** —— 理想应长期为 0，非 0 说明阶梯没兜住
- **原始模型 vs 实际模型的成本差** —— 量化插件省了多少钱

## 上线路径

1. `dry_run: true` + 单条 ingress，观察 1–3 天档位分布
2. 切 `dry_run: false`，先对低优先级租户开；拿 `budget_cost_micro` 累计值和厂商账单对一遍
3. 全量后**上调** `ai-token-ratelimit` 阈值，让它退回真正的「极限兜底」角色，而不是日常拦截器

回滚就是 `configDisable: true`，秒级生效，Redis 计数键随 TTL 自然过期。

## 一致性模型

刻意做成最终一致：

| 场景 | 行为 | 理由 |
|---|---|---|
| 并发请求读到同一水位 | 都按同一档决策，可能短暂超支一个批次 | 降级是软决策；强一致要预扣+回滚，代价远大于收益 |
| Redis 不可达（请求阶段） | `fail_open: true` → 放行；`false` → 拒绝 | 默认可用性优先 |
| Redis 扣减失败（响应阶段） | 记 error 日志，不影响响应 | 响应已经在回客户端路上 |
| 上游未返回 usage | 跳过扣减 | 宁可漏记，不可乱记 |
| `ai-proxy` Fallback 到了别的模型 | 按响应里回报的真实模型计费 | 计费必须跟随实际执行的模型 |

## 环境要求

- Higress ≥ 2.0（AI 网关插件、`wasm-go` SDK）
- Go 1.24+（`GOOS=wasip1 GOARCH=wasm`）
- 网关可达的 Redis，且已通过 `McpBridge` 注册

## 状态与当前限制

当前版本适合本地和受控环境验证。生产使用前，请按照 [`docs/production-verification.md`](docs/production-verification.md) 在你的 Higress 版本和目标路由配置上完成验证。

当前限制：

- 默认覆盖 chat/generation JSON 路径：`/completions`、`/messages`、`/responses`、`/generateContent`。
- 非 JSON、multipart、音频上传、embeddings、rerank、moderations 请求默认不计费。
- 计费依赖上游响应里的 `usage` 字段。
- 请求体阶段改写模型和路由头后，是否能进入目标上游，需要在目标 Higress 环境中验证。
- 能力兼容性通过 `traffic_profile` 和 `model_capabilities` 在配置期校验；插件运行时不检查请求内容。

当前限制清单见 [`docs/known-issues.md`](docs/known-issues.md)。

## License

[Apache 2.0](LICENSE)
