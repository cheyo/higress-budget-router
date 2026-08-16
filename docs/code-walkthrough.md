# `main.go` 逐行解读 — ai-budget-router

- 对应源码：`main.go`，402 行（有效代码 318 行）
- 日期：2026-08-15

阅读顺序建议：先看 §1 的整体骨架，再按 §3 → §4 → §5 的顺序跟着请求的生命周期走一遍，最后看 §6 的辅助函数。§8 是我在逐行核对时发现的问题清单。

---

## 1. 整体骨架

```
L1-13    文件头注释：插件定位 + priority 方向的警告
L14-34   package / import
L36      func main() {}          ← 空的，wasm 插件不走 main
L38-46   func init()             ← 真正的入口：注册四个 Hook
L48-91   const 块               ← ctx key、响应头名、两段 Lua 脚本
L93-95   parseConfig             ← 配置解析（一行转发给 config 包）
L101-166 onHttpRequestHeaders    ← 阶段一：定位租户 + 异步查水位
L172-255 onHttpRequestBody       ← 阶段二：按水位改写 model
L261-321 onHttpStreamingResponseBody ← 阶段三：解析 usage + 扣减
L327-402 helpers                 ← skip / resolveTenant / isCheaper / record / sendRejected
```

三个 Hook 对应请求生命周期的三个切点，中间靠 `ctx.SetContext / GetContext` 传状态。这是 proxy-wasm 编程模型的核心约束：**每个 Hook 是独立的回调，函数内的局部变量不跨 Hook 存活，唯一的跨阶段存储就是 `HttpContext`。**

---

## 2. 文件头与 import（L1–L34）

```go
L1-12   // 注释：四层分工 + priority 越大越先执行的警告
```

把 priority 的方向写进源码注释，是因为这个坑一旦踩了，现象是「插件配好了但完全不生效」，而不是报错——排查成本极高。写在代码里比写在文档里更容易被下一个人看到。

```go
L14  package main
L36  func main() {}
```

这两行组合起来很反直觉：**`package main` 但 `main()` 是空的**。原因是 Higress 的 wasm 插件用 `-buildmode=c-shared` 编译成 wasm 模块，宿主（Envoy）加载后调用的是 proxy-wasm ABI 导出的符号，不是 `main()`。所有初始化必须放进 `init()`——`init()` 在模块加载时由 Go runtime 自动执行。`main()` 必须存在（否则 `package main` 编译不过），但永远不会被调用。

```go
L17  "encoding/json"   → 只用在 L183 json.Valid()，做一次 JSON 合法性预检
L18  "fmt"             → Sprintf 拼 Redis key（L122）和格式化日志
L19  "math"            → Max/Min 钳位（L149）、Round 四舍五入（L290）
L20  "net/url"         → 解析 query string 取租户（L351-355）
L21  "strings"         → Contains 判断 content-type（L109）

L23  "higress-budget-router/config"  → 配置结构与解析
L24  "higress-budget-router/util"    → Cookie 提取、路径后缀匹配

L26  proxywasm          → 宿主 ABI：读写 header/body、发响应、恢复请求
L27  proxywasm/types    → types.Action 枚举
L28  wasm-go/pkg/log    → 日志（自动带上插件名和 request id）
L29  wasm-go/pkg/tokenusage → ★ SSE usage 解析，本插件最省力的一块
L30  wasm-go/pkg/wrapper    → SetCtx、HttpContext、RedisClient
L31  gjson              → 只读 JSON 取值（不反序列化，零分配）
L32  resp               → Redis RESP 协议的返回值类型
L33  sjson              → 写 JSON（改 model 字段）
```

`gjson` + `sjson` 组合而不是 `encoding/json` Marshal/Unmarshal，是刻意的：LLM 请求体可能有几百 KB 的 `messages` 数组，全量反序列化再序列化会带来可观的 CPU 和内存开销，而我们只需要动一个 `model` 字段。`sjson.SetBytes` 是做**字节级的原地替换**，其余部分原样保留——顺便也保住了字段顺序和未知字段。

---

## 3. `init()` 与常量块

### 3.1 Hook 注册（L38–L46）

```go
L38  func init() {
L39      wrapper.SetCtx(
L40          "ai-budget-router",                                    // 插件名，出现在日志里
L41          wrapper.ParseConfig(parseConfig),                      // 配置解析
L42          wrapper.ProcessRequestHeaders(onHttpRequestHeaders),   // 请求头阶段
L43          wrapper.ProcessRequestBody(onHttpRequestBody),         // 请求体阶段（buffer 模式）
L44          wrapper.ProcessStreamingResponseBody(onHttpStreamingResponseBody), // 响应体阶段（流式）
L45      )
L46  }
```

四个注册项里有两个关键选择：

**L43 用 `ProcessRequestBody`（缓冲）而不是 `ProcessStreamingRequestBody`（流式）。** 因为要改 `model` 字段就必须看到完整的 JSON——流式拿到的是任意切分的字节块，`{"mod` / `el":"gpt-4o"` 可能横跨两个 chunk，无法解析。代价是请求体要全量缓冲进内存，所以 L128 才要设 `SetRequestBodyBufferLimit`。

**L44 反过来用 `ProcessStreamingResponseBody`（流式）而不是缓冲。** 因为 LLM 响应是 SSE 长流，缓冲会把首 token 延迟拖成完整响应延迟——用户体验直接崩掉。流式逐块透传，只在最后一块（`endOfStream=true`）做扣减。

> 注册顺序不影响执行顺序。执行顺序由 Envoy filter chain 的固有阶段决定：requestHeaders → requestBody → responseHeaders → responseBody。

### 3.2 ctx key 常量（L48–L58）

```go
L50  ctxKeyBudgetKey     = "budget_redis_key"        // Redis 键，跨请求/响应阶段传递
L51  ctxKeyTenant        = "budget_tenant"           // 租户标识
L52  ctxKeyRemainRatio   = "budget_remain_ratio"     // 剩余预算比例（float64）
L53  ctxKeyUsedMicro     = "budget_used_micro"       // 已用微单位
L54  ctxKeyLevelName     = "budget_level"            // 命中的档位名
L55  ctxKeyOriginalModel = "budget_original_model"   // 客户端原始模型
L56  ctxKeyEffectiveModl = "budget_effective_model"  // 实际转发的模型
L57  ctxKeyDegraded      = "budget_degraded"         // 是否发生改写
L58  ctxKeySkip          = "budget_skip"             // 本请求跳过处理
```

用常量而不是字面量，是为了避免「写入时拼错一个字母，读取时静默拿到 nil」——`ctx.GetContext` 对不存在的 key 返回 nil 而不是报错，这类 bug 极难查。

`ctxKeyEffectiveModl` 少一个 `e` 是为了和上面几行对齐（gofmt 会按最长的 key 对齐等号）。可读性上有争议，改名成 `ctxKeyActualModel` 更好。

> **注**：`ctxKeyUsedMicro`、`ctxKeyOriginalModel`、`ctxKeyDegraded` 三个写了但没有任何地方读，属于死代码。详见 §8。

### 3.3 响应头常量（L61–L63）

```go
L61  HeaderBudgetRemaining = "x-higress-budget-remaining"
L62  HeaderBudgetLevel     = "x-higress-budget-level"   // 只有这个用到了（L398）
L63  HeaderModelDegraded   = "x-higress-model-degraded"
```

原本的设计是把降级信息通过响应头回传给客户端，让 SDK 能感知「我请求的是 gpt-4o，实际跑的是 qwen-plus」。**目前只实现了拒绝场景（L398），另外两个头是死常量。** 详见 §8。

Go 的规则：未使用的**变量**和**import** 会编译报错，未使用的**常量**不会。所以这三行能悄悄留下来。

### 3.4 Lua 脚本：读水位（L65–L73）

```lua
L69  local used = tonumber(redis.call('get', KEYS[1]) or "0")
L70  local ttl = redis.call('ttl', KEYS[1])
L71  if ttl < 0 then ttl = tonumber(ARGV[1]) end
L72  return {used, ttl}
```

逐行：

- **L69** `redis.call('get', ...)` 在键不存在时返回 Lua 的 `false`（不是 `nil`）。`false or "0"` → `"0"`，再 `tonumber` → `0`。这个 `or "0"` 兜底避免了「新租户第一次请求时 `tonumber(false)` 返回 nil，后续算术报错」。
- **L70** `TTL` 的三种返回：`>= 0` 剩余秒数；`-1` 键存在但无过期时间；`-2` 键不存在。
- **L71** 把 `-1` / `-2` 统一规整成配置的周期长度，让调用方不用关心这三种语义。
- **L72** Lua table 转成 RESP 数组返回，Go 侧用 `response.Array()` 取。

**这段脚本是纯只读的——没有任何 `set` / `incr`。** 这是本插件最核心的设计决策：请求阶段不做预扣。理由在方案文档 §5：LLM 的输出长度方差极大（同一 prompt 可能 50 token 也可能 4000 token），预扣估高则预算被虚占、用户提前被降级，估低则形同虚设。

用 Lua 而不是直接发两条 `GET` + `TTL` 命令，是为了**一次 RTT 拿到两个值**——在请求关键路径上，省一次网络往返是实打实的收益。

### 3.5 Lua 脚本：原子扣减（L75–L90）

```lua
L80  local added = tonumber(ARGV[1])
L81  local window = tonumber(ARGV[2])
L82  local current = redis.call('incrby', KEYS[1], added)
L83  if current == added then
L84      redis.call('expire', KEYS[1], window)
L85  else
L86      local ttl = redis.call('ttl', KEYS[1])
L87      if ttl < 0 then redis.call('expire', KEYS[1], window) end
L88  end
L89  return current
```

- **L82** `INCRBY` 在键不存在时会自动当作 0 处理并创建，所以不需要先判断存在性。这一步是原子的，多个网关实例并发扣减不会丢更新。
- **L83** `current == added` 是一个巧妙的判断：**如果加完之后的总数正好等于本次加的量，说明加之前是 0，也就是这个键是刚创建的。** 只有这时才需要设 TTL。
- **L84** 首次创建才 `EXPIRE`。如果每次都设，TTL 会被不断刷新成完整周期，预算永远不会重置——这是滑动窗口和固定窗口的分水岭，我们要的是固定窗口。
- **L86-87** 修复分支：键存在但 TTL 丢了（比如运维手工 `SET` 过、或者主从切换时的边界情况），补设一次。没有这个分支，一个丢了 TTL 的键会永久占用预算，租户再也用不了服务。
- **L89** 返回累计值，仅用于日志观测（L305）。

整段脚本在 Redis 里是**单次原子执行**的——Redis 的 Lua 执行期间不会被其他命令打断。如果拆成 Go 侧的 `INCRBY` + 判断 + `EXPIRE` 三次调用，中间会有竞态窗口。

---

## 4. 阶段一：`onHttpRequestHeaders`（L101–L166）

### 4.1 四道前置门禁（L102–L120）

```go
L101 func onHttpRequestHeaders(ctx wrapper.HttpContext, cfg config.BudgetRouterConfig) types.Action {
```

签名说明：`cfg` 是**值传递**的副本，不是指针。SDK 这么设计是为了防止 Hook 里误改全局配置——配置是所有请求共享的，改了会污染后续请求。

```go
L102     if !util.HasAnySuffix(ctx.Path(), cfg.PathSuffixes) {
L103         return skip(ctx, "path not enabled")
L104     }
```

**门禁一：路径。** 只处理 `/completions`、`/messages` 这类 LLM 推理路径。`/models`（列模型）、`/health` 这些不消耗 token 的请求直接放行。放在第一位是因为它最便宜——一次字符串后缀比较，能挡掉大部分无关流量。

```go
L105     if !ctx.HasRequestBody() {
L106         return skip(ctx, "no request body")
L107     }
```

**门禁二：有没有请求体。** `HasRequestBody()` 的实现是看 headers 阶段的 `endOfStream` 标志——如果 headers 就是流的结尾，说明没有 body。没 body 就没有 `model` 字段可改。

```go
L108     contentType, _ := proxywasm.GetHttpRequestHeader("content-type")
L109     if !strings.Contains(contentType, "application/json") {
L112         return skip(ctx, "non-json content-type: "+contentType)
L113     }
```

**门禁三：content-type。** multipart（音频转写上传）、二进制流这类请求体无法用 sjson 安全改写。

L108 的 `_` 忽略了 error：header 不存在时 `contentType` 是空串，`strings.Contains("", "application/json")` 为 false，自然走到 skip 分支——所以不需要单独处理 error。

> ⚠️ **这里有个 bug**：L110-111 的注释写着「仍然放行，但响应阶段照常扣减」，但实际上 `skip()` 之后 `budgetKey` 从未被计算（它在 L122，在这个 return 之后），导致响应阶段 L274 因为 `budgetKey == ""` 直接跳过扣减。详见 §8.1。

```go
L115     tenant := resolveTenant(ctx, cfg)
L116     if tenant == "" {
L119         return skip(ctx, "tenant unresolved")
L120     }
```

**门禁四：租户。** 取不到租户就没法记账，只能放行。这里用 `log.Warnf` 而不是 `Debugf`（L117）——取不到租户通常意味着配置错了（比如 `tenant_source: consumer` 但没配 key-auth），值得告警。

四道门禁的顺序是按「成本从低到高」排的：字符串比较 → 读标志位 → 读 header → 读 header + 可能解析 cookie/query。

### 4.2 构造 Redis 键（L121–L124）

```go
L122     budgetKey := fmt.Sprintf("%s:{%s}:%s:%d", cfg.RedisKeyPrefix, tenant, cfg.RuleName, cfg.BudgetPeriod)
L123     ctx.SetContext(ctxKeyTenant, tenant)
L124     ctx.SetContext(ctxKeyBudgetKey, budgetKey)
```

生成形如 `higress-ai-budget:{acme-corp}:tenant-daily-budget:86400`。

**`{tenant}` 外面那对花括号是 Redis Cluster 的 hash tag，不是 Go 的格式化占位符。** Redis Cluster 计算 slot 时，如果键里有 `{...}`，只对花括号内的内容做 CRC16。这保证同一租户的所有键落在同一个 slot，Lua 脚本的多键操作才不会报 `CROSSSLOT` 错误。当前脚本只用一个键，但把 hash tag 提前加上，将来要加「租户+模型」维度的多键统计时不用改键格式、不用洗数据。

把 `budget_period` 也拼进键名：改周期配置（86400 → 3600）时自动换一个新键，不会出现「日预算的累计值被当成小时预算」的错乱。

L123-124 存进 ctx，因为响应阶段还要用——那时 header 已经不可读了。

### 4.3 为改写请求体做准备（L126–L128）

```go
L127     _ = proxywasm.RemoveHttpRequestHeader("content-length")
L128     ctx.SetRequestBodyBufferLimit(cfg.MaxBodyBytes)
```

**L127 必须在 headers 阶段做，不能等到 body 阶段。** headers 一旦发往上游就改不了了。改写 body 后长度会变（`gpt-4o` → `deepseek-v3` 多 5 字节），如果 `content-length` 还是旧值，上游会读少字节或超时挂住。删掉它之后 Envoy 会自动改用 chunked 传输。

`_ =` 显式忽略 error：header 本来就不存在（chunked 请求）时会返回 error，这是正常情况。

**L128** 默认 10 MB。Envoy 的默认请求体缓冲上限较小，长上下文的 LLM 请求（几十轮对话 + 长文档）很容易超过，超了会被截断或报 413。这个值直接吃网关内存（`并发数 × 单请求体大小`），要按实际流量调。

### 4.4 异步查 Redis（L130–L155）

```go
L130     err := cfg.RedisClient.Eval(ReadWaterLevelScript, 1,
L131         []interface{}{budgetKey},        // KEYS
L132         []interface{}{cfg.BudgetPeriod}, // ARGV
L133         func(response resp.Value) {      // 回调
```

参数依次是：脚本、numkeys、KEYS 数组、ARGV 数组、回调。

**这是异步调用——`Eval` 立即返回，回调在 Redis 响应到达时才执行。** proxy-wasm 是单线程事件循环模型，不能阻塞等待；阻塞会卡死整个 worker 线程上的所有请求。

```go
L134             defer func() { _ = proxywasm.ResumeHttpRequest() }()
```

**整个回调里最重要的一行。** L165 返回 `HeaderStopAllIterationAndWatermark` 把请求挂起了，必须有人来唤醒它，否则请求永久卡死直到客户端超时。用 `defer` 而不是在每个 return 前手写一遍，保证 4 条返回路径（error / 数组异常 / 正常 / 潜在 panic）都不会漏。

```go
L136             if e := response.Error(); e != nil {
L137                 log.Errorf(...)
L138                 ctx.SetContext(ctxKeySkip, true)
L139                 return
L140             }
```

Redis 报错（超时、脚本错误、连接断开）→ 标记跳过 → defer 唤醒请求 → 请求正常走下去，不降级。这是 **fail-open**。

**注意这里是 `ctx.SetContext(ctxKeySkip, true)` 而不是调 `skip()` 辅助函数**——因为 `skip()` 里会调 `DontReadRequestBody()`，而此刻 headers 阶段早就返回了，body 的读取策略已经定死，再调没有意义甚至可能出错。这个区别很容易写错。

```go
L141             arr := response.Array()
L142             if len(arr) < 1 { ... 标记跳过并返回 ... }
L147             usedMicro := int64(arr[0].Integer())
```

防御性检查：Lua 脚本理论上一定返回 2 元素数组，但如果有人手工改了脚本、或者 Redis 版本行为不一致，`arr[0]` 会直接 panic。在 wasm 里 panic 会让整个 VM 崩溃，影响该 worker 上所有请求——所以这类边界检查不能省。

`arr[1]`（ttl）目前没用上，脚本里保留是为了将来做「预算即将重置，暂缓降级」这类策略。

```go
L148             remain := float64(cfg.QuotaMicro-usedMicro) / float64(cfg.QuotaMicro)
L149             remain = math.Max(0, math.Min(1, remain))
```

**L148** 计算剩余比例。全程整数减法后才转 float，避免精度问题。

**L149** 钳位到 `[0, 1]`。两种越界都真实存在：超支时 `usedMicro > QuotaMicro` → 负数；如果有人手工把计数改成负数 → 大于 1。钳位之后 `MatchLevel` 的阈值比较才有意义。

`math.Min` 在内、`math.Max` 在外：先砍上界再砍下界，顺序无所谓，两种写法等价。

```go
L151             ctx.SetContext(ctxKeyUsedMicro, usedMicro)
L152             ctx.SetContext(ctxKeyRemainRatio, remain)
```

**L152 是这个阶段唯一的产出**——把水位存进 ctx，交给下一个 Hook。（L151 目前没人读，见 §8.2。）

### 4.5 返回值（L157–L165）

```go
L157     if err != nil {
L158         log.Errorf(...)
L159         if !cfg.FailOpen {
L160             sendRejected(cfg, "budget backend unavailable")
L161             return types.ActionPause
L162         }
L163         return skip(ctx, "redis dispatch failed")
L164     }
L165     return types.HeaderStopAllIterationAndWatermark
```

**L157 的 `err` 是「发起调用」本身失败**（比如 Redis 集群没注册、连接池耗尽），不是 Redis 返回的业务错误。区别在于：这种情况下**回调根本不会被调用**，所以不能返回 Stop——没人来 Resume，请求会卡死。必须走同步返回路径。

**L159-161** fail-closed 模式：预算后端挂了就拒绝请求。适合成本极度敏感、宁可拒服务也不能失控超支的场景。`sendRejected` 之后返回 `ActionPause`——响应已经由插件直接发出，不要继续走 filter chain。

**L165 `HeaderStopAllIterationAndWatermark`** 拆开看：
- `Stop` — 暂停 filter chain，下游插件（ai-quota / ai-token-ratelimit / ai-proxy）都不执行
- `AllIteration` — 连 body 的读取也一起暂停，不只是 headers
- `Watermark` — 触发 Envoy 的流控水位机制，通知下游 TCP 连接减缓发送，避免请求挂起期间数据在缓冲区堆积

必须和回调里的 `ResumeHttpRequest()` 配对使用。这一对是 proxy-wasm 异步编程的标准范式。

---

## 5. 阶段二：`onHttpRequestBody`（L172–L255）

这个 Hook 在完整请求体缓冲完成后被调用一次，`body` 是全量字节。

### 5.1 前置检查（L173–L193）

```go
L173     if ctx.GetBoolContext(ctxKeySkip, false) {
L174         return types.ActionContinue
L175     }
```

上一阶段标记过跳过（Redis 出错等）就直接放行。`GetBoolContext(key, default)` 是 SDK 提供的带默认值取值，省掉手写类型断言。

```go
L176     remainRaw := ctx.GetContext(ctxKeyRemainRatio)
L177     remain, ok := remainRaw.(float64)
L178     if !ok { ... 放行 ... }
```

**双保险。** 理论上 skip 检查已经覆盖了所有异常路径，但 `ctx.GetContext` 返回 `interface{}`，类型断言失败会拿到零值 `0.0`——而 `0.0` 恰好意味着「预算耗尽」，会触发最严厉的降级甚至拒绝。**在这里省掉 `ok` 检查，后果是「插件出点小问题 → 所有请求被误判为预算耗尽 → 全量 429」。** 这类「失败时恰好落进最危险的分支」的陷阱值得专门防一手。

```go
L183     if !json.Valid(body) { ... 放行 ... }
```

先做一次合法性预检。gjson 对非法 JSON 不报错、只返回空值，如果不预检，一个畸形请求体会被静默当成「没有 model 字段」。用 `log.Errorf` 而不是 Debug——客户端发了非法 JSON，上游多半也会拒，值得记录。

```go
L187     originalModel := gjson.GetBytes(body, cfg.ModelKey).String()
L188     if originalModel == "" { ... 放行 ... }
```

`cfg.ModelKey` 默认 `"model"`，但配成 gjson 路径语法可以适配嵌套结构（比如 OpenAI Batch API 的 `body.model`）。

```go
L192     ctx.SetContext(ctxKeyOriginalModel, originalModel)
L193     ctx.SetContext(ctxKeyEffectiveModl, originalModel)
```

**L193 先把生效模型设成原模型**，后面真降级了再覆盖（L250）。这样无论走哪条分支，响应阶段读 `ctxKeyEffectiveModl` 都能拿到正确的值，不用在每条分支里补一次。（L192 目前没人读，见 §8.2。）

### 5.2 档位匹配与拒绝分支（L195–L211）

```go
L195     level := cfg.MatchLevel(remain)
L196     if level == nil {
L197         record(ctx, cfg, "normal", originalModel, originalModel, remain, false)
L198         return types.ActionContinue
L199     }
```

`MatchLevel` 在 `config` 包里：档位表按 threshold **升序**排列，返回第一条满足 `remain <= threshold` 的。返回 nil 表示预算充足。

即使不降级也要 `record`——观测需要完整的分母。只统计降级的请求，算不出降级率。

```go
L200     ctx.SetContext(ctxKeyLevelName, level.Name)
```

冗余：`record()` 内部（L380）会再设一次。见 §8.3。

```go
L203     if level.Reject {
L204         record(...)
L205         log.Warnf(...)
L206         if cfg.DryRun {
L207             return types.ActionContinue
L208         }
L209         sendRejected(cfg, fmt.Sprintf("budget exhausted, remaining=%.2f%%", remain*100))
L210         return types.ActionPause
L211     }
```

兜底档，通常配 `threshold: 0.0`。

**L204 的 `record` 在 L209 的拒绝之前调用**——顺序很重要。拒绝之后请求就终止了，日志属性必须提前写好，否则这些被拒的请求在日志里会缺字段，而它们恰恰是最需要被看到的。

**L206-207 `dry_run` 在拒绝分支单独判一次**：灰度期绝不能真拒绝用户请求。

### 5.3 三道防误伤（L213–L230）

```go
L214     if level.Model == "" || level.Model == originalModel { ... 不改写 ... }
```

**防误伤一。** `Model` 为空 = 该档只告警不改写（用来做「先观察这个水位区间的流量特征」）。目标模型 == 当前模型 = 客户端本来就在用便宜模型，无需改写——省掉一次无意义的 body 重写和路由头变更。

```go
L218     if !isCheaper(cfg, level.Model, originalModel) { ... 不改写 ... }
```

**防误伤二：绝不降级到更贵的模型。** 典型场景：运维配了 `threshold: 0.1 → model: qwen-plus`，但某个客户端本来用的是更便宜的 deepseek——如果无脑改写，插件反而在加速烧预算。

```go
L225     if cfg.DryRun {
L226         log.Infof("ai-budget-router[dry-run]: would degrade %s -> %s ...")
L228         record(ctx, cfg, level.Name, originalModel, originalModel, remain, false)
L229         return types.ActionContinue
L230     }
```

**防误伤三：dry-run。** 完整走一遍决策链，只在最后一步不动手。

**L228 传的是 `originalModel, originalModel` 和 `degraded=false`**——刻意的：日志属性要如实反映**实际发生了什么**，而不是「本来打算做什么」。「打算做什么」通过 L226 的 Infof 单独记录。如果这里记成 `degraded=true`，dry-run 期间的日志会和真实运行时的日志混淆，观测数据就废了。

三道防误伤的顺序：便宜的判断（字符串比较）在前，涉及 map 查找的在后，全局开关在最后——因为 dry-run 也要走完整决策才有观测价值。

### 5.4 执行改写（L232–L254）

```go
L232     newBody, err := sjson.SetBytes(body, cfg.ModelKey, level.Model)
L233     if err != nil { ... 降级失败但放行 ... }
```

`sjson.SetBytes` 返回**新的字节切片**，不修改原 body。改写失败只记日志并放行——降级失败的后果是多花点钱，比中断业务轻得多。

```go
L238     if err := proxywasm.ReplaceHttpRequestBody(newBody); err != nil { ... 放行 ... }
```

真正写回宿主。前面（L127）已经删了 `content-length`，Envoy 会自动处理长度变化。

```go
L244     if cfg.ModelToHeader != "" {
L245         if err := proxywasm.ReplaceHttpRequestHeader(cfg.ModelToHeader, level.Model); err != nil {
L246             log.Errorf(...)
L247         }
L248     }
```

**同步改写路由头 `x-higress-llm-model`。** 只改 body 不改头会导致状态撕裂：body 说 deepseek，路由头说 gpt-4o，`ai-proxy` 可能按头选 provider、按 body 传模型名 → 请求打到错误的上游。

这里改头**只记日志不回滚 body**——因为回滚同样可能失败，两次失败会让状态更乱。已经改了 body 至少方向是对的。

> 这是方案文档 §10 列的第一个待实测点：body 阶段改路由头能否触发 Envoy 重新选路。`model-router` 用的正是这个模式，但路由缓存的清除时机在不同 Higress 版本上需要实测——尤其当不同模型走不同 Route 时。

```go
L250     ctx.SetContext(ctxKeyEffectiveModl, level.Model)
L251     record(ctx, cfg, level.Name, originalModel, level.Model, remain, true)
```

**只有走到这里才 `degraded=true`。** 前面所有失败/跳过分支传的都是 false——`budget_degraded` 这个字段在日志里的语义是「实际改写成功了」，而不是「尝试过改写」。这个严格性是降级率指标可信的前提。

**L250 更新生效模型**，供响应阶段计费兜底用（L287）。

---

## 6. 阶段三：`onHttpStreamingResponseBody`（L261–L321）

```go
L261 func onHttpStreamingResponseBody(ctx wrapper.HttpContext, cfg config.BudgetRouterConfig,
                                      data []byte, endOfStream bool) []byte {
```

签名和前两个 Hook 不同：**返回 `[]byte` 而不是 `types.Action`**——返回值就是要继续往下游发的数据。本插件不改响应内容，所以每条路径都 `return data` 原样透传。

**这个函数在一次 SSE 响应中会被调用几十到几百次**，每来一个 chunk 调一次。所以里面的逻辑必须足够轻。

### 6.1 逐块累积 usage（L262–L271）

```go
L262     if usage := tokenusage.GetTokenUsage(ctx, data); usage.TotalToken > 0 {
L263         ctx.SetContext(tokenusage.CtxKeyInputToken, usage.InputToken)
L264         ctx.SetContext(tokenusage.CtxKeyOutputToken, usage.OutputToken)
L265         if usage.Model != "" && usage.Model != tokenusage.ModelUnknown {
L266             ctx.SetContext(tokenusage.CtxKeyModel, usage.Model)
L267         }
L268     }
```

**`tokenusage.GetTokenUsage` 是整个插件最省力的一块。** SDK 里这个函数近 300 行，已经处理了：

- SSE 分片规整（`UnifySSEChunk`）与按 `\n\n` 切分事件
- OpenAI ChatCompletions / Responses API / Images、Anthropic Messages、Gemini GenerateContent、豆包等多家的 usage 字段路径差异
- Responses API 大 chunk 跨块合并（`mergeLargeResponseAPIChunks`）
- Anthropic 的 cache_creation / cache_read token 单独计数

自己写这块至少要几百行，而且每家厂商改一次协议就要跟一次。

**L262 的短路很关键**：函数内部会先判断 chunk 里有没有 `"usage"` / `"usageMetadata"` 字符串，没有就直接跳过 gjson 解析。SSE 流里绝大多数 chunk 是纯内容增量，不含 usage——所以这个高频调用的实际开销是一次 `bytes.Contains`。

**为什么用「覆盖」而不是「累加」**（L263-264 是 Set 不是 +=）：一次响应里 usage 通常只在最后一个事件里出现一次，且是累计值不是增量。累加会重复计费。

**L265 的 `ModelUnknown` 判断**：SDK 取不到模型时会返回字符串 `"unknown"`，不加这个判断会把 `"unknown"` 存进 ctx，导致 L286 的兜底逻辑失效。

```go
L269     if !endOfStream {
L270         return data
L271     }
```

**中间 chunk 到此为止。** 下面的扣减逻辑一次响应只执行一次。

### 6.2 取数与校验（L273–L295）

```go
L273     budgetKey := ctx.GetStringContext(ctxKeyBudgetKey, "")
L274     if budgetKey == "" {
L275         return data
L276     }
```

请求阶段没走完整流程（被四道门禁挡掉）就没有 budgetKey，不扣减。

> ⚠️ 这行和 L112 的非 JSON skip 组合起来产生了 §8.1 描述的 bug。

```go
L277     inputToken, ok1 := ctx.GetContext(tokenusage.CtxKeyInputToken).(int64)
L278     outputToken, ok2 := ctx.GetContext(tokenusage.CtxKeyOutputToken).(int64)
L279     if !ok1 || !ok2 || inputToken+outputToken == 0 {
L280         log.Debugf(...)
L281         return data
L282     }
```

**宁可漏记，不可乱记。** 上游没返回 usage（有些自建模型服务不吐 usage）时，与其估一个数扣掉，不如不扣——错误的扣减会污染水位，进而让所有租户的降级决策失真。

用 `Debugf` 而不是 `Warnf`：某些接入方常态不返回 usage，用 Warn 会刷屏。

```go
L285     model := ctx.GetStringContext(tokenusage.CtxKeyModel, "")
L286     if model == "" || model == tokenusage.ModelUnknown {
L287         model = ctx.GetStringContext(ctxKeyEffectiveModl, "")
L288     }
```

**计费模型的优先级：响应体里的真实模型 > 我们改写后的模型。**

这个顺序是为 `ai-proxy` 的 Fallback 场景准备的：我们把请求改成了 deepseek-v3，但 deepseek 挂了，`ai-proxy` 自动 Fallback 到了 qwen-plus——那就必须按 qwen-plus 计费。信任响应而不是信任自己的意图。

```go
L289     price := cfg.PriceOf(model)
L290     costMicro := int64(math.Round(float64(inputToken)*price.Input + float64(outputToken)*price.Output))
```

**L290 是整个计费的核心，也是「微单位」设计最关键的一行。**

推导：单价配置的是「每**百万** Token 的货币单位数」，所以
```
inputTokens / 1e6 × price_in  货币单位
= inputTokens × price_in       微单位   （因为 1 货币单位 = 1e6 微单位）
```
**分母天然抵消，结果直接就是整数微单位，不需要任何缩放。** 这就是为什么整个链路能用整数 `INCRBY` 而不是精度会漂移的 `INCRBYFLOAT`。

`math.Round` 而不是截断：大量小额请求向下截断会系统性少计费，长期累积偏差可观。

```go
L291     if costMicro <= 0 { ... 跳过 ... }
```

极小请求 + 极低单价可能四舍五入到 0。发一条 `INCRBY 0` 没有意义，还会创建一个空键。`<= 0` 而不是 `== 0` 是防御负数（单价配成负值这种配置错误）。

### 6.3 扣减与观测（L297–L320）

```go
L297     err := cfg.RedisClient.Eval(DeductScript, 1,
L298         []interface{}{budgetKey},
L299         []interface{}{costMicro, cfg.BudgetPeriod},
L300         func(response resp.Value) { ... 只记日志 ... })
```

**这个回调里没有 `ResumeHttpResponse()`**——因为我们没有暂停响应。响应正常往客户端发，扣减在后台异步完成。用户不会为记账多等一个 Redis RTT。

代价是**扣减结果对本次请求毫无影响**——它影响的是这个租户的**下一次**请求。这正是「事后扣减 + 下次生效」模型的本质：一个请求的滞后，换取零延迟开销和精确计费。

```go
L309     if err != nil {
L310         log.Errorf("ai-budget-router: deduct dispatch failed key=%s: %v", budgetKey, err)
L311     }
```

**扣减失败只记日志，不影响响应。** 响应已经在回客户端的路上，不能因为记账失败而中断——这是把可用性排在计费准确性之前的明确取舍。生产上应该对这条日志配告警：持续出现意味着预算水位正在系统性失真。

```go
L313     if cfg.RecordAttribute {
L314         ctx.SetUserAttributeMap(map[string]interface{}{
L315             "budget_cost_micro":  costMicro,
L316             "budget_billed_mode": model,
L317         })
L318         _ = ctx.WriteUserAttributeToLogWithKey(wrapper.AILogKey)
L319     }
```

补齐成本相关的日志属性。`WriteUserAttributeToLogWithKey(wrapper.AILogKey)` 把属性写进 Higress 的 AI 专用访问日志字段，和 `ai-statistics` 的输出在同一个结构里，下游 SLS / ELK 可以直接联查。

> `budget_billed_mode` 是 `budget_billed_model` 的笔误，应该改掉——字段名一旦上了看板再改就要动下游查询。

---

## 7. 辅助函数（L327–L402）

### `skip`（L327–L332）

```go
L328     log.Debugf("ai-budget-router: bypass (%s)", reason)
L329     ctx.SetContext(ctxKeySkip, true)
L330     ctx.DontReadRequestBody()
L331     return types.ActionContinue
```

三件事打包：记原因、标记跳过、告诉 SDK 不用缓冲请求体。

**L330 是性能上的关键。** 因为注册了 `ProcessRequestBody`，SDK 默认会缓冲每个请求的完整 body。对于插件根本不处理的请求（比如 `/v1/models`），白白缓冲几百 KB 是纯浪费。`DontReadRequestBody()` 明确告诉 SDK 跳过。

**只能在 headers 阶段调用**——这也是为什么 Redis 回调里（L138）不能复用这个函数。

### `resolveTenant`（L334–L364）

五种租户来源：

```go
L336     case config.TenantFixed:            return cfg.TenantSource.Key
```
固定值，整条链路共用一份预算。适合「先给整个网关设个总闸」的最简场景。

```go
L338     case config.TenantByConsumer, config.TenantByHeader:
L339         v, err := proxywasm.GetHttpRequestHeader(cfg.TenantSource.Key)
```
两个 case 合并，因为实现完全一样——`consumer` 只是 `header` 的一个预设（key 默认 `x-mse-consumer`）。分成两个枚举值是为了配置的可读性：写 `type: consumer` 比写 `type: header, key: x-mse-consumer` 意图清晰得多。

**这是推荐用法**，前提是 AUTHN 阶段有 `key-auth` / `jwt-auth` 先跑完写入这个头。这也是本插件选 DEFAULT 800 而不是 AUTHN 阶段的直接原因。

```go
L344     case config.TenantByCookie:
L350     case config.TenantByParam:
```
浏览器直连和调试场景的补充。`TenantByParam` 用 `url.Parse` + `ParseQuery` 而不是手写字符串切分，是为了正确处理 URL 编码（租户名含中文或特殊字符）。

所有失败路径都返回空串，由调用方（L116）统一处理——不返回 error 是因为这里的「取不到」不是异常，是正常的业务分支。

### `isCheaper`（L368–L376）

```go
L369     tp, okT := cfg.ModelPrices[target]
L370     op, okO := cfg.ModelPrices[origin]
L371     if !okT || !okO {
L373         return true
L374     }
L375     return tp.Input+tp.Output < op.Input+op.Output
```

**L375 用「输入单价 + 输出单价」之和做比较，是一个粗糙但够用的近似。** 严格来说应该按实际的输入/输出 token 比例加权，但请求发出前根本不知道会输出多少 token。用简单求和的好处是行为可预测、运维一眼能看懂；真要精细控制，配置里显式指定目标模型即可。

**L371-373 的取舍值得说明**：任一模型未配单价时返回 `true`（允许降级）。

注释里写的是「保守认为不便宜，避免误降级」，但代码实际是**放行降级**——注释和代码语义相反，应该改注释。选择放行的理由是：运维在 `degrade_levels` 里显式写了 `model: xxx`，这本身就是明确的降级意图；因为漏配一个单价就静默不生效，比允许降级更难排查。而 `model_prices` 的主要用途是计费，不是决策。

### `record`（L378–L393）

```go
L380     ctx.SetContext(ctxKeyLevelName, level)
L381     ctx.SetContext(ctxKeyDegraded, degraded)
L382     if !cfg.RecordAttribute {
L383         return
L384     }
L385     ctx.SetUserAttributeMap(map[string]interface{}{ ... 6 个字段 ... })
```

统一的观测出口，被 8 处调用。集中在一个函数里的好处：加字段只改一处，不会出现「某条分支忘了记某个字段」导致看板数据有洞。

**L380-381 在开关判断之前**，因为 ctx 是内部状态传递，和「要不要写日志」无关。

**L388 `fmt.Sprintf("%.4f", remain)` 存成字符串而不是 float64**：日志系统对浮点的处理不一致（有的会科学计数、有的会丢精度），固定 4 位小数的字符串在 SLS / ELK 里最稳，也便于直接做字符串聚合。

**注意 `record` 内部没有调 `WriteUserAttributeToLogWithKey`**——只是把属性挂到 ctx 上。真正落盘在响应阶段的 L318 统一做一次。这样请求阶段和响应阶段的属性会合并成一条完整日志，而不是分成两条。

> 但这里有个隐患：如果请求在 L209 被拒绝，响应阶段的 L318 不会执行，`record` 挂上去的属性就落不了盘。见 §8.4。

### `sendRejected`（L395–L402）

```go
L396     headers := [][2]string{
L397         {"content-type", "application/json; charset=utf-8"},
L398         {HeaderBudgetLevel, "exhausted"},
L399     }
L401     _ = proxywasm.SendHttpResponse(cfg.RejectedCode, headers, []byte(cfg.RejectedMessage), -1)
```

**L397 明确带 charset**：错误信息里有中文，不带 charset 部分客户端会乱码。

**L398 带上 `x-higress-budget-level: exhausted`**，让客户端 SDK 能区分「预算耗尽」和「限流」——两者都是 429，但重试策略完全不同：限流退避几秒就能恢复，预算耗尽要等到下个周期，盲目重试只会浪费。

**L401 最后的 `-1`** 是 `grpcStatus` 参数，-1 表示这不是 gRPC 响应。

---

## 8. 逐行核对时发现的问题

### 8.1 【功能缺陷】非 JSON 请求体不会被计费

**位置**：L108–L113 与 L273–L276

L110-111 的注释和方案文档 §5 都写着「非 JSON body → 跳过改写，**响应阶段仍正常扣减**」。但实际上：

1. L112 走 `skip()` 返回，此时 `budgetKey` 还没被计算（它在 L122）
2. 响应阶段 L273 取到空串 → L274 直接 return，**不扣减**

**影响**：走 multipart 的接口（音频转写 `/audio/transcriptions`、文件上传等）消耗的 token 完全不计入预算。如果这类流量占比不小，预算水位会系统性偏低，降级永远不触发。

**修复方向**：把租户解析和 `budgetKey` 构造（L115-124）提到 content-type 检查之前，再引入一个独立的 `ctxKeyNoRewrite` 标志区分「不改写」和「不计费」。当前的 `ctxKeySkip` 把两件事混在了一起。

### 8.2 【死代码】3 个 ctx key + 2 个响应头常量

| 符号 | 位置 | 状态 |
|---|---|---|
| `ctxKeyUsedMicro` | L53，写于 L151 | 无人读取 |
| `ctxKeyOriginalModel` | L55，写于 L192 | 无人读取 |
| `ctxKeyDegraded` | L57，写于 L381 | 无人读取 |
| `HeaderBudgetRemaining` | L61 | 完全未使用 |
| `HeaderModelDegraded` | L63 | 完全未使用 |

Go 不会对未使用的常量报错，所以这些能一路编译通过。后两个反映的是一个**没做完的设计**：原本要把降级信息通过响应头回传给客户端，只实现了拒绝场景。

**建议**：要么补完（在 `onHttpResponseHeaders` 里加这两个头，让客户端 SDK 能感知降级），要么删掉。留着会让人以为功能已经有了。

### 8.3 【冗余】L200 重复设置

`ctx.SetContext(ctxKeyLevelName, level.Name)` 在 L200 设一次，紧接着 `record()` 内部 L380 又设一次。删掉 L200 即可。

### 8.4 【观测盲区】被拒绝的请求可能丢日志属性

`record()` 只把属性挂到 ctx，真正落盘靠响应阶段的 L318。但 L209 拒绝后请求终止，响应体阶段不会执行——**`exhausted` 档的请求，恰恰是最需要被观测到的，反而可能在日志里缺字段**。

**修复方向**：在 `sendRejected` 之前显式调一次 `ctx.WriteUserAttributeToLogWithKey(wrapper.AILogKey)`。

### 8.5 【边界】quota 极小时除零

L148 `float64(cfg.QuotaMicro-usedMicro) / float64(cfg.QuotaMicro)`：配置校验只拦了 `quota <= 0`，但 `quota: 0.0000001` 会让 `QuotaMicro = int64(0.1) = 0` → 除零 → `remain` 为 NaN 或 Inf。

L149 的 `math.Max/Min` 对 NaN 无效（`math.Min(1, NaN)` 返回 NaN），NaN 传到 `MatchLevel` 后所有 `<=` 比较都是 false → 不降级。**最终是 fail-open，不会造成事故**，但属于靠运气兜住。建议在 `config.Parse` 里加 `QuotaMicro <= 0` 的校验。

### 8.6 【笔误】两处命名

- L316 `budget_billed_mode` → 应为 `budget_billed_model`（上看板后再改成本高）
- L56 `ctxKeyEffectiveModl` 缺一个 `e`，为了 gofmt 对齐牺牲了可读性，建议改名 `ctxKeyActualModel`

### 8.7 【注释与代码不符】

L367 注释写「目标模型未配置单价时，**保守认为不便宜**，避免误降级」，但 L373 实际 `return true`（允许降级）。代码的选择是对的（理由见 §7），改注释即可。

---

## 9. 一句话总结每个函数

| 函数 | 行数 | 一句话 |
|---|---|---|
| `init` | 9 | 注册四个 Hook；body 用缓冲、响应用流式 |
| `parseConfig` | 3 | 一行转发给 config 包 |
| `onHttpRequestHeaders` | 66 | 四道门禁 → 构造 Redis key → 异步查水位 → 挂起请求等回调 |
| `onHttpRequestBody` | 84 | 取水位 → 匹配档位 → 三道防误伤 → sjson 改 model + 改路由头 |
| `onHttpStreamingResponseBody` | 61 | 逐块攒 usage → 末块折算成本 → 异步 INCRBY → 写日志属性 |
| `skip` | 6 | 记原因 + 标记跳过 + 关掉 body 缓冲 |
| `resolveTenant` | 31 | 五种来源取租户，失败一律返回空串 |
| `isCheaper` | 9 | 单价求和比大小；缺单价时放行降级 |
| `record` | 16 | 统一观测出口，8 处调用 |
| `sendRejected` | 8 | 发 429 + 带上 budget-level 头供客户端区分重试策略 |

**318 行有效代码里，主流程大概只占三分之一，剩下三分之二是容错分支和观测。** 这个比例不是臃肿——它是插件敢不敢上生产的分水岭。网关插件跑在所有流量的关键路径上，一个未处理的 nil 就能让整个 wasm VM 崩溃，影响该 worker 上的全部请求。
