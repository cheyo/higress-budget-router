// Copyright (c) 2026 icheyo
//
// ai-budget-router：预算感知的主动降级路由插件
//
// 与 Higress 原生插件的分工：
//   - 本插件 (默认阶段, priority 800)：请求发出前，按租户实时预算水位主动改写 model → 主动降级
//   - ai-quota (默认阶段, priority 750)：配额校验
//   - ai-token-ratelimit (默认阶段, priority 600)：Token 限流，极限情况下 429 兜底
//   - ai-proxy (默认阶段, priority 100)：协议转换 + 上游故障 Fallback
//
// 注意：Higress WasmPlugin 的 priority「值越大越先执行」，
// 因此要抢在原生插件之前生效，priority 必须【大于】600/750，而不是小于 100。

package main

import (
	"encoding/json"
	"fmt"
	"math"
	"net/url"
	"strings"

	"higress-budget-router/config"
	"higress-budget-router/util"

	"github.com/higress-group/proxy-wasm-go-sdk/proxywasm"
	"github.com/higress-group/proxy-wasm-go-sdk/proxywasm/types"
	"github.com/higress-group/wasm-go/pkg/log"
	"github.com/higress-group/wasm-go/pkg/tokenusage"
	"github.com/higress-group/wasm-go/pkg/wrapper"
	"github.com/tidwall/gjson"
	"github.com/tidwall/resp"
	"github.com/tidwall/sjson"
)

func main() {}

func init() {
	wrapper.SetCtx(
		"higress-budget-router",
		wrapper.ParseConfig(parseConfig),
		wrapper.ProcessRequestHeaders(onHttpRequestHeaders),
		wrapper.ProcessRequestBody(onHttpRequestBody),
		wrapper.ProcessStreamingResponseBody(onHttpStreamingResponseBody),
	)
}

const (
	// ---- ctx keys ----
	ctxKeyBudgetKey   = "budget_redis_key"
	ctxKeyTenant      = "budget_tenant"
	ctxKeyRemainRatio = "budget_remain_ratio"
	// ctx 内部键名，仅用于跨阶段保存实际模型；不会出现在对外接口上。
	ctxKeyActualModel = "budget_effective_model"
	ctxKeySkip        = "budget_skip"

	// ---- 响应头 ----
	// 只在拒绝时回传，让客户端 SDK 能区分「预算耗尽」和「限流」——
	// 两者都是 429，但重试策略完全不同。
	//
	HeaderBudgetLevel = "x-higress-budget-level"

	// ---- budget_degrade_blocked_by 的取值 ----
	// 「命中了降级档但最终没降」的原因。运维靠这个判断守卫是不是配得过严：
	// request_too_large 占比高 → max_request_bytes 该放宽或换个大窗口的目标模型。
	blockedByRequestTooLarge = "request_too_large"
	blockedByNotCheaper      = "target_not_cheaper"
	blockedByRewriteFailed   = "rewrite_failed"

	// ---- budget_rejected_reason 的取值 ----
	// 两者都返回 429，但运维处置完全不同：前者调预算，后者查 Redis
	rejectReasonExhausted   = "budget_exhausted"
	rejectReasonBackendDown = "budget_backend_unavailable"

	// ReadWaterLevelScript 请求阶段只读脚本。
	// KEYS[1] = 预算计数键；ARGV[1] = 周期秒数
	// 返回 {used_micro, ttl}
	ReadWaterLevelScript = `
		local used = tonumber(redis.call('get', KEYS[1]) or "0")
		local ttl = redis.call('ttl', KEYS[1])
		if ttl < 0 then ttl = tonumber(ARGV[1]) end
		return {used, ttl}
	`

	// DeductScript 响应阶段原子扣减脚本。
	// KEYS[1] = 预算计数键；ARGV[1] = 本次消耗（微单位）；ARGV[2] = 周期秒数
	// 首次创建键时设置 TTL，保证预算按自然周期滚动重置。
	// 返回累计已用（微单位）
	DeductScript = `
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
	`
)

func parseConfig(j gjson.Result, cfg *config.BudgetRouterConfig) error {
	return config.Parse(j, cfg)
}

// ---------------------------------------------------------------------------
// 请求头阶段：定位租户 → 异步读取 Redis 水位 → 放行给请求体阶段做改写
// ---------------------------------------------------------------------------

func onHttpRequestHeaders(ctx wrapper.HttpContext, cfg config.BudgetRouterConfig) types.Action {
	if !util.HasAnySuffix(ctx.Path(), cfg.PathSuffixes) {
		return skip(ctx, "path not enabled")
	}
	if !ctx.HasRequestBody() {
		return skip(ctx, "no request body")
	}
	contentType, _ := proxywasm.GetHttpRequestHeader("content-type")
	if !strings.Contains(contentType, "application/json") {
		// 非 JSON 请求体（如 multipart 上传）无法安全改写 model。
		//
		// ⚠️ 当前实现下这类请求【也不会被计费】：budgetKey 在下面才构造，
		// 走这条 return 就永远不会被写入 ctx，响应阶段的扣减守卫会直接放弃。
		// 这是当前边界：非 JSON、multipart、音频上传等流量建议拆分独立策略。
		//
		// 默认配置下触发条件很窄：白名单只有 4 个 chat 类路径，客户端要漏发或
		// 写错 content-type 才会走到这里。
		return skip(ctx, "non-json content-type: "+contentType)
	}

	tenant := resolveTenant(ctx, cfg)
	if tenant == "" {
		log.Warnf("cannot resolve tenant (type=%s key=%s), bypass",
			cfg.TenantSource.Type, cfg.TenantSource.Key)
		return skip(ctx, "tenant unresolved")
	}
	// hash tag 保证 Redis Cluster 下同租户的键落在同一 slot
	budgetKey := fmt.Sprintf("%s:{%s}:%s:%d", cfg.RedisKeyPrefix, tenant, cfg.RuleName, cfg.BudgetPeriod)
	ctx.SetContext(ctxKeyTenant, tenant)
	ctx.SetContext(ctxKeyBudgetKey, budgetKey)

	// 请求体要被改写，先摘掉 content-length 并放开缓冲上限
	_ = proxywasm.RemoveHttpRequestHeader("content-length")
	ctx.SetRequestBodyBufferLimit(cfg.MaxBodyBytes)

	err := cfg.RedisClient.Eval(ReadWaterLevelScript, 1,
		[]interface{}{budgetKey},
		[]interface{}{cfg.BudgetPeriod},
		func(response resp.Value) {
			defer func() { _ = proxywasm.ResumeHttpRequest() }()

			if e := response.Error(); e != nil {
				log.Errorf("read water level failed: %v", e)
				ctx.SetContext(ctxKeySkip, true)
				return
			}
			arr := response.Array()
			if len(arr) < 1 {
				log.Errorf("unexpected redis reply for key %s", budgetKey)
				ctx.SetContext(ctxKeySkip, true)
				return
			}
			usedMicro := int64(arr[0].Integer())
			remain := float64(cfg.QuotaMicro-usedMicro) / float64(cfg.QuotaMicro)
			remain = math.Max(0, math.Min(1, remain))

			ctx.SetContext(ctxKeyRemainRatio, remain)
			log.Debugf("tenant=%s key=%s used=%d quota=%d remain=%.4f",
				tenant, budgetKey, usedMicro, cfg.QuotaMicro, remain)
		})

	if err != nil {
		log.Errorf("redis eval dispatch failed: %v", err)
		if !cfg.FailOpen {
			sendRejected(ctx, cfg, rejectReasonBackendDown, "budget backend unavailable")
			return types.ActionPause
		}
		return skip(ctx, "redis dispatch failed")
	}
	return types.HeaderStopAllIterationAndWatermark
}

// ---------------------------------------------------------------------------
// 请求体阶段：按水位命中降级档，改写 model 字段与路由头
// ---------------------------------------------------------------------------

func onHttpRequestBody(ctx wrapper.HttpContext, cfg config.BudgetRouterConfig, body []byte) types.Action {
	if ctx.GetBoolContext(ctxKeySkip, false) {
		return types.ActionContinue
	}
	remainRaw := ctx.GetContext(ctxKeyRemainRatio)
	remain, ok := remainRaw.(float64)
	if !ok {
		log.Debugf("no water level in context, bypass rewrite")
		return types.ActionContinue
	}

	if !json.Valid(body) {
		log.Errorf("invalid json request body, bypass rewrite")
		return types.ActionContinue
	}
	originalModel := gjson.GetBytes(body, cfg.ModelKey).String()
	if originalModel == "" {
		log.Debugf("model field %q absent, bypass rewrite", cfg.ModelKey)
		return types.ActionContinue
	}
	ctx.SetContext(ctxKeyActualModel, originalModel)

	// rec 把本次请求里恒定的几项（原模型、水位、请求体大小）闭包进去，
	// 让下面每个分支只需要交代「命中哪档、实际用了哪个模型、有没有被拦、拦的原因」。
	// record 仍是唯一的观测出口，加字段只改它一处。
	rec := func(level, effectiveModel, blockedBy string, degraded bool) {
		record(ctx, cfg, level, originalModel, effectiveModel, remain, degraded, blockedBy, len(body))
	}

	level := cfg.MatchLevel(remain)
	if level == nil {
		rec("normal", originalModel, "", false)
		return types.ActionContinue
	}

	// 兜底档：预算彻底耗尽，直接拒绝
	if level.Reject {
		rec(level.Name, originalModel, "", false)
		log.Warnf("tenant budget exhausted (remain=%.4f), rejecting", remain)
		if cfg.DryRun {
			return types.ActionContinue
		}
		sendRejected(ctx, cfg, rejectReasonExhausted, fmt.Sprintf("budget exhausted, remaining=%.2f%%", remain*100))
		return types.ActionPause
	}

	// 目标模型为空 / 与原模型相同 / 原模型本就更便宜 → 不改写
	if level.Model == "" || level.Model == originalModel {
		rec(level.Name, originalModel, "", false)
		return types.ActionContinue
	}
	if !isCheaper(cfg, level.Model, originalModel) {
		log.Debugf("target model %s is not cheaper than %s, skip degrade",
			level.Model, originalModel)
		rec(level.Name, originalModel, blockedByNotCheaper, false)
		return types.ActionContinue
	}

	// 请求体过大：目标模型的上下文窗口可能装不下，降级会让请求直接失败。
	//
	// 这里刻意【不做 token 估算】。估算的偏差取决于语言构成和分词器，估小了
	// 就是把「省点钱」换成「请求报错」。阈值由运维在配置里给定，用 dry_run 期间
	// budget_request_bytes 与响应里真实 usage.prompt_tokens 的比值反推。
	// 详见 docs/user-manual.md。
	if level.MaxRequestBytes > 0 && len(body) > level.MaxRequestBytes {
		log.Debugf("request body %d bytes exceeds level %s max_request_bytes %d, skip degrade",
			len(body), level.Name, level.MaxRequestBytes)
		rec(level.Name, originalModel, blockedByRequestTooLarge, false)
		return types.ActionContinue
	}

	if cfg.DryRun {
		log.Infof("[dry-run] would degrade %s -> %s (level=%s remain=%.4f bytes=%d)",
			originalModel, level.Model, level.Name, remain, len(body))
		rec(level.Name, originalModel, "", false)
		return types.ActionContinue
	}

	newBody, err := sjson.SetBytes(body, cfg.ModelKey, level.Model)
	if err != nil {
		log.Errorf("rewrite model failed: %v", err)
		rec(level.Name, originalModel, blockedByRewriteFailed, false)
		return types.ActionContinue
	}
	if err := proxywasm.ReplaceHttpRequestBody(newBody); err != nil {
		log.Errorf("replace request body failed: %v", err)
		rec(level.Name, originalModel, blockedByRewriteFailed, false)
		return types.ActionContinue
	}
	// 同步改写路由头，让下游 model-router / ai-proxy 与 Route 匹配到新模型
	if cfg.ModelToHeader != "" {
		if err := proxywasm.ReplaceHttpRequestHeader(cfg.ModelToHeader, level.Model); err != nil {
			log.Errorf("replace routing header %s failed: %v", cfg.ModelToHeader, err)
		}
	}

	ctx.SetContext(ctxKeyActualModel, level.Model)
	rec(level.Name, level.Model, "", true)
	log.Infof("degraded %s -> %s (level=%s remain=%.4f)",
		originalModel, level.Model, level.Name, remain)
	return types.ActionContinue
}

// ---------------------------------------------------------------------------
// 响应阶段：解析（含 SSE 流式）usage → 折算成本 → Redis 原子扣减
// ---------------------------------------------------------------------------

func onHttpStreamingResponseBody(ctx wrapper.HttpContext, cfg config.BudgetRouterConfig, data []byte, endOfStream bool) []byte {
	if usage := tokenusage.GetTokenUsage(ctx, data); usage.TotalToken > 0 {
		ctx.SetContext(tokenusage.CtxKeyInputToken, usage.InputToken)
		ctx.SetContext(tokenusage.CtxKeyOutputToken, usage.OutputToken)
		if usage.Model != "" && usage.Model != tokenusage.ModelUnknown {
			ctx.SetContext(tokenusage.CtxKeyModel, usage.Model)
		}
	}
	if !endOfStream {
		return data
	}

	// 响应已结束。请求阶段挂的观测属性必须在这里落盘，而且【不能挡在计费成功
	// 路径之后】——上游报错、上游不吐 usage、成本舍入到 0 都会让下面几个分支
	// 提前 return，而那批请求恰恰最需要被看到（"降级后 4xx 率"这个看板就靠它）。
	if cfg.RecordAttribute {
		defer func() { _ = ctx.WriteUserAttributeToLogWithKey(wrapper.AILogKey) }()
	}

	budgetKey := ctx.GetStringContext(ctxKeyBudgetKey, "")
	if budgetKey == "" {
		return data
	}
	inputToken, ok1 := ctx.GetContext(tokenusage.CtxKeyInputToken).(int64)
	outputToken, ok2 := ctx.GetContext(tokenusage.CtxKeyOutputToken).(int64)
	if !ok1 || !ok2 || inputToken+outputToken == 0 {
		log.Debugf("no usage captured for key=%s, skip deduction", budgetKey)
		return data
	}

	// 计费模型优先取响应里回报的真实模型，其次取改写后的生效模型
	model := ctx.GetStringContext(tokenusage.CtxKeyModel, "")
	if model == "" || model == tokenusage.ModelUnknown {
		model = ctx.GetStringContext(ctxKeyActualModel, "")
	}
	price := cfg.PriceOf(model)
	costMicro := int64(math.Round(float64(inputToken)*price.Input + float64(outputToken)*price.Output))
	if costMicro <= 0 {
		log.Debugf("cost rounds to 0 for model=%s in=%d out=%d, skip deduction",
			model, inputToken, outputToken)
		return data
	}

	err := cfg.RedisClient.Eval(DeductScript, 1,
		[]interface{}{budgetKey},
		[]interface{}{costMicro, cfg.BudgetPeriod},
		func(response resp.Value) {
			if e := response.Error(); e != nil {
				log.Errorf("deduct failed key=%s: %v", budgetKey, e)
				return
			}
			used := int64(response.Integer())
			log.Debugf("deducted key=%s model=%s in=%d out=%d cost=%d used=%d/%d",
				budgetKey, model, inputToken, outputToken, costMicro, used, cfg.QuotaMicro)
		})
	if err != nil {
		log.Errorf("deduct dispatch failed key=%s: %v", budgetKey, err)
	}

	if cfg.RecordAttribute {
		// 只挂字段，落盘由函数开头的 defer 统一做（见那里的说明）
		ctx.SetUserAttribute("budget_cost_micro", costMicro)
		ctx.SetUserAttribute("budget_billed_model", model)
	}
	return data
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func skip(ctx wrapper.HttpContext, reason string) types.Action {
	log.Debugf("bypass (%s)", reason)
	ctx.SetContext(ctxKeySkip, true)
	ctx.DontReadRequestBody()
	return types.ActionContinue
}

func resolveTenant(ctx wrapper.HttpContext, cfg config.BudgetRouterConfig) string {
	switch cfg.TenantSource.Type {
	case config.TenantFixed:
		return cfg.TenantSource.Key
	case config.TenantByConsumer, config.TenantByHeader:
		v, err := proxywasm.GetHttpRequestHeader(cfg.TenantSource.Key)
		if err != nil {
			return ""
		}
		return v
	case config.TenantByCookie:
		cookie, err := proxywasm.GetHttpRequestHeader("cookie")
		if err != nil {
			return ""
		}
		return util.ExtractCookieValueByKey(cookie, cfg.TenantSource.Key)
	case config.TenantByParam:
		u, err := url.Parse(ctx.Path())
		if err != nil {
			return ""
		}
		q, err := url.ParseQuery(u.RawQuery)
		if err != nil {
			return ""
		}
		if vs, ok := q[cfg.TenantSource.Key]; ok && len(vs) > 0 {
			return vs[0]
		}
	}
	return ""
}

// isCheaper 用「输入+输出单价之和」粗略比较两个模型的成本。
//
// 任一方未配置单价时【放行降级】（返回 true），不是拦下。理由：运维在
// degrade_levels 里显式写了目标模型，本身就是明确的降级意图；因为漏配一个单价
// 就静默不生效，比允许降级更难排查。而且 validateLadder 在配置期已经强制要求
// 所有降级目标必须有单价，运行时走到这个分支只可能是原模型没配价。
//
// ⚠️ 注意这个兜底方向与能力检查【相反】：能力查不到时必须保守拒绝，因为
// 失败代价不对称——单价未知最坏是多花钱，能力未知会让请求直接失败。
func isCheaper(cfg config.BudgetRouterConfig, target, origin string) bool {
	tp, okT := cfg.ModelPrices[target]
	op, okO := cfg.ModelPrices[origin]
	if !okT || !okO {
		// 任一方未配置单价时不做成本判断，交由运维通过配置显式表达降级意图
		return true
	}
	return tp.Input+tp.Output < op.Input+op.Output
}

// record 是唯一的观测出口。加日志字段只改这一处。
//
// blockedBy 非空表示「本来命中了降级档，但被某道守卫拦下、维持了原模型」。
// 它和 degraded 是互补的两个视角：degraded 回答「省到钱了吗」，
// blockedBy 回答「没省到的话卡在哪」——没有后者就无法判断守卫是不是配得过严。
func record(ctx wrapper.HttpContext, cfg config.BudgetRouterConfig,
	level, originalModel, effectiveModel string, remain float64, degraded bool,
	blockedBy string, requestBytes int) {
	if !cfg.RecordAttribute {
		return
	}
	// 必须逐键 SetUserAttribute，不能用 SetUserAttributeMap。
	// SDK 里 SetUserAttributeMap 的实现是 `ctx.userAttribute = kvmap`——
	// 【整体替换】而不是合并，会覆盖请求阶段已经写入的观测字段。
	ctx.SetUserAttribute("budget_tenant", ctx.GetStringContext(ctxKeyTenant, ""))
	ctx.SetUserAttribute("budget_level", level)
	ctx.SetUserAttribute("budget_remain_ratio", fmt.Sprintf("%.4f", remain))
	ctx.SetUserAttribute("budget_original_model", originalModel)
	ctx.SetUserAttribute("budget_actual_model", effectiveModel)
	ctx.SetUserAttribute("budget_degraded", degraded)
	// 与响应阶段的真实 usage.prompt_tokens 配对，用于反推本链路的
	// 字节/token 比值，进而定出 max_request_bytes。见用户手册「校准 max_request_bytes」一节。
	ctx.SetUserAttribute("budget_request_bytes", requestBytes)
	if blockedBy != "" {
		ctx.SetUserAttribute("budget_degrade_blocked_by", blockedBy)
	}
}

// sendRejected 直接返回拒绝响应。
//
// 请求到此终止，响应阶段不会再执行，所以【必须在这里把观测属性落盘】——
// 被拒绝的请求是最需要出现在看板上的那批，漏了就等于这个事件不存在。
func sendRejected(ctx wrapper.HttpContext, cfg config.BudgetRouterConfig, reason, detail string) {
	if cfg.RecordAttribute {
		// 区分「预算耗尽」和「Redis 不可达导致 fail-closed 拒绝」——
		// 两者都是 429，但运维的处置动作完全不同
		ctx.SetUserAttribute("budget_rejected_reason", reason)
		_ = ctx.WriteUserAttributeToLogWithKey(wrapper.AILogKey)
	}
	headers := [][2]string{
		{"content-type", "application/json; charset=utf-8"},
		{HeaderBudgetLevel, "exhausted"},
	}
	log.Warnf("reject request: %s", detail)
	_ = proxywasm.SendHttpResponse(cfg.RejectedCode, headers, []byte(cfg.RejectedMessage), -1)
}
