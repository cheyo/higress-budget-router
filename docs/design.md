# Higress 预算感知降级路由插件（ai-budget-router）技术方案 v0.1

- 文档版本：v0.1
- 日期：2026-08-15
- 插件名：`ai-budget-router`
- 语言/SDK：Go 1.24 + `higress-group/wasm-go` + `higress-group/proxy-wasm-go-sdk`（GOOS=wasip1）

---

## 0. 插件执行位置：phase 与 priority

Higress 的插件链排序规则，官方文档原文：**「在相同 phase 情况下，priority 值越大，插件在插件链位置越靠前」**。即 **priority 越大越先执行**。

对照现网内置插件的实际取值（取自 Higress 主仓各插件 README 的「插件执行优先级」）：

| 插件 | phase | priority | 职责 |
|---|---|---|---|
| `model-router` | AUTHN | 900 | 从 body 提取 model，写路由头 `x-higress-llm-model` |
| **`ai-budget-router`（本插件）** | **DEFAULT** | **800** | **预算水位驱动的主动降级** |
| `ai-quota` | DEFAULT | 750 | 配额校验 |
| `ai-token-ratelimit` | DEFAULT | 600 | Token 限流，429 兜底 |
| `ai-statistics` | DEFAULT | 250 | 统计埋点 |
| `ai-proxy` | DEFAULT | 100 | 协议转换 + 上游 Fallback |

本插件的位置由两条硬约束决定：改写必须早于 `ai-proxy`（100），因为请求一旦进入 `ai-proxy` 就已完成协议转换并转发，此时改 `model` 不再生效；也必须早于 `ai-token-ratelimit`（600），否则 429 会抢在主动降级之前返回，降级形同虚设。

**结论：本插件配 `phase: UNSPECIFIED_PHASE`（默认阶段）+ `priority: 800`。**

选 800 而不是更大值的三点理由：

1. 大于 750/600/100，保证在 `ai-quota`、`ai-token-ratelimit`、`ai-proxy` 之前完成改写；
2. 落在 DEFAULT 阶段而非 AUTHN，是为了让 AUTHN 阶段的认证插件（`key-auth` / `jwt-auth` / `oauth`）先跑完，本插件才能直接读到 `x-mse-consumer` 作为租户标识；
3. `model-router`（AUTHN 900）也已执行完毕，路由头已经存在，本插件改写 model 时**同步覆盖该头**即可，语义清晰。

### 0.1 响应阶段的执行顺序

Envoy filter chain 里 decoder（请求）按链序执行，encoder（响应）**逆序**执行。所以：

- 请求阶段：`ai-budget-router`(800) → `ai-quota`(750) → `ai-token-ratelimit`(600) → `ai-proxy`(100)
- 响应阶段：`ai-proxy`(100) → `ai-token-ratelimit`(600) → `ai-quota`(750) → `ai-budget-router`(800)

这对本插件是**有利**的：响应阶段本插件排在最后，看到的是 `ai-proxy` 已经归一化成 OpenAI 协议的响应体，SSE usage 解析可以统一走一套逻辑，不必为每家厂商写适配。

---

## 1. 四层防护

在上述执行顺序下，四层防护是这样的：

```
                    请求方向 ──────────────────────────────────────►
┌──────────────┐  ┌───────────────────┐  ┌────────────────┐  ┌──────────────┐
│ model-router │→ │ ai-budget-router  │→ │ai-token-ratelim│→ │   ai-proxy   │
│  AUTHN 900   │  │   DEFAULT 800     │  │  DEFAULT 600   │  │  DEFAULT 100 │
│              │  │                   │  │                │  │              │
│ 提取 model   │  │ ① 主动降级        │  │ ③ 兜底拦截 429 │  │ ② 协议路由   │
│ 写路由头     │  │  查水位→改 model  │  │   （读预算）   │  │ ④ 故障 Fallback│
└──────────────┘  └───────────────────┘  └────────────────┘  └──────────────┘
                          ▲                                          │
                          │  ⑤ SSE usage 解析 + Redis 原子扣减       │
                          └──────────────────────────────────────────┘
                    ◄────────────────────────────────── 响应方向
```

| 层 | 触发时机 | 决策依据 | 失败后果 |
|---|---|---|---|
| ① 主动降级（本插件） | 请求发出**前** | 租户实时预算水位（动态） | 无——降级是软着陆 |
| ② 协议路由（ai-proxy） | 请求发出前 | 模型名 → provider 映射（静态） | 转发失败 |
| ③ 兜底拦截（ai-token-ratelimit） | 请求发出前 | 上一周期累计 Token（事后统计） | 业务中断 429 |
| ④ 故障 Fallback（ai-proxy） | 上游返回 4xx/5xx/超时**后** | 上游失败信号（被动） | 重试延迟 |

本插件填补的空白，就是**唯一一层「在请求发出前、基于动态预算状态、主动做出可用性不降级的成本决策」**。其余三层要么只看静态属性，要么只在事后动作。

---

## 2. 数据流与时序

```
Client
  │ POST /v1/chat/completions  {"model":"gpt-4o", ...}
  ▼
[AUTHN] key-auth ──► x-mse-consumer: acme-corp
[AUTHN] model-router ──► x-higress-llm-model: gpt-4o
  │
  ▼
[DEFAULT 800] ai-budget-router
  │
  ├─ onHttpRequestHeaders
  │    1. path 后缀命中？content-type 是 JSON？否 → DontReadRequestBody + 放行
  │    2. 解析租户 → budgetKey = "higress-ai-budget:{acme-corp}:tenant-daily-budget:86400"
  │    3. RemoveHttpRequestHeader("content-length") + SetRequestBodyBufferLimit
  │    4. Redis EVAL ReadWaterLevelScript（异步）
  │       return HeaderStopAllIterationAndWatermark   ← 挂起请求
  │    5. 回调：used=170_000_000, quota=200_000_000 → remain = 0.15
  │       ctx.SetContext(remain); ResumeHttpRequest()
  │
  ├─ onHttpRequestBody
  │    6. MatchLevel(0.15)：升序档位表 [0.00, 0.10, 0.30]，
  │       取第一条满足 0.15 <= threshold 的 → 0.30 档 "warn" → qwen-plus
  │    7. sjson 改写 body.model = "qwen-plus"
  │       ReplaceHttpRequestBody + ReplaceHttpRequestHeader("x-higress-llm-model","qwen-plus")
  │    8. 记录 UserAttribute（原模型/实际模型/水位/档位）
  ▼
[DEFAULT 750/600] ai-quota / ai-token-ratelimit  ← 看到的是已降级后的请求
  ▼
[DEFAULT 100] ai-proxy ──► 按 qwen-plus 做协议转换，转发上游
  ▼
上游 LLM ──► SSE 流
  ▼
[DEFAULT 100] ai-proxy 归一化成 OpenAI SSE
  ▼
[DEFAULT 800] ai-budget-router.onHttpStreamingResponseBody
      9. tokenusage.GetTokenUsage(ctx, chunk) 逐块累积（SDK 已处理 SSE 分片/合并）
     10. endOfStream：cost_micro = round(in*price_in + out*price_out)
     11. Redis EVAL DeductScript（INCRBY + 首次 EXPIRE）
     12. 写 AI 访问日志属性
  ▼
Client
```

---

## 3. Redis 数据结构

### 3.1 键设计

```
{redis_key_prefix}:{<tenant>}:{rule_name}:{budget_period}
例：higress-ai-budget:{acme-corp}:tenant-daily-budget:86400
```

- 租户名外层加 `{}` **hash tag**，保证 Redis Cluster 下同租户的所有键落在同一 slot，Lua 脚本可安全多键操作；
- 值：**已消耗的微单位整数**（`INCRBY` 累加）；
- TTL：首次写入时设为 `budget_period`，到期自动删除 → 预算天然按周期滚动重置，不需要定时任务。

### 3.2 计量单位：为什么用「微单位整数」

浮点数在 Redis 里累加（`INCRBYFLOAT`）会有精度漂移，且 Lua 的数值比较不可靠。方案统一约定：

```
1 货币单位 = 1_000_000 微单位
单价配置 = 每【百万】Token 的货币单位数
∴ cost_micro = round(inputTokens × price_in + outputTokens × price_out)
```

推导：`inputTokens / 1e6 × price_in` 货币单位 `= inputTokens × price_in` 微单位。分母天然抵消，**不需要任何缩放，结果直接就是整数微单位**。

举例（gpt-4o，input 2.5 / output 10 元每百万 Token）：

| 输入 Token | 输出 Token | cost_micro | 折合 |
|---|---|---|---|
| 1000 | 500 | `1000×2.5 + 500×10 = 7500` | 0.0075 元 |
| 200000 | 50000 | `500000 + 500000 = 1_000_000` | 1.00 元 |

`quota: 200`（元）→ `QuotaMicro = 200_000_000`。全链路整数，无累积误差。

### 3.3 Lua 脚本

**请求阶段（只读）**：

```lua
local used = tonumber(redis.call('get', KEYS[1]) or "0")
local ttl = redis.call('ttl', KEYS[1])
if ttl < 0 then ttl = tonumber(ARGV[1]) end
return {used, ttl}
```

只读不写，是刻意的：**请求阶段不做任何预扣**。预扣需要预估 Token 数，估不准会导致预算被虚占；而降级本身是软决策，多放行几个请求不会造成硬故障。

**响应阶段（原子扣减）**：

```lua
local added = tonumber(ARGV[1])
local window = tonumber(ARGV[2])
local current = redis.call('incrby', KEYS[1], added)
if current == added then
    redis.call('expire', KEYS[1], window)
else
    local ttl = redis.call('ttl', KEYS[1])
    if ttl < 0 then redis.call('expire', KEYS[1], window) end
end
return current
```

`current == added` 判断「本次是不是第一次写入」，只在首写时设 TTL；`ttl < 0` 分支修复异常丢失 TTL 的键，避免预算永不重置。

---

## 4. 降级策略语义

配置是一张阶梯表，`threshold` 是**剩余预算比例**：

```yaml
degrade_levels:
  - { name: warn,      threshold: 0.30, model: qwen-plus }
  - { name: degrade,   threshold: 0.10, model: deepseek-v3 }
  - { name: exhausted, threshold: 0.00, reject: true }
```

**匹配规则：按 threshold 升序排列，取第一条满足 `remain <= threshold` 的档。**

这里踩过一个坑：最初实现按降序排列取第一条满足的，结果 `remain = 0.05` 会命中 `warn`(0.30) 而不是 `degrade`(0.10)——降级力度反而随水位下降而变弱。单元测试 `TestMatchLevel` 直接把这个 bug 打了出来。改成升序后：

| 剩余比例 | 命中档 | 行为 |
|---|---|---|
| 1.00 / 0.55 / 0.3001 | — | 维持原模型 |
| 0.30 / 0.20 | warn | → qwen-plus |
| 0.10 / 0.05 | degrade | → deepseek-v3 |
| 0.00 | exhausted | 429 拒绝 |

### 4.1 三条防误伤保护

1. **同模型不改写**：目标模型 == 当前模型时直接放行，不做无意义的 body 重写；
2. **不降级到更贵的模型**：`isCheaper()` 用「输入+输出单价之和」比较；两边都在 `model_prices` 里配了单价才做此判断，任一未配则遵从运维的显式配置意图；
3. **`dry_run: true`**：完整走一遍决策与打标，但不改写 body/header。灰度期先开这个，观察 `budget_degraded` 属性在日志里的分布，确认降级率符合预期再切真。

---

## 5. 一致性与容错的取舍

这套设计是**最终一致**的，不是强一致。明确列出取舍：

| 场景 | 行为 | 理由 |
|---|---|---|
| 并发请求读到同一水位 | 都按同一档决策，可能短暂超支一个批次 | 降级是软决策；强一致要预扣+回滚，代价远大于收益 |
| Redis 超时/不可达（请求阶段） | `fail_open: true` → 放行不降级；`false` → 按 `rejected_code` 拒绝 | 默认可用性优先；成本敏感场景可切 fail-closed |
| Redis 扣减失败（响应阶段） | 记 error 日志，不影响响应 | 响应已经在回客户端路上，不能因记账失败而中断 |
| 上游未返回 usage | 跳过扣减并 debug 日志 | 宁可漏记，不可乱记 |
| 请求被 `ai-proxy` Fallback 到备用模型 | 按响应体里的真实 model 计费（`tokenusage` 提取），取不到才回落到改写后的模型 | 计费必须跟随实际执行的模型 |
| 非 JSON body（multipart 音频等） | 跳过改写，响应阶段仍正常扣减 | 无法安全改写二进制/分段体 |

**为什么不在请求阶段预扣**：预扣需要估算 output token，AI 请求的输出长度方差极大（同一个 prompt 可能 50 token 也可能 4000 token）。预扣估高则预算被虚占、用户提前被降级；估低则形同虚设。相比之下「事后精确扣减 + 下一次请求生效」的一个请求延迟，是可接受的代价。

---

## 6. 性能影响

| 项 | 开销 |
|---|---|
| 请求阶段 Redis EVAL | 1 次 RTT，同机房 Redis 典型 0.3–1 ms |
| 请求体缓冲 | 需要完整缓冲请求体才能改写（`SetRequestBodyBufferLimit`，默认 10 MB） |
| 响应阶段 | 流式逐块解析，`tokenusage` 只在 chunk 含 `"usage"` 时才做 gjson 提取；扣减在 `endOfStream` 单次异步发出，不阻塞响应 |
| 内存 | 每请求 ctx 存 6 个小对象；wasm VM 按 worker 线程隔离 |

对 LLM 请求（首 token 延迟通常 300 ms – 数秒）而言，1 ms 量级的 Redis 往返可以忽略。

若后续压测发现 Redis 成为瓶颈，优化路径（本版本未实现，留作 v0.2）：wasm VM 内做 200–500 ms 的水位本地缓存 + `RegisterTickFunc` 异步刷新。代价是水位滞后一个刷新周期，需要评估是否可接受。

---

## 7. 可观测性

插件写入的 AI 访问日志属性（`wrapper.AILogKey`）：

| 属性 | 含义 |
|---|---|
| `budget_tenant` | 租户标识 |
| `budget_level` | 命中的档位名（`normal` / `warn` / `degrade` / `exhausted`） |
| `budget_remain_ratio` | 决策时的剩余预算比例 |
| `budget_original_model` | 客户端原始请求的模型 |
| `budget_actual_model` | 实际转发的模型 |
| `budget_degraded` | 是否发生了改写 |
| `budget_cost_micro` | 本次请求消耗（微单位） |
| `budget_billed_mode` | 计费所用的模型名 |

建议的核心看板指标：

- **降级率** = `budget_degraded=true` 请求数 / 总请求数，按租户切分；
- **档位分布**随时间的变化，用于校准 `threshold` 是否设得过激；
- **`exhausted` 计数**——理想情况应长期为 0，非 0 说明降级阶梯没能兜住，需要加档或调低阈值；
- **原始模型 vs 实际模型的成本差**，量化插件省下了多少钱。

---

## 8. 配置项清单

| 字段 | 类型 | 默认 | 说明 |
|---|---|---|---|
| `rule_name` | string | 必填 | 规则名，参与 Redis key 构造 |
| `redis.service_name` | string | 必填 | McpBridge 注册的服务名 |
| `redis.service_port` | int | 6379 / 80(.static) | |
| `redis.timeout` | int | 1000 | ms |
| `redis.username/password/database` | | | |
| `redis_key_prefix` | string | `higress-ai-budget` | |
| `tenant_source.type` | enum | `consumer` | `consumer`/`header`/`param`/`cookie`/`fixed` |
| `tenant_source.key` | string | `x-mse-consumer` | 非 consumer 类型必填 |
| `budget_period` | int | 86400 | 秒 |
| `quota` | float | 必填 | 周期总预算（货币单位） |
| `degrade_levels[].name` | string | `level-N` | |
| `degrade_levels[].threshold` | float | 必填 | [0,1] 剩余比例 |
| `degrade_levels[].model` | string | | 目标模型；空则只打标 |
| `degrade_levels[].reject` | bool | false | 该档直接拒绝 |
| `model_prices.<model>.input/output` | float | | 每百万 Token 单价 |
| `default_price.input/output` | float | 1 / 1 | 未知模型的兜底单价 |
| `model_key` | string | `model` | body 中模型字段的 gjson 路径 |
| `model_to_header` | string | `x-higress-llm-model` | 空串表示不改路由头 |
| `enable_on_path_suffix` | []string | `/completions,/messages,/responses,/generateContent` | |
| `rejected_code` | int | 429 | |
| `rejected_message` | string | JSON 错误体 | |
| `dry_run` | bool | false | 灰度开关 |
| `fail_open` | bool | true | Redis 异常时放行 |
| `record_attribute` | bool | true | 写日志属性 |
| `max_body_bytes` | int | 10485760 | 请求体缓冲上限 |

---

## 9. 上线路径

1. **本地**：`make build`（`go test ./...` + `GOOS=wasip1 go vet` + wasm 编译）
2. **镜像**：`make push`，产物是 `scratch` 镜像，根路径 `plugin.wasm`
3. **灰度一**：`dry_run: true` + 单条 ingress 生效，观察 1–3 天的档位分布与降级率
4. **灰度二**：`dry_run: false`，只对内部/低优先级租户开；核对账单与 `budget_cost_micro` 累计值的偏差
5. **全量**：扩到全部 ingress；同时把 `ai-token-ratelimit` 的阈值上调，让它退回真正的「极限兜底」角色，而不是日常拦截器

**回滚**：`configDisable: true` 即可秒级摘除，Redis 里的计数键会随 TTL 自然过期，无残留。

---

## 10. 待验证的技术点

以下几点建议在联调环境实测确认，不要直接按文档假设上生产：

1. **请求体阶段改写路由头能否触发 Envoy 重新选路**。`model-router` 采用的正是「body 阶段改 `x-higress-llm-model`」这一模式，说明 Higress 侧对此有支持；但路由缓存的清除时机需要在你的 Higress 版本上实测——尤其当不同模型走**不同 Route/上游服务**时。若实测不重新选路，退路是把降级决策提到 AUTHN 阶段（priority > 900）在 header 阶段完成，代价是拿不到 `x-mse-consumer`，租户维度需改用自定义 header。
2. **本插件（800）与 `ai-token-ratelimit`（600）的预算口径是否要打通**。当前两者各记各的账。若希望 429 兜底也基于同一份水位，需要让两者共享 Redis key 格式，或干脆关掉 `ai-token-ratelimit`、由本插件的 `exhausted` 档承担拒绝职责（推荐后者，链路更简单）。
3. **`ai-proxy` Fallback 触发后的计费归属**。当前实现优先信任响应体里的 model 字段，需实测确认 Fallback 场景下 `ai-proxy` 是否会把真实模型名透传出来。
4. **多 worker 线程下的水位一致性**。Redis 是唯一真相源，wasm VM 无本地状态，理论上无问题；压测时确认一下 QPS 上去后 Redis 连接池是否够用。

---

## 附：源码结构

```
higress-budget-router/
├── main.go                 插件主流程（三个 Hook + Lua 脚本 + helpers）
├── config/
│   ├── config.go           配置解析、降级档匹配、单价查询
│   └── config_test.go      6 组单测（含把降序 bug 打出来的 TestMatchLevel）
├── util/
│   ├── util.go             Cookie 提取、路径后缀匹配
│   └── util_test.go
├── examples/               三份场景化 WasmPlugin CR
│   ├── basic.yaml          按 consumer 分租户的最简配置
│   ├── dry-run.yaml        灰度观察（建议首次上线用这份）
│   └── multi-tenant-header.yaml  按 header 分租户 + 多套策略
├── docs/
│   ├── design.md           本文档
│   ├── code-walkthrough.md main.go 逐行解读
│   └── known-issues.md     已知问题清单
├── .github/workflows/      CI（test/vet/wasm）+ Release（推 OCI 镜像）
├── Dockerfile              scratch 镜像，根路径 plugin.wasm
├── Makefile                build / test / vet / image / push
└── go.mod                  module higress-budget-router
                            wasm-go v1.0.7-xxx, proxy-wasm-go-sdk 20251103
```
