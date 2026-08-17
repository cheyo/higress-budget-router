# 本地联调与对比测试

在本机已有的 Higress 原生环境（`D:\claudecode\open-llm-gateway` 的 `docker-compose.native.yml`）上，
手工验证 `ai-budget-router` 的降级效果，并与「不装插件」的基线做对比。

## 插件要不要先发布成包？

**不需要。** Higress 的 `WasmPlugin.spec.url` 支持三种来源：

| 来源 | 写法 | 适用场景 |
|---|---|---|
| OCI 镜像 | `oci://ghcr.io/cheyo/higress-budget-router:0.1.0` | 正式发布、多环境分发 |
| HTTP | `https://…/plugin.wasm` | 内网文件服务 |
| 本地文件 | `file:///opt/plugins/budget-router/plugin.wasm` | **本地联调** |

联调走 `file://`：把编译出的 `main.wasm` 用 `docker cp` 放进网关容器即可，
不必推 ghcr、不必登录任何仓库。[setup.sh](setup.sh) 做的就是这件事。

注意：`docker cp` 进容器的文件在容器**重建**后会丢失（重启不丢）。
`/data` 是命名卷所以配置本身是持久的——两者生命周期不一致，
`docker compose down` 之后需要重新跑一次 `setup.sh`。

## 环境拓扑

```
curl -H "Host: budget.local"
      │
      ▼  :8080
┌─────────────────────────────────────────┐
│ higress-gw                              │
│   Ingress budget-mock → mock-llm.dns    │
│   WasmPlugin ai-budget-router (800)     │──── EVAL 读水位/扣减 ──►  higress-redis :6379
│     └ file:///opt/plugins/…/plugin.wasm │                          （McpBridge: redis.dns）
└─────────────────────────────────────────┘
      │
      ▼  mock-llm.local:8000
 higress-mock-llm —— 原样回显收到的 model，并返回标准 OpenAI usage
```

**为什么用 mock-llm 而不是真实厂商**：现网 4 条厂商路由的 API Key 还是 `YOUR_*_API_KEY`
占位符，打过去只会拿到上游认证失败，看不到降级效果；mock 还会把收到的 `model`
原样回显，等于免费给了我们一个"插件到底改没改写"的探针。

**这条测试路由不经过 `ai-proxy`**：`ai-proxy` 的 `defaultConfigDisable: true`，
只对 qwen/openai/moonshot/azure 四条 ingress 生效。mock 本身就说 OpenAI 协议，
少一层转换，排查时变量更少。

## 前置：编译

本机没装 Go，用容器编译：

```bash
bash test/local/build.sh
```

产出仓库根目录的 `main.wasm`（已在 `.gitignore` 里，不会被提交）。

## 一次性搭建

```bash
bash test/local/setup.sh
```

做四件事：把 `redis.local:6379` / `mock-llm.local:8000` 注册进 McpBridge、
下发 `budget.local` 的测试 Ingress、拷贝 wasm、下发 WasmPlugin（初始 **off**）。

> McpBridge 的 dns 类型只接受带点的 FQDN，裸主机名会被 controller 拒绝，
> 且数据面表现为静默 503——compose 里给两个容器配的 `*.local` 别名就是为此准备的。

## 对比测试：手工三步

### 第一步：基线（插件关闭）

```bash
bash test/local/toggle.sh off
bash test/local/cases.sh baseline
```

插件 `configDisable: true`，请求不经过它。**四个用例的实际模型都应该是 `mock-chat`，
HTTP 全 200**——无论 Redis 里的水位被设成多少，因为根本没人去读它。
这一轮里 `判定` 列除第一行外都会是 FAIL，那是预期的：FAIL 恰恰说明"没有插件就不会降级"。

### 第二步：灰度（dry-run）

```bash
bash test/local/toggle.sh dry-run
bash test/local/cases.sh dry-run
```

决策链完整执行、日志与日志属性照常写、Redis 照常扣减，但**不改写 model**。
所以实际模型仍是 `mock-chat`，同时 `docker logs higress-gw` 里能看到：

```
ai-budget-router[dry-run]: would degrade mock-chat -> mock-chat-backup (level=warn remain=0.4000)
```

这一步用来确认"决策是对的"，再决定要不要真改写。

### 第三步：插件生效

```bash
bash test/local/toggle.sh on
bash test/local/cases.sh plugin
```

期望结果（下表是 2026-08-17 在 all-in-one 2.1.0 上的实测输出）：

| 用例 | 预置已用(micro) | 剩余比例 | 命中档 | 实际模型 | HTTP |
|---|---|---|---|---|---|
| 充足 | 0 | 1.00 | — | `mock-chat`（不改写） | 200 |
| warn | 600000 | 0.40 | `warn` (≤0.50) | `mock-chat-backup` | 200 |
| degrade | 850000 | 0.15 | `degrade` (≤0.20) | `mock-chat-mini` | 200 |
| exhausted | 1000000 | 0.00 | `exhausted` (≤0.00) | 被拒，无上游请求 | 429 |

注意 `degrade` 那行：剩余 0.15 同时满足 `≤0.50` 和 `≤0.20` 两档，
**必须命中更严格的 0.20 档**。档位按 threshold 升序排列、取第一条满足的，
就是为了保证"水位越低降级越狠"。

`exhausted` 那行要看的不只是 429，还要确认 `docker logs higress-mock-llm`
**没有新增记录**——请求在网关就被拦掉了，根本没打到上游，这才是"省钱"的最强形态。

省钱效果直接对比（同样长度的提示词，`cases.sh` 会打印）：

| 水位 | 实际模型 | 单价(每百万token) | 本次扣减(micro) |
|---|---|---|---|
| 充足 | `mock-chat` | 100 | ~5200 |
| warn | `mock-chat-backup` | 10 | ~400 |

同一个请求，降级后成本差一个数量级——注意计费用的是**降级后**模型的单价，
即插件按实际执行的模型记账，不是按客户端请求的模型。

## 手工单发（不想跑整套脚本时）

```bash
bash -c 'source test/local/lib.sh; set_used 600000; ask mock-chat | head -c 400'
```

或者纯 curl（水位自己用 redis-cli 设）：

```bash
curl -s http://127.0.0.1:8080/v1/chat/completions -H "Host: budget.local" -H "Content-Type: application/json" -H "x-tenant-id: acme" -d '{"model":"mock-chat","messages":[{"role":"user","content":"你好"}]}'
```

设水位 / 查水位：

```bash
docker exec higress-redis redis-cli SET 'higress-ai-budget:{acme}:tenant-daily-budget:3600' 600000 EX 3600
```

```bash
docker exec higress-redis redis-cli GET 'higress-ai-budget:{acme}:tenant-daily-budget:3600'
```

## 验证计费扣减

`cases.sh` 最后两段会各发一次非流式与流式请求，然后打印计数键。要点：

- 流式必须带 `stream_options.include_usage`，否则上游不会补那一帧 usage，
  插件拿不到 token 数就**跳过扣减**（这是有意的：宁可漏记，不可乱记）；
- 扣减金额 = `round(in×price_in + out×price_out)`，本配置下 `mock-chat` 是
  100/百万 token，`mock-chat-backup` 是 10——降级前后的扣减速度差 10 倍，
  这就是插件省下的钱；
- 首次写入时才设 TTL，`TTL` 应等于 `budget_period`（3600）。

## 搭建过程中踩到的四个坑

这几条不写下来，重搭一次还会再踩一遍：

1. **`matchRules.ingress` 必须写裸 ingress 名**，不能写 `higress-system/budget-mock`。
   写成 `namespace/name` 匹配不上，插件对该路由**静默不生效**——
   VM 正常加载、`wasm.…crash: 0`、日志一行不出，看起来像"插件坏了"。
   内置 `ai-proxy` 用的也是裸名（`- qwen`）。判断依据：
   `docker exec higress-gw curl -s localhost:15000/stats | grep wasm`
   能看到 VM，但请求时 `grep ai-budget-router: gateway.log` 没有任何输出。

2. **配置下发要等 10~15 秒**。写完 `/data` 下的文件立刻发请求，打到的还是旧配置。
   `toggle.sh` 里固定 `sleep 15` 就是为此。

3. **插件日志默认全被吞掉**。Envoy 以 `-l warning` 启动，插件的 `Info`/`Debug`
   一行都看不到。先打开：

   ```bash
   docker exec higress-gw curl -s -X POST "http://127.0.0.1:15000/logging?wasm=debug"
   ```

4. **提示词别用中文**。git-bash 把中文传给 `curl -d` 会破坏编码，
   上游收到非法 JSON 直接回 400 `invalid json body`——
   和插件毫无关系，但排查时极易误判成"插件把 body 改坏了"。
   `lib.sh` 里的 `$PROMPT` 因此保持纯 ASCII。

## 一个已确认的缺陷

降级发生时，日志里必然伴随一条：

```
error … replace routing header x-higress-llm-model failed: bad argument
```

请求体阶段改不了请求头，这是 proxy-wasm 的阶段约束。**model 改写本身是成功的**
（上游确实收到降级后的模型），只有路由头没改成。详见
[docs/known-issues.md](../../docs/known-issues.md) §0——
本地这套测试路由不依赖该头，所以不影响上面的结论。

## 排查

```bash
docker logs --tail 100 higress-gw
```

```bash
docker exec higress-gw sh -c 'tail -50 /var/log/higress/controller.log'
```

```bash
docker logs --tail 30 higress-mock-llm
```

mock 的日志会打印它**实际收到**的 model（`[mock-llm] mock-chat-backup non-stream tokens=…`），
和响应回显互为印证。

| 症状 | 多半是 |
|---|---|
| 一直 404 | Ingress 没生效，或 Host 头没带 `budget.local` |
| 一直 503 | McpBridge 没注册成功 / 域名不是 FQDN，查 controller.log |
| 插件没反应 | `configDisable` 还是 true，或 wasm 路径不对；`docker logs higress-gw` 搜 `budget` |
| 水位读到 0 | 租户头 `x-tenant-id` 没带，插件解析不到租户会直接 bypass |
| 不扣减 | 响应里没有 usage —— 流式要带 `include_usage` |

## 清理

```bash
bash test/local/toggle.sh off
```

```bash
docker exec higress-gw sh -c 'rm -f /data/ingresses/budget-mock.yaml /data/wasmplugins/ai-budget-router-0.1.0.yaml'
```

McpBridge 里注册的 `redis` / `mock-llm` 两条留着无副作用，后续联调还要用。
