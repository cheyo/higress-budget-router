# Known Issues — v0.1

逐行核对 `main.go` 时发现的问题清单。按严重程度排序。行号对应 v0.1 的 `main.go`。

建议开源后把 §1 和 §4 转成 GitHub Issue 跟踪。

---

## 1. 【功能缺陷】非 JSON 请求体完全不计费

**位置**：`main.go` L108–L113 与 L273–L276

L110-111 的代码注释和 `design.md` §5 都写着「非 JSON body → 跳过改写，**响应阶段仍正常扣减**」。实际做不到：

1. L112 走 `skip()` 返回时，`budgetKey` 还没有被计算（它在 L122，在这个 return 之后）
2. 响应阶段 L273 `GetStringContext(ctxKeyBudgetKey, "")` 拿到空串 → L274 直接 return，**不扣减**

**影响**：走 multipart 的接口（`/audio/transcriptions`、文件上传等）消耗的 Token 完全不计入预算。这类流量占比高时，水位会系统性偏低，降级永远不触发——插件看起来在跑，实际形同虚设。

**根因**：`ctxKeySkip` 这一个标志把「不改写请求」和「不参与计费」两件不同的事混在了一起。

**修复方向**：
- 把租户解析和 `budgetKey` 构造（L115–L124）提到 content-type 检查之前
- 引入独立的 `ctxKeyNoRewrite` 标志，与 `ctxKeySkip` 区分开
- 语义变成：`skip` = 这个请求与本插件完全无关（路径不匹配、无 body、租户取不到）；`noRewrite` = 参与计费但不改写

---

## 2. 【观测盲区】被拒绝的请求可能丢日志属性

**位置**：`main.go` L204 / L209 与 L313–L319

`record()` 只把属性挂到 ctx，真正落盘靠响应体阶段的 L318 `WriteUserAttributeToLogWithKey`。但 L209 `sendRejected` 之后请求终止，响应体阶段不会执行。

**影响**：`exhausted` 档的请求——恰恰是最需要被看到的那批——在日志里可能缺字段，导致「预算耗尽」这个关键事件无法在看板上统计。

**修复方向**：在 `sendRejected` 之前显式调一次 `ctx.WriteUserAttributeToLogWithKey(wrapper.AILogKey)`。同样适用于 L160 的 fail-closed 拒绝路径。

---

## 3. 【死代码】3 个 ctx key + 2 个响应头常量

| 符号 | 位置 | 状态 |
|---|---|---|
| `ctxKeyUsedMicro` | L53，写于 L151 | 无人读取 |
| `ctxKeyOriginalModel` | L55，写于 L192 | 无人读取 |
| `ctxKeyDegraded` | L57，写于 L381 | 无人读取 |
| `HeaderBudgetRemaining` | L61 | 完全未使用 |
| `HeaderModelDegraded` | L63 | 完全未使用 |

Go 不会对未使用的**常量**报错（不同于变量和 import），所以这些一路编译通过。

后两个反映的是一个**没做完的设计**：原本要把降级信息通过响应头回传给客户端，让 SDK 能感知「我请求的是 gpt-4o，实际跑的是 qwen-plus」，目前只实现了拒绝场景（L398）。

**修复方向**：二选一——
- 补完：加一个 `onHttpResponseHeaders` Hook，把 `x-higress-budget-remaining` 和 `x-higress-model-degraded` 写进响应头
- 删掉：留着会让人以为功能已经有了

---

## 4. 【边界】quota 极小时除零产生 NaN

**位置**：`main.go` L148，`config/config.go` 的 quota 校验

配置校验只拦了 `quota <= 0`，但 `quota: 0.0000001` 会让 `QuotaMicro = int64(0.1) = 0`，L148 的除法产生 NaN 或 Inf。

L149 的 `math.Max/Min` 对 NaN 无效（`math.Min(1, NaN)` 返回 NaN），NaN 传到 `MatchLevel` 后所有 `<=` 比较都是 false → 不降级。

**影响**：最终是 fail-open，不会造成事故，但属于靠运气兜住而不是靠设计。

**修复方向**：在 `config.Parse` 里把校验从 `quota <= 0` 改成同时校验 `QuotaMicro <= 0`。

---

## 5. 【冗余】L200 重复设置 level name

`ctx.SetContext(ctxKeyLevelName, level.Name)` 在 L200 设一次，紧接着 `record()` 内部 L380 又设一次。删掉 L200 即可。

---

## 6. 【笔误】两处命名

- **L316 `budget_billed_mode`** → 应为 `budget_billed_model`。这是个会写进访问日志的字段名，**上了看板再改成本很高**（下游查询、告警规则都要跟着改），建议在第一次上线前改掉。
- **L56 `ctxKeyEffectiveModl`** 少一个 `e`，是为了让 gofmt 的等号对齐更整齐而牺牲了可读性。建议改名 `ctxKeyActualModel`。

---

## 7. 【注释与代码不符】isCheaper 的兜底方向

**位置**：`main.go` L366–L374

注释写「目标模型未配置单价时，**保守认为不便宜**，避免误降级」，但 L373 实际 `return true`（允许降级）。

代码的选择是对的——运维在 `degrade_levels` 里显式写了 `model: xxx` 本身就是明确的降级意图，因为漏配一个单价就静默不生效，比允许降级更难排查。**改注释即可，不要改代码。**

---

## 待实测（不是缺陷，是需要在你的环境确认的假设）

见 [`design.md`](design.md) §10，摘要：

1. **请求体阶段改写路由头能否触发 Envoy 重新选路** —— 最关键的一条。`model-router` 用的正是这个模式，但路由缓存的清除时机在不同 Higress 版本上需要实测，尤其当不同模型走**不同 Route/上游服务**时。若实测不生效，退路是把决策提到 AUTHN 阶段（priority > 900）在 header 阶段完成，代价是拿不到 `x-mse-consumer`。
2. 本插件（800）与 `ai-token-ratelimit`（600）的预算口径是否要打通。
3. `ai-proxy` Fallback 触发后，响应体里是否会透传真实模型名。
4. 高 QPS 下 Redis 连接池是否够用。
