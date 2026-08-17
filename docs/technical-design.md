# Higress Budget Router 技术方案

本文描述当前版本的最终技术方案。更偏操作的内容见 [`user-manual.md`](user-manual.md) 和 [`production-verification.md`](production-verification.md)。

## 1. 插件定位

`higress-budget-router` 是 Higress AI 网关上的 Proxy-Wasm 插件，用租户预算水位决定是否把请求从高价模型主动降级到低价模型，或在预算耗尽时返回 429。

插件运行在 `UNSPECIFIED_PHASE`，建议 `priority: 800`。Higress 同一 phase 内 priority 值越大越先执行，因此该设置让插件在 `ai-token-ratelimit` 和 `ai-proxy` 之前完成预算判断与模型改写。

```
                    请求方向 ──────────────────────────────────────►
┌──────────────┐  ┌───────────────────┐  ┌────────────────┐  ┌──────────────┐
│ model-router │→ │ budget-router     │→ │ai-token-ratelim│→ │   ai-proxy   │
│  AUTHN 900   │  │ DEFAULT 800       │  │ DEFAULT 600    │  │ DEFAULT 100  │
│ 提取 model   │  │ 查预算水位改模型  │  │ 429 兜底       │  │ 协议转换/路由│
└──────────────┘  └───────────────────┘  └────────────────┘  └──────────────┘
                          ▲                                          │
                          │  解析 usage + Redis 原子扣减             │
                          └──────────────────────────────────────────┘
                    ◄────────────────────────────────── 响应方向
```

## 2. 请求链路

请求头阶段：

1. 判断路径是否命中 `enable_on_path_suffix`。
2. 判断请求体是否存在且 `content-type` 为 JSON。
3. 根据 `tenant_source` 解析租户。
4. 构造 Redis 预算键。
5. 异步读取当前已用预算和 TTL。
6. 将剩余预算比例写入请求上下文，恢复请求处理。

请求体阶段：

1. 读取原始模型字段，默认路径为 `model`。
2. 用剩余预算比例匹配 `degrade_levels`。
3. 命中 `reject: true` 时直接返回 429。
4. 命中降级档时，校验目标模型更便宜、请求体大小未超过该档 `max_request_bytes`。
5. 改写请求体里的模型字段，并同步改写 `x-higress-llm-model` 路由头。
6. 写入 `budget_*` 观测属性。

响应体阶段：

1. 通过 Higress `tokenusage` SDK 解析响应里的 usage。
2. 优先使用响应中回报的模型作为计费模型，取不到时使用请求阶段记录的实际模型。
3. 计算本次成本。
4. 使用 Redis Lua 脚本 `INCRBY` 原子累加已用预算，并在首次写入时设置 TTL。
5. 将 `budget_cost_micro`、`budget_billed_model` 等字段写入 AI 访问日志。

## 3. Redis 预算模型

Redis 键格式：

```text
{redis_key_prefix}:{<tenant>}:{rule_name}:{budget_period}
```

示例：

```text
higress-ai-budget:{tenant-a}:tenant-daily-budget:86400
```

值表示当前周期内已消耗的预算，单位为微单位：

```text
1 货币单位 = 1,000,000 微单位
```

模型价格按每百万 token 配置，所以成本可以直接整数化：

```text
cost_micro = round(input_tokens * input_price + output_tokens * output_price)
```

例如 `gpt-4o: { input: 2.5, output: 10 }`，一次请求 usage 为 `input_tokens=10`、`output_tokens=5`，则：

```text
cost_micro = round(10 * 2.5 + 5 * 10) = 75
```

## 4. 降级阶梯

`degrade_levels` 是核心策略配置。`threshold` 表示剩余预算比例，而不是已用比例。

```yaml
degrade_levels:
  - { name: exhausted, threshold: 0.00, reject: true }
  - { name: degrade,   threshold: 0.10, model: deepseek-v3 }
  - { name: warn,      threshold: 0.30, model: qwen-plus }
```

插件按 `threshold` 升序匹配，取第一条满足 `remain <= threshold` 的档位：

| 剩余预算比例 | 命中档位 | 行为 |
|---|---|---|
| `0.3001` 以上 | 无 | 维持原模型 |
| `0.30` 到 `0.10` 之间 | `warn` | 改写为 `qwen-plus` |
| `0.10` 到 `0.00` 之间 | `degrade` | 改写为 `deepseek-v3` |
| `0.00` | `exhausted` | 返回 429 |

降级保护：

- 目标模型为空时只打标，不改写。
- 目标模型与原模型相同时不改写。
- 目标模型价格不低于原模型时不改写。
- 请求体超过该档 `max_request_bytes` 时不改写。
- `dry_run: true` 时只记录决策，不改写也不拒绝。

## 5. 配置期校验

插件在配置解析阶段执行以下校验，任一失败则整份配置不生效：

- `rule_name`、Redis 服务名、`quota` 等必填项必须有效。
- `quota` 换算成微单位后必须大于 0。
- `degrade_levels[].threshold` 必须在 `[0,1]`。
- `threshold` 不能重复。
- `reject: true` 只能配置在 threshold 最小的一档。
- `max_request_bytes` 不能超过全局 `max_body_bytes`。
- 降级目标模型必须配置单价。
- 预算越紧张，目标模型应越便宜。
- 如果配置了 `traffic_profile` 和 `model_capabilities`，降级目标必须支持该路由声明的能力。

## 6. 一致性与容错

| 场景 | 行为 |
|---|---|
| 并发请求读取同一预算水位 | 都按同一水位决策，预算扣减在响应后最终一致 |
| 请求阶段 Redis 不可达 | `fail_open: true` 放行；`false` 返回拒绝响应 |
| 响应阶段 Redis 扣减失败 | 记录错误日志，不影响客户端响应 |
| 上游没有返回 usage | 不扣减预算 |
| 上游 fallback 到其他模型 | 优先按响应里的模型计费 |
| 非 JSON 请求体 | 不改写，不进入当前预算链路 |

插件不做请求阶段预扣。原因是输出 token 无法准确预估，预扣会造成预算虚占或低估。当前方案采用响应后精确扣减，代价是预算判断会有一个请求的滞后。

## 7. 可观测性

插件写入 Higress AI 访问日志的主要字段：

| 字段 | 含义 |
|---|---|
| `budget_tenant` | 租户标识 |
| `budget_level` | 命中的预算档位 |
| `budget_remain_ratio` | 决策时剩余预算比例 |
| `budget_original_model` | 客户端请求模型 |
| `budget_actual_model` | 实际转发模型 |
| `budget_degraded` | 是否发生降级 |
| `budget_degrade_blocked_by` | 命中降级档但未改写的原因 |
| `budget_request_bytes` | 请求体大小 |
| `budget_cost_micro` | 本次请求成本 |
| `budget_billed_model` | 本次计费用的模型 |

建议看板：

- 降级率：`budget_degraded=true` / 总请求数。
- 档位分布：按 `budget_level` 观察阈值是否合理。
- 拒绝数：`budget_level=exhausted` 或 `budget_rejected_reason=budget_exhausted`。
- 成本节省：比较 `budget_original_model` 与 `budget_actual_model`。

## 8. 当前边界

当前版本适合 chat/generation 类 JSON 请求的预算感知降级。以下场景需要独立验证或拆分路由策略：

- embeddings、rerank、moderations 等非 chat 类接口。
- multipart、audio 上传等非 JSON 请求体。
- 上游不返回 usage 的模型或供应商。
- 响应压缩、fallback、不同模型跨上游路由等网关行为。

生产前验证清单见 [`known-issues.md`](known-issues.md) 和 [`production-verification.md`](production-verification.md)。
