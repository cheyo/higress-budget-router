// Copyright (c) 2026 cheyo
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
		"ai-budget-router",
		wrapper.ParseConfig(parseConfig),
		wrapper.ProcessRequestHeaders(onHttpRequestHeaders),
		wrapper.ProcessRequestBody(onHttpRequestBody),
		wrapper.ProcessStreamingResponseBody(onHttpStreamingResponseBody),
	)
}

const (
	// ---- ctx keys ----
	ctxKeyBudgetKey     = "budget_redis_key"
	ctxKeyTenant        = "budget_tenant"
	ctxKeyRemainRatio   = "budget_remain_ratio"
	ctxKeyUsedMicro     = "budget_used_micro"
	ctxKeyLevelName     = "budget_level"
	ctxKeyOriginalModel = "budget_original_model"
	ctxKeyEffectiveModl = "budget_effective_model"
	ctxKeyDegraded      = "budget_degraded"
	ctxKeySkip          = "budget_skip"

	// ---- 响应头 / 日志属性 ----
	HeaderBudgetRemaining = "x-higress-budget-remaining"
	HeaderBudgetLevel     = "x-higress-budget-level"
	HeaderModelDegraded   = "x-higress-model-degraded"

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
		// 非 JSON 请求体（如 multipart 音频上传）无法安全改写 model，
		// 仍然放行，但响应阶段照常扣减。
		return skip(ctx, "non-json content-type: "+contentType)
	}

	tenant := resolveTenant(ctx, cfg)
	if tenant == "" {
		log.Warnf("ai-budget-router: cannot resolve tenant (type=%s key=%s), bypass",
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
				log.Errorf("ai-budget-router: read water level failed: %v", e)
				ctx.SetContext(ctxKeySkip, true)
				return
			}
			arr := response.Array()
			if len(arr) < 1 {
				log.Errorf("ai-budget-router: unexpected redis reply for key %s", budgetKey)
				ctx.SetContext(ctxKeySkip, true)
				return
			}
			usedMicro := int64(arr[0].Integer())
			remain := float64(cfg.QuotaMicro-usedMicro) / float64(cfg.QuotaMicro)
			remain = math.Max(0, math.Min(1, remain))

			ctx.SetContext(ctxKeyUsedMicro, usedMicro)
			ctx.SetContext(ctxKeyRemainRatio, remain)
			log.Debugf("ai-budget-router: tenant=%s key=%s used=%d quota=%d remain=%.4f",
				tenant, budgetKey, usedMicro, cfg.QuotaMicro, remain)
		})

	if err != nil {
		log.Errorf("ai-budget-router: redis eval dispatch failed: %v", err)
		if !cfg.FailOpen {
			sendRejected(cfg, "budget backend unavailable")
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
		log.Debugf("ai-budget-router: no water level in context, bypass rewrite")
		return types.ActionContinue
	}

	if !json.Valid(body) {
		log.Errorf("ai-budget-router: invalid json request body, bypass rewrite")
		return types.ActionContinue
	}
	originalModel := gjson.GetBytes(body, cfg.ModelKey).String()
	if originalModel == "" {
		log.Debugf("ai-budget-router: model field %q absent, bypass rewrite", cfg.ModelKey)
		return types.ActionContinue
	}
	ctx.SetContext(ctxKeyOriginalModel, originalModel)
	ctx.SetContext(ctxKeyEffectiveModl, originalModel)

	level := cfg.MatchLevel(remain)
	if level == nil {
		record(ctx, cfg, "normal", originalModel, originalModel, remain, false)
		return types.ActionContinue
	}
	ctx.SetContext(ctxKeyLevelName, level.Name)

	// 兜底档：预算彻底耗尽，直接拒绝
	if level.Reject {
		record(ctx, cfg, level.Name, originalModel, originalModel, remain, false)
		log.Warnf("ai-budget-router: tenant budget exhausted (remain=%.4f), rejecting", remain)
		if cfg.DryRun {
			return types.ActionContinue
		}
		sendRejected(cfg, fmt.Sprintf("budget exhausted, remaining=%.2f%%", remain*100))
		return types.ActionPause
	}

	// 目标模型为空 / 与原模型相同 / 原模型本就更便宜 → 不改写
	if level.Model == "" || level.Model == originalModel {
		record(ctx, cfg, level.Name, originalModel, originalModel, remain, false)
		return types.ActionContinue
	}
	if !isCheaper(cfg, level.Model, originalModel) {
		log.Debugf("ai-budget-router: target model %s is not cheaper than %s, skip degrade",
			level.Model, originalModel)
		record(ctx, cfg, level.Name, originalModel, originalModel, remain, false)
		return types.ActionContinue
	}

	if cfg.DryRun {
		log.Infof("ai-budget-router[dry-run]: would degrade %s -> %s (level=%s remain=%.4f)",
			originalModel, level.Model, level.Name, remain)
		record(ctx, cfg, level.Name, originalModel, originalModel, remain, false)
		return types.ActionContinue
	}

	newBody, err := sjson.SetBytes(body, cfg.ModelKey, level.Model)
	if err != nil {
		log.Errorf("ai-budget-router: rewrite model failed: %v", err)
		record(ctx, cfg, level.Name, originalModel, originalModel, remain, false)
		return types.ActionContinue
	}
	if err := proxywasm.ReplaceHttpRequestBody(newBody); err != nil {
		log.Errorf("ai-budget-router: replace request body failed: %v", err)
		record(ctx, cfg, level.Name, originalModel, originalModel, remain, false)
		return types.ActionContinue
	}
	// 同步改写路由头，让下游 model-router / ai-proxy 与 Route 匹配到新模型
	if cfg.ModelToHeader != "" {
		if err := proxywasm.ReplaceHttpRequestHeader(cfg.ModelToHeader, level.Model); err != nil {
			log.Errorf("ai-budget-router: replace routing header %s failed: %v", cfg.ModelToHeader, err)
		}
	}

	ctx.SetContext(ctxKeyEffectiveModl, level.Model)
	record(ctx, cfg, level.Name, originalModel, level.Model, remain, true)
	log.Infof("ai-budget-router: degraded %s -> %s (level=%s remain=%.4f)",
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

	budgetKey := ctx.GetStringContext(ctxKeyBudgetKey, "")
	if budgetKey == "" {
		return data
	}
	inputToken, ok1 := ctx.GetContext(tokenusage.CtxKeyInputToken).(int64)
	outputToken, ok2 := ctx.GetContext(tokenusage.CtxKeyOutputToken).(int64)
	if !ok1 || !ok2 || inputToken+outputToken == 0 {
		log.Debugf("ai-budget-router: no usage captured for key=%s, skip deduction", budgetKey)
		return data
	}

	// 计费模型优先取响应里回报的真实模型，其次取改写后的生效模型
	model := ctx.GetStringContext(tokenusage.CtxKeyModel, "")
	if model == "" || model == tokenusage.ModelUnknown {
		model = ctx.GetStringContext(ctxKeyEffectiveModl, "")
	}
	price := cfg.PriceOf(model)
	costMicro := int64(math.Round(float64(inputToken)*price.Input + float64(outputToken)*price.Output))
	if costMicro <= 0 {
		log.Debugf("ai-budget-router: cost rounds to 0 for model=%s in=%d out=%d, skip deduction",
			model, inputToken, outputToken)
		return data
	}

	err := cfg.RedisClient.Eval(DeductScript, 1,
		[]interface{}{budgetKey},
		[]interface{}{costMicro, cfg.BudgetPeriod},
		func(response resp.Value) {
			if e := response.Error(); e != nil {
				log.Errorf("ai-budget-router: deduct failed key=%s: %v", budgetKey, e)
				return
			}
			used := int64(response.Integer())
			log.Debugf("ai-budget-router: deducted key=%s model=%s in=%d out=%d cost=%d used=%d/%d",
				budgetKey, model, inputToken, outputToken, costMicro, used, cfg.QuotaMicro)
		})
	if err != nil {
		log.Errorf("ai-budget-router: deduct dispatch failed key=%s: %v", budgetKey, err)
	}

	if cfg.RecordAttribute {
		ctx.SetUserAttributeMap(map[string]interface{}{
			"budget_cost_micro":  costMicro,
			"budget_billed_mode": model,
		})
		_ = ctx.WriteUserAttributeToLogWithKey(wrapper.AILogKey)
	}
	return data
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func skip(ctx wrapper.HttpContext, reason string) types.Action {
	log.Debugf("ai-budget-router: bypass (%s)", reason)
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

// isCheaper 用「输入+输出单价之和」粗略比较两个模型的成本；
// 目标模型未配置单价时，保守认为不便宜，避免误降级。
func isCheaper(cfg config.BudgetRouterConfig, target, origin string) bool {
	tp, okT := cfg.ModelPrices[target]
	op, okO := cfg.ModelPrices[origin]
	if !okT || !okO {
		// 任一方未配置单价时不做成本判断，交由运维通过配置显式表达降级意图
		return true
	}
	return tp.Input+tp.Output < op.Input+op.Output
}

func record(ctx wrapper.HttpContext, cfg config.BudgetRouterConfig,
	level, originalModel, effectiveModel string, remain float64, degraded bool) {
	ctx.SetContext(ctxKeyLevelName, level)
	ctx.SetContext(ctxKeyDegraded, degraded)
	if !cfg.RecordAttribute {
		return
	}
	ctx.SetUserAttributeMap(map[string]interface{}{
		"budget_tenant":         ctx.GetStringContext(ctxKeyTenant, ""),
		"budget_level":          level,
		"budget_remain_ratio":   fmt.Sprintf("%.4f", remain),
		"budget_original_model": originalModel,
		"budget_actual_model":   effectiveModel,
		"budget_degraded":       degraded,
	})
}

func sendRejected(cfg config.BudgetRouterConfig, detail string) {
	headers := [][2]string{
		{"content-type", "application/json; charset=utf-8"},
		{HeaderBudgetLevel, "exhausted"},
	}
	log.Warnf("ai-budget-router: reject request: %s", detail)
	_ = proxywasm.SendHttpResponse(cfg.RejectedCode, headers, []byte(cfg.RejectedMessage), -1)
}
