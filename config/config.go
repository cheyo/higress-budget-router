// Copyright (c) 2026 cheyo
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
}

// TenantSource 租户标识提取方式
type TenantSource struct {
	Type string
	Key  string
}

type BudgetRouterConfig struct {
	RedisClient wrapper.RedisClient

	RuleName        string
	RedisKeyPrefix  string
	TenantSource    TenantSource
	BudgetPeriod    int64   // 预算周期，秒
	Quota           float64 // 周期总预算，货币单位
	QuotaMicro      int64   // 周期总预算，微单位
	DegradeLevels   []DegradeLevel
	ModelPrices     map[string]ModelPrice
	DefaultPrice    ModelPrice
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

	// ---- 降级策略 ----
	levels := json.Get("degrade_levels")
	if levels.Exists() && levels.IsArray() {
		for _, l := range levels.Array() {
			lv := DegradeLevel{
				Name:      l.Get("name").String(),
				Threshold: l.Get("threshold").Float(),
				Model:     l.Get("model").String(),
				Reject:    l.Get("reject").Bool(),
			}
			if lv.Threshold < 0 || lv.Threshold > 1 {
				return fmt.Errorf("degrade_levels[%s].threshold must be within [0,1], got %v", lv.Name, lv.Threshold)
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

	return nil
}
