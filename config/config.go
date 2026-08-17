// Copyright (c) 2026 icheyo
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.

package config

import (
	"errors"
	"fmt"
	"strings"

	"github.com/higress-group/wasm-go/pkg/log"
	"github.com/higress-group/wasm-go/pkg/wrapper"
	"github.com/tidwall/gjson"
)

// 租户标识来源类型
const (
	TenantByConsumer = "consumer" // 从 x-mse-consumer 请求头取（需 key-auth 等认证插件在 AUTHN 阶段先执行）
	TenantByHeader   = "header"
	TenantByParam    = "param"
	TenantByCookie   = "cookie"
	TenantFixed      = "fixed" // 全局单租户，直接用 key 字段作为租户名

	ConsumerHeader = "x-mse-consumer"

	DefaultModelKey     = "model"
	DefaultModelHeader  = "x-higress-llm-model"
	DefaultRejectedCode = 429
	DefaultRedisTimeout = 1000

	// 预算与成本统一使用「微单位」整数累加：
	// 1 个货币单位 = 1_000_000 微单位。
	// 单价按「每百万 Token 的货币单位数」配置，因此
	//   cost_micro = round(inputTokens*priceIn + outputTokens*priceOut)
	// 天然就是微单位整数，无浮点累加误差。
	MicroScale = 1_000_000
)

// ModelPrice 模型单价，单位：货币单位 / 百万 Token
type ModelPrice struct {
	Input  float64
	Output float64
}

// DegradeLevel 一档降级策略。
// 当「剩余预算比例 <= Threshold」时命中该档，把请求模型改写为 Model。
// 多档按 Threshold 升序排列，取第一条满足的（即阈值最小、最严格的一档），
// 保证水位越低降级越狠：remain=0.05 命中 0.10 档而不是 0.30 档。
type DegradeLevel struct {
	Name      string
	Threshold float64 // (0,1]，剩余比例阈值
	Model     string  // 目标模型；为空表示该档只告警不改写
	Reject    bool    // 该档直接拒绝（通常用于 threshold=0 的兜底档）

	// MaxRequestBytes 请求体超过此字节数时不降级（目标模型上下文窗口可能装不下）。
	// 0 表示不设限。
	//
	// 这里刻意用「运维配置的字节阈值」而不是「插件运行时估算 token 数」：
	// token 估算的偏差取决于语言构成和分词器，估小了会让请求在降级后直接失败。
	// 正确做法是 dry_run 期间用 budget_request_bytes 与响应里的真实
	// usage.prompt_tokens 对比，反推出本链路的字节/token 比值，再由人定阈值。
	// 详见 docs/user-manual.md。
	MaxRequestBytes int
}

// 已知的流量特征项。运维在 traffic_profile 里声明本 route 会用到哪些能力，
// 在 model_capabilities 里声明各模型支持哪些能力，插件在【配置生效时】比对。
// 运行时不做任何请求内容检测。
const (
	CapTools      = "tools"       // 工具/函数调用
	CapVision     = "vision"      // 图片输入
	CapAudio      = "audio"       // 音频输入
	CapJSONSchema = "json_schema" // 结构化输出 response_format
)

// KnownCapabilities 用于校验拼写，避免 traffic_profile 写错一个字母就静默失效
var KnownCapabilities = []string{CapTools, CapVision, CapAudio, CapJSONSchema}

func isKnownCapability(c string) bool {
	for _, k := range KnownCapabilities {
		if k == c {
			return true
		}
	}
	return false
}

// TenantSource 租户标识提取方式
type TenantSource struct {
	Type string
	Key  string
}

type BudgetRouterConfig struct {
	RedisClient wrapper.RedisClient

	RuleName       string
	RedisKeyPrefix string
	TenantSource   TenantSource
	BudgetPeriod   int64   // 预算周期，秒
	Quota          float64 // 周期总预算，货币单位
	QuotaMicro     int64   // 周期总预算，微单位
	DegradeLevels  []DegradeLevel
	ModelPrices    map[string]ModelPrice
	DefaultPrice   ModelPrice

	// TrafficProfile 本 route 上的请求会用到哪些能力（运维声明，插件不检测）
	TrafficProfile []string
	// ModelCapabilities 各模型支持哪些能力。可选；填了就在配置生效时
	// 校验每个降级目标能否承接 TrafficProfile。
	ModelCapabilities map[string]map[string]bool

	ModelKey        string
	ModelToHeader   string
	PathSuffixes    []string
	RejectedCode    uint32
	RejectedMessage string

	// 观测/灰度开关
	DryRun          bool // 只打标记与日志，不真正改写 model
	FailOpen        bool // Redis 异常时是否放行（默认 true）
	MaxBodyBytes    uint32
	RecordAttribute bool
}

func (c *BudgetRouterConfig) PriceOf(model string) ModelPrice {
	if p, ok := c.ModelPrices[model]; ok {
		return p
	}
	return c.DefaultPrice
}

// MatchLevel 根据剩余比例返回命中的降级档（nil 表示预算充足，维持原路由）
func (c *BudgetRouterConfig) MatchLevel(remainRatio float64) *DegradeLevel {
	for i := range c.DegradeLevels {
		if remainRatio <= c.DegradeLevels[i].Threshold {
			return &c.DegradeLevels[i]
		}
	}
	return nil
}

func InitRedisClient(json gjson.Result, cfg *BudgetRouterConfig) error {
	redisConfig := json.Get("redis")
	if !redisConfig.Exists() {
		return errors.New("missing redis in config")
	}

	serviceName := redisConfig.Get("service_name").String()
	if serviceName == "" {
		return errors.New("redis service name must not be empty")
	}

	servicePort := int(redisConfig.Get("service_port").Int())
	if servicePort == 0 {
		if strings.HasSuffix(serviceName, ".static") {
			servicePort = 80
		} else {
			servicePort = 6379
		}
	}

	timeout := int(redisConfig.Get("timeout").Int())
	if timeout == 0 {
		timeout = DefaultRedisTimeout
	}

	cfg.RedisClient = wrapper.NewRedisClusterClient(wrapper.FQDNCluster{
		FQDN: serviceName,
		Port: int64(servicePort),
	})
	err := cfg.RedisClient.Init(
		redisConfig.Get("username").String(),
		redisConfig.Get("password").String(),
		int64(timeout),
		wrapper.WithDataBase(int(redisConfig.Get("database").Int())),
	)
	if cfg.RedisClient.Ready() {
		log.Info("ai-budget-router: redis init successfully")
	} else {
		log.Error("ai-budget-router: redis init failed, will retry later")
	}
	return err
}

// Parse 解析完整插件配置：先初始化 Redis 客户端，再解析业务参数。
func Parse(json gjson.Result, cfg *BudgetRouterConfig) error {
	if err := InitRedisClient(json, cfg); err != nil {
		return err
	}
	return parseBusiness(json, cfg)
}

// parseBusiness 只解析业务参数，不触碰宿主环境，便于单测覆盖。
func parseBusiness(json gjson.Result, cfg *BudgetRouterConfig) error {
	cfg.RuleName = json.Get("rule_name").String()
	if cfg.RuleName == "" {
		return errors.New("missing rule_name in config")
	}

	cfg.RedisKeyPrefix = json.Get("redis_key_prefix").String()
	if cfg.RedisKeyPrefix == "" {
		cfg.RedisKeyPrefix = "higress-ai-budget"
	}

	// ---- 租户来源 ----
	ts := json.Get("tenant_source")
	cfg.TenantSource.Type = ts.Get("type").String()
	cfg.TenantSource.Key = ts.Get("key").String()
	if cfg.TenantSource.Type == "" {
		cfg.TenantSource.Type = TenantByConsumer
	}
	switch cfg.TenantSource.Type {
	case TenantByConsumer:
		if cfg.TenantSource.Key == "" {
			cfg.TenantSource.Key = ConsumerHeader
		}
	case TenantByHeader, TenantByParam, TenantByCookie, TenantFixed:
		if cfg.TenantSource.Key == "" {
			return fmt.Errorf("tenant_source.key must not be empty when type=%s", cfg.TenantSource.Type)
		}
	default:
		return fmt.Errorf("unsupported tenant_source.type: %s", cfg.TenantSource.Type)
	}

	// ---- 预算 ----
	cfg.BudgetPeriod = json.Get("budget_period").Int()
	if cfg.BudgetPeriod <= 0 {
		cfg.BudgetPeriod = 86400
	}
	cfg.Quota = json.Get("quota").Float()
	if cfg.Quota <= 0 {
		return errors.New("quota must be greater than 0")
	}
	cfg.QuotaMicro = int64(cfg.Quota * MicroScale)
	// QuotaMicro 必须大于 0，避免后续剩余比例计算出现除零或 NaN。
	if cfg.QuotaMicro <= 0 {
		return fmt.Errorf("quota 换算成微单位后为 0（quota=%v），最小可配 0.000001", cfg.Quota)
	}

	// ---- 降级策略 ----
	levels := json.Get("degrade_levels")
	if levels.Exists() && levels.IsArray() {
		for _, l := range levels.Array() {
			lv := DegradeLevel{
				Name:            l.Get("name").String(),
				Threshold:       l.Get("threshold").Float(),
				Model:           l.Get("model").String(),
				Reject:          l.Get("reject").Bool(),
				MaxRequestBytes: int(l.Get("max_request_bytes").Int()),
			}
			if lv.Threshold < 0 || lv.Threshold > 1 {
				return fmt.Errorf("degrade_levels[%s].threshold must be within [0,1], got %v", lv.Name, lv.Threshold)
			}
			if lv.MaxRequestBytes < 0 {
				return fmt.Errorf("degrade_levels[%s].max_request_bytes must not be negative, got %d", lv.Name, lv.MaxRequestBytes)
			}
			if lv.Model == "" && !lv.Reject {
				log.Warnf("ai-budget-router: degrade level %q has neither model nor reject, it will only be marked", lv.Name)
			}
			if lv.Name == "" {
				lv.Name = fmt.Sprintf("level-%d", len(cfg.DegradeLevels))
			}
			cfg.DegradeLevels = append(cfg.DegradeLevels, lv)
		}
	}
	// 按 threshold 升序：MatchLevel 取第一条满足的，即最严格的那档
	for i := 1; i < len(cfg.DegradeLevels); i++ {
		for j := i; j > 0 && cfg.DegradeLevels[j].Threshold < cfg.DegradeLevels[j-1].Threshold; j-- {
			cfg.DegradeLevels[j], cfg.DegradeLevels[j-1] = cfg.DegradeLevels[j-1], cfg.DegradeLevels[j]
		}
	}

	// ---- 模型单价 ----
	cfg.ModelPrices = make(map[string]ModelPrice)
	prices := json.Get("model_prices")
	if prices.Exists() && prices.IsObject() {
		for name, v := range prices.Map() {
			cfg.ModelPrices[name] = ModelPrice{
				Input:  v.Get("input").Float(),
				Output: v.Get("output").Float(),
			}
		}
	}
	dp := json.Get("default_price")
	cfg.DefaultPrice = ModelPrice{Input: dp.Get("input").Float(), Output: dp.Get("output").Float()}
	if cfg.DefaultPrice.Input == 0 && cfg.DefaultPrice.Output == 0 {
		// 未配置默认单价时，按 1 单位/百万 Token 记账，保证扣减不会静默为 0
		cfg.DefaultPrice = ModelPrice{Input: 1, Output: 1}
	}

	// ---- 流量特征声明与模型能力表 ----
	// 这两项配合起来，在【配置生效时】校验每个降级目标能否承接本 route 的流量。
	// 运行时不做任何请求内容检测——那是运维在配置期就该判断清楚的事。
	if tp := json.Get("traffic_profile"); tp.Exists() && tp.IsArray() {
		for _, c := range tp.Array() {
			name := c.String()
			if !isKnownCapability(name) {
				return fmt.Errorf("traffic_profile 含未知能力项 %q，可选值：%v", name, KnownCapabilities)
			}
			cfg.TrafficProfile = append(cfg.TrafficProfile, name)
		}
	}
	if mc := json.Get("model_capabilities"); mc.Exists() && mc.IsObject() {
		cfg.ModelCapabilities = make(map[string]map[string]bool)
		for model, caps := range mc.Map() {
			if !caps.IsObject() {
				return fmt.Errorf("model_capabilities.%s 必须是对象", model)
			}
			set := make(map[string]bool)
			for name, v := range caps.Map() {
				if !isKnownCapability(name) {
					return fmt.Errorf("model_capabilities.%s 含未知能力项 %q，可选值：%v", model, name, KnownCapabilities)
				}
				set[name] = v.Bool()
			}
			cfg.ModelCapabilities[model] = set
		}
	}

	// ---- 其它 ----
	cfg.ModelKey = json.Get("model_key").String()
	if cfg.ModelKey == "" {
		cfg.ModelKey = DefaultModelKey
	}
	if h := json.Get("model_to_header"); h.Exists() {
		cfg.ModelToHeader = h.String() // 显式配空串表示不改写路由头
	} else {
		cfg.ModelToHeader = DefaultModelHeader
	}

	if suffixes := json.Get("enable_on_path_suffix"); suffixes.Exists() && suffixes.IsArray() {
		for _, s := range suffixes.Array() {
			cfg.PathSuffixes = append(cfg.PathSuffixes, s.String())
		}
	} else {
		cfg.PathSuffixes = []string{"/completions", "/messages", "/responses", "/generateContent"}
	}

	if code := json.Get("rejected_code"); code.Exists() {
		cfg.RejectedCode = uint32(code.Uint())
	} else {
		cfg.RejectedCode = DefaultRejectedCode
	}
	cfg.RejectedMessage = json.Get("rejected_message").String()
	if cfg.RejectedMessage == "" {
		cfg.RejectedMessage = `{"error":{"code":"budget_exhausted","message":"tenant AI budget exhausted"}}`
	}

	cfg.DryRun = json.Get("dry_run").Bool()
	if fo := json.Get("fail_open"); fo.Exists() {
		cfg.FailOpen = fo.Bool()
	} else {
		cfg.FailOpen = true
	}
	if mb := json.Get("max_body_bytes"); mb.Exists() {
		cfg.MaxBodyBytes = uint32(mb.Uint())
	} else {
		cfg.MaxBodyBytes = 10 * 1024 * 1024
	}
	if ra := json.Get("record_attribute"); ra.Exists() {
		cfg.RecordAttribute = ra.Bool()
	} else {
		cfg.RecordAttribute = true
	}

	return validateLadder(cfg)
}

// validateLadder 在【配置生效时】跑一遍降级阶梯的一致性检查。
//
// 这些校验全部是零运行时开销的：只在运维 apply 配置时执行一次。
// 之所以要做得这么严，是因为 Higress 控制台对自定义插件只给一个裸 YAML
// 文本框——没有表单、没有字段提示、没有前端校验，配错了会原样下发。
// 这里是唯一的防线。任何一条不通过，整份配置都不生效，Higress 保留当前有效配置。
func validateLadder(cfg *BudgetRouterConfig) error {
	seen := make(map[float64]string, len(cfg.DegradeLevels))
	var prevPriced *DegradeLevel // 上一个「有目标模型且有单价」的档，用于单调性检查

	for i := range cfg.DegradeLevels {
		lv := &cfg.DegradeLevels[i]

		// ① threshold 不能重复，否则靠后的那档永远匹配不到
		if dup, ok := seen[lv.Threshold]; ok {
			return fmt.Errorf("degrade_levels: %q 与 %q 的 threshold 都是 %v，重复的档位永远匹配不到",
				lv.Name, dup, lv.Threshold)
		}
		seen[lv.Threshold] = lv.Name

		// ② reject 档必须是 threshold 最小的那个。
		//    否则「预算彻底耗尽」反而能拿到模型、「还剩一点」却被拒，语义颠倒。
		if lv.Reject && i != 0 {
			return fmt.Errorf("degrade_levels: 拒绝档 %q（threshold=%v）之下还有更低的档 %q，"+
				"reject 必须配在 threshold 最小的一档",
				lv.Name, lv.Threshold, cfg.DegradeLevels[0].Name)
		}

		// ③ 请求体上限不能超过全局缓冲上限，否则这个阈值永远触发不了
		if lv.MaxRequestBytes > 0 && uint32(lv.MaxRequestBytes) > cfg.MaxBodyBytes {
			return fmt.Errorf("degrade_levels[%s].max_request_bytes(%d) 超过全局 max_body_bytes(%d)，该阈值永远不会触发",
				lv.Name, lv.MaxRequestBytes, cfg.MaxBodyBytes)
		}

		if lv.Model == "" {
			continue // 只打标的档或拒绝档，下面几项不适用
		}

		// ④ 降级目标必须配了单价。
		//    否则响应阶段会回落到 default_price 记账——那是一笔假账，比不记更糟。
		price, ok := cfg.ModelPrices[lv.Model]
		if !ok {
			return fmt.Errorf("degrade_levels[%s] 的目标模型 %q 不在 model_prices 中，"+
				"计费会回落到 default_price 记假账；请补上单价",
				lv.Name, lv.Model)
		}

		// ⑤ 单调性：threshold 越小（预算越紧）目标模型必须越便宜。
		//    档位表已按 threshold 升序，所以单价也应当升序。
		//    这条能自动发现「越降越贵」这种肉眼很难看出来的配反了的阶梯。
		if prevPriced != nil {
			prev := cfg.ModelPrices[prevPriced.Model]
			if price.Input+price.Output < prev.Input+prev.Output {
				return fmt.Errorf("degrade_levels 阶梯方向配反了：%q(threshold=%v) 的目标 %s 比更紧张的 %q(threshold=%v) 的目标 %s 还便宜；"+
					"预算越紧张应当降到越便宜的模型",
					lv.Name, lv.Threshold, lv.Model,
					prevPriced.Name, prevPriced.Threshold, prevPriced.Model)
			}
		}
		prevPriced = lv

		// ⑥ 若运维提供了能力表，校验目标模型能承接本 route 声明的流量特征
		if len(cfg.ModelCapabilities) > 0 && len(cfg.TrafficProfile) > 0 {
			caps, known := cfg.ModelCapabilities[lv.Model]
			if !known {
				return fmt.Errorf("degrade_levels[%s] 的目标模型 %q 不在 model_capabilities 中；"+
					"要么补上它的能力声明，要么整块删掉 model_capabilities 改为完全由运维自负",
					lv.Name, lv.Model)
			}
			for _, need := range cfg.TrafficProfile {
				if !caps[need] {
					return fmt.Errorf("degrade_levels[%s] 的目标模型 %q 不满足 traffic_profile：需要 %q，但 model_capabilities.%s.%s = false。"+
						"请更换目标模型，或把这类流量拆到独立 route（见 matchRules）",
						lv.Name, lv.Model, need, lv.Model, need)
				}
			}
		}
	}
	return nil
}
