# higress-budget-router

[![CI](https://github.com/OWNER/higress-budget-router/actions/workflows/ci.yml/badge.svg)](https://github.com/OWNER/higress-budget-router/actions/workflows/ci.yml)
[![License](https://img.shields.io/badge/license-Apache%202.0-blue.svg)](LICENSE)
[![Go](https://img.shields.io/badge/go-1.24-00ADD8.svg)](go.mod)

**Budget-aware model degradation for the [Higress](https://higress.cn) AI Gateway — actively downgrade to a cheaper model *before* the request goes out, instead of blocking users with a 429 after the money is already spent.**

[中文文档](README_zh-CN.md) · **[User manual (中文)](docs/user-manual.md)** · [Production verification (中文)](docs/production-verification.md) · [Technical design (中文)](docs/technical-design.md)

---

## The gap this fills

Higress ships excellent native AI plugins, but none of them route on *budget state*:

| Layer | Plugin | When it acts | What it decides on |
|---|---|---|---|
| ① Proactive degradation | **higress-budget-router** | **before** the request goes out | **tenant's live budget water level (dynamic)** |
| ② Protocol routing | `ai-proxy` | before the request goes out | model name → provider (static) |
| ③ Backstop rejection | `ai-token-ratelimit` | before the request goes out | accumulated tokens (after-the-fact) |
| ④ Failure fallback | `ai-proxy` fallback | *after* upstream 4xx/5xx | upstream failure signal (reactive) |

Native token rate limiting can only **count afterwards and block afterwards**. When the quota runs out your users get a hard 429 and the business stops. This plugin adds the missing layer: as the budget drains, requests are transparently moved to cheaper models — a soft landing instead of a wall.

```
                    request ──────────────────────────────────────►
┌──────────────┐  ┌───────────────────┐  ┌────────────────┐  ┌──────────────┐
│ model-router │→ │  budget-router    │→ │ai-token-ratelim│→ │   ai-proxy   │
│  AUTHN 900   │  │   DEFAULT 800     │  │  DEFAULT 600   │  │  DEFAULT 100 │
│ extract model│  │ ① check water lvl │  │ ③ 429 backstop │  │ ② protocol   │
│ set route hdr│  │   rewrite model   │  │                │  │ ④ fallback   │
└──────────────┘  └───────────────────┘  └────────────────┘  └──────────────┘
                          ▲                                          │
                          │  ⑤ parse SSE usage + atomic Redis deduct │
                          └──────────────────────────────────────────┘
                    ◄────────────────────────────────── response
```

## ⚠️ Read this before you deploy: priority direction

In Higress, **a larger `priority` value runs earlier** (within the same phase). Getting this backwards is the #1 way to make this plugin silently do nothing — it will load fine, log nothing unusual, and simply never take effect because `ai-proxy` already forwarded the request.

```yaml
spec:
  phase: UNSPECIFIED_PHASE
  priority: 800          # MUST be > ai-quota(750) > ai-token-ratelimit(600) > ai-proxy(100)
```

`DEFAULT` phase rather than `AUTHN` is deliberate: it lets the AUTHN-phase auth plugins (`key-auth`, `jwt-auth`) run first so this plugin can read `x-mse-consumer` as the tenant key.

## Quick start

```bash
# 1. build
make build                    # go test + vet + wasm

# 2. push the OCI image
make push REGISTRY=ghcr.io/OWNER VERSION=0.1.0

# 3. apply
kubectl apply -f examples/basic.yaml
```

Minimal config — a 200-unit daily budget per consumer, two degradation steps, then reject:

```yaml
rule_name: tenant-daily-budget
redis:
  service_name: redis.dns
  service_port: 6379
tenant_source:
  type: consumer                # reads x-mse-consumer
budget_period: 86400
quota: 200
degrade_levels:
  - { name: warn,      threshold: 0.30, model: qwen-plus }
  - { name: degrade,   threshold: 0.10, model: deepseek-v3 }
  - { name: exhausted, threshold: 0.0,  reject: true }
model_prices:                   # cost per MILLION tokens
  gpt-4o:      { input: 2.5,  output: 10 }
  qwen-plus:   { input: 0.8,  output: 2 }
  deepseek-v3: { input: 0.14, output: 0.28 }
dry_run: true                   # start here — mark and log only, rewrite nothing
```

More scenarios in [`examples/`](examples/).

## User-facing policy model

`degrade_levels` is the central user-facing policy: it describes both when to downgrade and when to reject. `threshold` is the **remaining budget ratio**, not the used ratio; a level with `model` rewrites the request model, while a level with `reject: true` returns 429 without calling the upstream LLM.

Budgets are tracked by `tenant + rule_name + budget_period`, not separately per model by default. `model_prices.input/output` are prices per million input/output tokens, and the real token counts come from the upstream response `usage` field. See the [user manual](docs/user-manual.md) for the full explanation.

## How it works

**Request headers phase** — resolve the tenant, build the Redis key, fire an async read-only `EVAL` for the water level, and park the request (`HeaderStopAllIterationAndWatermark`) until the callback resumes it.

**Request body phase** — match the remaining-budget ratio against the degradation ladder, then rewrite `body.model` with `sjson` and the `x-higress-llm-model` routing header to match.

**Response body phase (streaming)** — accumulate usage per chunk via the SDK's `tokenusage.GetTokenUsage` (which already handles SSE framing plus OpenAI / Anthropic / Gemini / Doubao field layouts), then on the last chunk convert to cost and `INCRBY` it into Redis atomically.

### No pre-deduction, by design

The request phase reads the water level but **never writes**. Pre-deducting would require estimating output tokens, and LLM output length varies wildly for the same prompt — estimate high and users get degraded early on budget that was never spent; estimate low and it does nothing. Exact post-hoc deduction costs one request of lag and buys precise accounting.

### Integer accounting in "micro units"

Prices are configured **per million tokens**, so the denominators cancel out and cost lands directly on an integer:

```
inputTokens/1e6 × price_in currency units  ==  inputTokens × price_in micro units
∴ cost_micro = round(in × price_in + out × price_out)
```

That is why the whole pipeline can use integer `INCRBY` instead of drift-prone `INCRBYFLOAT`. 1 currency unit = 1e6 micro units.

## Configuration

| Field | Type | Default | Description |
|---|---|---|---|
| `rule_name` | string | **required** | Part of the Redis key |
| `redis.service_name` | string | **required** | Service registered via McpBridge |
| `redis.service_port` | int | 6379 (80 for `.static`) | |
| `redis.timeout` | int | 1000 | ms |
| `redis.username` / `password` / `database` | | | |
| `redis_key_prefix` | string | `higress-ai-budget` | |
| `tenant_source.type` | enum | `consumer` | `consumer`/`header`/`param`/`cookie`/`fixed` |
| `tenant_source.key` | string | `x-mse-consumer` | required unless type is `consumer` |
| `budget_period` | int | 86400 | seconds; Redis TTL rolls the budget over |
| `quota` | float | **required** | budget per period, in currency units |
| `degrade_levels[].name` | string | `level-N` | shown in log attributes |
| `degrade_levels[].threshold` | float | **required** | **remaining** ratio, [0,1] |
| `degrade_levels[].model` | string | | target model; empty = mark only |
| `degrade_levels[].reject` | bool | false | reject with `rejected_code`; must be the lowest threshold |
| `degrade_levels[].max_request_bytes` | int | 0 (off) | skip degradation above this body size — calibrate from dry-run data, see [user manual §11](docs/user-manual.md#11-校准-max_request_bytes) |
| `traffic_profile` | []string | - | capabilities this route's traffic uses: `tools`/`vision`/`audio`/`json_schema` |
| `model_capabilities` | object | - | per-model capability table; validated **at config-apply time**, zero runtime cost |
| `model_prices.<model>.input/output` | float | | per **million** tokens |
| `default_price.input/output` | float | 1 / 1 | fallback for unknown models |
| `model_key` | string | `model` | gjson path to the model field |
| `model_to_header` | string | `x-higress-llm-model` | empty disables header rewrite |
| `enable_on_path_suffix` | []string | `/completions` `/messages` `/responses` `/generateContent` | |
| `rejected_code` | int | 429 | |
| `rejected_message` | string | JSON error body | |
| `dry_run` | bool | false | decide and log, rewrite nothing |
| `fail_open` | bool | true | let traffic through when Redis is unreachable |
| `record_attribute` | bool | true | write AI access-log attributes |
| `max_body_bytes` | int | 10485760 | request body buffer cap |

**Ladder matching**: levels are sorted by `threshold` **ascending**, and the first one satisfying `remaining <= threshold` wins — i.e. the strictest applicable step. With `[0.00, 0.10, 0.30]`, a remaining ratio of `0.05` hits the `0.10` step, not `0.30`.

## Observability

Written into the Higress AI access log (`wrapper.AILogKey`):

`budget_tenant` · `budget_level` · `budget_remain_ratio` · `budget_original_model` · `budget_actual_model` · `budget_degraded` · `budget_degrade_blocked_by` · `budget_request_bytes` · `budget_cost_micro` · `budget_billed_model`

Dashboards worth building:

- **Degradation rate** = `budget_degraded=true` / total, split by tenant
- **Level distribution over time** — tells you whether your thresholds are too aggressive
- **`exhausted` count** — should stay at zero; anything else means the ladder failed to absorb the load
- **Cost delta** between `budget_original_model` and `budget_actual_model` — quantifies what the plugin saved

## Rollout

1. `dry_run: true` on a single ingress; watch the level distribution for 1–3 days
2. Flip to `dry_run: false` for low-priority tenants; reconcile `budget_cost_micro` against your provider invoice
3. Roll out everywhere, then **raise** the `ai-token-ratelimit` thresholds so it goes back to being a true last-resort backstop rather than a daily blocker

Rollback is `configDisable: true` — instant, and the Redis counters expire on their own TTL.

## Consistency model

Eventually consistent, deliberately:

| Situation | Behaviour | Why |
|---|---|---|
| Concurrent requests read the same level | all decide identically; one batch may overshoot | degradation is a soft decision; strong consistency needs pre-deduct + rollback |
| Redis unreachable (request phase) | `fail_open: true` → pass through; `false` → reject | availability first by default |
| Redis deduct fails (response phase) | log an error, response unaffected | the response is already on its way out |
| Upstream returns no usage | skip deduction | better to under-record than to mis-record |
| `ai-proxy` fell back to another model | bill the model reported in the response | billing must follow what actually ran |

## Requirements

- Higress ≥ 2.0 (AI gateway plugins, `wasm-go` SDK)
- Go 1.24+ (`GOOS=wasip1 GOARCH=wasm`)
- Redis reachable from the gateway and registered through `McpBridge`

## Status & limits

This project is ready for local and controlled-environment validation. Before production, run the [production verification guide](docs/production-verification.md) against your Higress version and target routing setup.

Current limits:

- Default coverage is chat/generation JSON paths: `/completions`, `/messages`, `/responses`, `/generateContent`.
- Non-JSON, multipart, audio upload, embeddings, rerank, and moderation requests are not billed by the default policy.
- Billing requires an upstream `usage` field.
- Route re-selection after request-body-phase model rewrite must be verified in the target Higress environment.
- Capability compatibility is enforced at config time through `traffic_profile` and `model_capabilities`; the plugin does not inspect request content at runtime.

See [`docs/known-issues.md`](docs/known-issues.md) for the current limit list.

## License

[Apache 2.0](LICENSE)
