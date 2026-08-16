package config

import (
	"math"
	"testing"

	"github.com/tidwall/gjson"
)

const sampleJSON = `{
  "rule_name": "tenant-budget",
  "redis": {"service_name": "redis.static", "service_port": 6379, "timeout": 1000},
  "tenant_source": {"type": "header", "key": "x-tenant-id"},
  "budget_period": 86400,
  "quota": 100,
  "degrade_levels": [
    {"name": "warn",     "threshold": 0.30, "model": "qwen-plus"},
    {"name": "degrade",  "threshold": 0.10, "model": "deepseek-v3"},
    {"name": "exhausted","threshold": 0.00, "reject": true}
  ],
  "model_prices": {
    "gpt-4o":      {"input": 2.5,  "output": 10},
    "qwen-plus":   {"input": 0.8,  "output": 2},
    "deepseek-v3": {"input": 0.14, "output": 0.28}
  },
  "default_price": {"input": 1, "output": 1},
  "dry_run": false
}`

// parseNoRedis 复用真实的配置解析逻辑，但跳过需要宿主环境的 Redis 初始化。
func parseNoRedis(t *testing.T, raw string) *BudgetRouterConfig {
	t.Helper()
	cfg := &BudgetRouterConfig{}
	j := gjson.Parse(raw)
	if err := parseBusiness(j, cfg); err != nil {
		t.Fatalf("parse failed: %v", err)
	}
	return cfg
}

func TestDegradeLevelsSortedAscending(t *testing.T) {
	cfg := parseNoRedis(t, sampleJSON)
	if len(cfg.DegradeLevels) != 3 {
		t.Fatalf("want 3 levels, got %d", len(cfg.DegradeLevels))
	}
	for i := 1; i < len(cfg.DegradeLevels); i++ {
		if cfg.DegradeLevels[i].Threshold < cfg.DegradeLevels[i-1].Threshold {
			t.Fatalf("levels not sorted ascending: %+v", cfg.DegradeLevels)
		}
	}
}

func TestMatchLevel(t *testing.T) {
	cfg := parseNoRedis(t, sampleJSON)
	cases := []struct {
		remain float64
		want   string // "" 表示不降级
	}{
		{1.00, ""},
		{0.55, ""},
		{0.3001, ""},
		{0.30, "warn"},
		{0.20, "warn"},
		{0.10, "degrade"},
		{0.05, "degrade"},
		{0.00, "exhausted"},
	}
	for _, c := range cases {
		lv := cfg.MatchLevel(c.remain)
		got := ""
		if lv != nil {
			got = lv.Name
		}
		if got != c.want {
			t.Errorf("remain=%.4f: want level %q, got %q", c.remain, c.want, got)
		}
	}
}

func TestQuotaMicroAndCostRounding(t *testing.T) {
	cfg := parseNoRedis(t, sampleJSON)
	if cfg.QuotaMicro != 100*MicroScale {
		t.Fatalf("want QuotaMicro=%d, got %d", 100*MicroScale, cfg.QuotaMicro)
	}

	// gpt-4o：1000 输入 + 500 输出
	// = 1000*2.5 + 500*10 = 7500 微单位 = 0.0075 货币单位
	p := cfg.PriceOf("gpt-4o")
	cost := int64(math.Round(1000*p.Input + 500*p.Output))
	if cost != 7500 {
		t.Fatalf("want cost 7500 micro, got %d", cost)
	}

	// deepseek-v3 同等 Token 数应显著更低
	pd := cfg.PriceOf("deepseek-v3")
	costD := int64(math.Round(1000*pd.Input + 500*pd.Output))
	if costD >= cost {
		t.Fatalf("deepseek cost %d should be far below gpt-4o cost %d", costD, cost)
	}
	if costD != 280 {
		t.Fatalf("want deepseek cost 280 micro, got %d", costD)
	}
}

func TestUnknownModelFallsBackToDefaultPrice(t *testing.T) {
	cfg := parseNoRedis(t, sampleJSON)
	p := cfg.PriceOf("some-model-not-in-table")
	if p.Input != 1 || p.Output != 1 {
		t.Fatalf("want default price {1,1}, got %+v", p)
	}
}

func TestDefaultsApplied(t *testing.T) {
	cfg := parseNoRedis(t, `{"rule_name":"r","quota":10}`)
	if cfg.BudgetPeriod != 86400 {
		t.Errorf("want default period 86400, got %d", cfg.BudgetPeriod)
	}
	if cfg.ModelKey != DefaultModelKey {
		t.Errorf("want default model key, got %s", cfg.ModelKey)
	}
	if cfg.ModelToHeader != DefaultModelHeader {
		t.Errorf("want default model header, got %s", cfg.ModelToHeader)
	}
	if !cfg.FailOpen {
		t.Errorf("fail_open should default to true")
	}
	if cfg.RejectedCode != DefaultRejectedCode {
		t.Errorf("want default rejected code %d, got %d", DefaultRejectedCode, cfg.RejectedCode)
	}
	if cfg.TenantSource.Type != TenantByConsumer || cfg.TenantSource.Key != ConsumerHeader {
		t.Errorf("want default tenant source consumer/%s, got %+v", ConsumerHeader, cfg.TenantSource)
	}
	if cfg.DefaultPrice.Input != 1 || cfg.DefaultPrice.Output != 1 {
		t.Errorf("want fallback default price {1,1}, got %+v", cfg.DefaultPrice)
	}
}

func TestInvalidConfigRejected(t *testing.T) {
	bad := []string{
		`{"quota":10}`,                 // 缺 rule_name
		`{"rule_name":"r"}`,            // 缺 quota
		`{"rule_name":"r","quota":-1}`, // quota 非正
		`{"rule_name":"r","quota":1,"tenant_source":{"type":"header"}}`, // header 类型缺 key
		`{"rule_name":"r","quota":1,"tenant_source":{"type":"weird","key":"k"}}`,
		`{"rule_name":"r","quota":1,"degrade_levels":[{"name":"x","threshold":1.5}]}`,
	}
	for _, raw := range bad {
		cfg := &BudgetRouterConfig{}
		if err := parseBusiness(gjson.Parse(raw), cfg); err == nil {
			t.Errorf("expected error for config: %s", raw)
		}
	}
}
