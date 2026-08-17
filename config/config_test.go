package config

import (
	"math"
	"strings"
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

// ---------------------------------------------------------------------------
// 配置期校验（validateLadder）
//
// 这些校验是运维唯一的防线：Higress 控制台对自定义插件只给一个裸 YAML 文本框，
// 没有表单也没有前端校验，配错了会原样下发。所以每条都要有回归测试。
// ---------------------------------------------------------------------------

const capsJSON = `
  "model_prices": {
    "gpt-4o":      {"input": 2.5,  "output": 10},
    "qwen-plus":   {"input": 0.8,  "output": 2},
    "deepseek-v3": {"input": 0.14, "output": 0.28}
  },
  "model_capabilities": {
    "qwen-plus":   {"tools": true,  "vision": false},
    "deepseek-v3": {"tools": true,  "vision": false}
  }`

func TestLadderAcceptsWellFormedConfig(t *testing.T) {
	cfg := parseNoRedis(t, `{
      "rule_name": "r", "quota": 100,
      "traffic_profile": ["tools"],`+capsJSON+`,
      "degrade_levels": [
        {"name":"warn",      "threshold":0.30, "model":"qwen-plus",   "max_request_bytes": 200000},
        {"name":"degrade",   "threshold":0.10, "model":"deepseek-v3", "max_request_bytes": 120000},
        {"name":"exhausted", "threshold":0.0,  "reject": true}
      ]}`)
	if len(cfg.DegradeLevels) != 3 {
		t.Fatalf("want 3 levels, got %d", len(cfg.DegradeLevels))
	}
	// 升序后 exhausted 应在最前
	if cfg.DegradeLevels[0].Name != "exhausted" {
		t.Errorf("want exhausted first after sort, got %s", cfg.DegradeLevels[0].Name)
	}
	if got := cfg.DegradeLevels[2].MaxRequestBytes; got != 200000 {
		t.Errorf("warn.max_request_bytes = %d, want 200000", got)
	}
}

func TestLadderRejectsBadConfig(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string // 错误信息里应当出现的关键字
	}{
		{
			"目标模型没配单价（会导致按 default_price 记假账）",
			`{"rule_name":"r","quota":100,"degrade_levels":[
			   {"name":"a","threshold":0.3,"model":"unknown-model"}]}`,
			"model_prices",
		},
		{
			"阶梯方向配反：预算更紧张的档反而用更贵的模型",
			`{"rule_name":"r","quota":100,` + capsJSON + `,
			  "degrade_levels":[
			    {"name":"tight", "threshold":0.10,"model":"qwen-plus"},
			    {"name":"loose", "threshold":0.30,"model":"deepseek-v3"}]}`,
			"方向配反",
		},
		{
			"threshold 重复，靠后的档永远匹配不到",
			`{"rule_name":"r","quota":100,` + capsJSON + `,
			  "degrade_levels":[
			    {"name":"a","threshold":0.3,"model":"qwen-plus"},
			    {"name":"b","threshold":0.3,"model":"deepseek-v3"}]}`,
			"重复",
		},
		{
			"reject 档不在最低位，语义颠倒",
			`{"rule_name":"r","quota":100,` + capsJSON + `,
			  "degrade_levels":[
			    {"name":"floor","threshold":0.0,"model":"deepseek-v3"},
			    {"name":"stop", "threshold":0.1,"reject":true}]}`,
			"reject",
		},
		{
			"max_request_bytes 超过全局缓冲上限，阈值永远触发不了",
			`{"rule_name":"r","quota":100,"max_body_bytes":1000,` + capsJSON + `,
			  "degrade_levels":[
			    {"name":"a","threshold":0.3,"model":"qwen-plus","max_request_bytes":999999}]}`,
			"max_body_bytes",
		},
		{
			"目标模型不满足 traffic_profile 声明的能力",
			`{"rule_name":"r","quota":100,"traffic_profile":["vision"],` + capsJSON + `,
			  "degrade_levels":[
			    {"name":"a","threshold":0.3,"model":"qwen-plus"}]}`,
			"traffic_profile",
		},
		{
			"traffic_profile 拼写错误",
			`{"rule_name":"r","quota":100,"traffic_profile":["tool"],` + capsJSON + `,
			  "degrade_levels":[{"name":"a","threshold":0.3,"model":"qwen-plus"}]}`,
			"未知能力项",
		},
		{
			"声明了能力表但降级目标不在表里",
			`{"rule_name":"r","quota":100,"traffic_profile":["tools"],
			  "model_prices":{"gpt-4o-mini":{"input":0.15,"output":0.6}},
			  "model_capabilities":{"qwen-plus":{"tools":true}},
			  "degrade_levels":[{"name":"a","threshold":0.3,"model":"gpt-4o-mini"}]}`,
			"model_capabilities",
		},
		{
			"max_request_bytes 为负",
			`{"rule_name":"r","quota":100,` + capsJSON + `,
			  "degrade_levels":[{"name":"a","threshold":0.3,"model":"qwen-plus","max_request_bytes":-1}]}`,
			"max_request_bytes",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := &BudgetRouterConfig{}
			err := parseBusiness(gjson.Parse(c.raw), cfg)
			if err == nil {
				t.Fatalf("期望配置被拒绝，但通过了")
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("错误信息应包含 %q，实际是：%v", c.want, err)
			}
		})
	}
}

func TestCapabilityCheckIsOptional(t *testing.T) {
	// 不提供 model_capabilities 时，退化为「完全由运维自负」，不做能力校验
	cfg := parseNoRedis(t, `{
      "rule_name":"r","quota":100,"traffic_profile":["vision"],
      "model_prices":{"deepseek-v3":{"input":0.14,"output":0.28}},
      "degrade_levels":[{"name":"a","threshold":0.3,"model":"deepseek-v3"}]}`)
	if len(cfg.TrafficProfile) != 1 {
		t.Errorf("traffic_profile 仍应被解析，got %v", cfg.TrafficProfile)
	}
}

func TestQuotaMicroMustNotRoundToZero(t *testing.T) {
	// quota > 0 但换算成微单位后为 0，会导致 remain 计算除零出 NaN，
	// 而 math.Max/Min 对 NaN 不钳位，NaN 传到 MatchLevel 后所有比较都是 false。
	// 配置期必须拦掉这类无效预算。
	cfg := &BudgetRouterConfig{}
	err := parseBusiness(gjson.Parse(`{"rule_name":"r","quota":0.0000001}`), cfg)
	if err == nil {
		t.Fatal("quota=1e-7 换算后 QuotaMicro=0，应当被拒绝")
	}
	if !strings.Contains(err.Error(), "微单位") {
		t.Errorf("错误信息应说明换算问题，实际是：%v", err)
	}

	// 边界：最小可用值应当通过
	ok := parseNoRedis(t, `{"rule_name":"r","quota":0.000001}`)
	if ok.QuotaMicro != 1 {
		t.Errorf("quota=1e-6 应得 QuotaMicro=1，实际 %d", ok.QuotaMicro)
	}
}
