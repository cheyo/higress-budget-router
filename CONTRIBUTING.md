# Contributing

## 开发环境

- Go 1.24+（`GOOS=wasip1 GOARCH=wasm` 编译目标）
- 不需要 TinyGo —— Higress 从 2.0 起用标准 Go 的 wasip1 后端

```bash
git clone https://github.com/cheyo/higress-budget-router.git
cd higress-budget-router
make build          # go test + vet + wasm 编译
```

## 常用命令

| 命令 | 作用 |
|---|---|
| `make test` | 跑单元测试 |
| `make vet` | `GOOS=wasip1` 下的 go vet |
| `make wasm` | 编译出 `main.wasm` |
| `make build` | 上面三个都跑一遍 |
| `make image` | 打成 OCI 镜像 |
| `make push` | 推镜像（需 `REGISTRY=` `VERSION=`） |

## 代码约定

**日志级别**：
- `Debugf` —— 正常流程的分支说明（跳过、命中档位、扣减明细）。高频路径一律用这个。
- `Infof` —— 实际发生了降级、dry-run 的决策结果
- `Warnf` —— 配置疑似有问题（租户取不到）、预算耗尽拒绝
- `Errorf` —— Redis 出错、body 改写失败等需要告警的情况

**错误处理的默认方向是 fail-open**。这个插件跑在所有流量的关键路径上，降级失败的后果是多花点钱，而插件自身出错导致请求中断的后果是业务故障。除非有明确理由，新增分支时优先放行。

**特别注意类型断言**。`ctx.GetContext` 返回 `interface{}`，断言失败会拿到零值。`remain` 的零值 `0.0` 恰好意味着「预算耗尽」——所以 L177 那样的 `ok` 检查不能省，否则「插件出点小问题」会变成「全量 429」。新增从 ctx 取值的代码时请照此办理。

**改动观测字段要谨慎**。`budget_*` 系列属性一旦上了看板，下游的查询、告警规则都会依赖字段名。改名等同于破坏性变更，请在 CHANGELOG 里标注。

## 提交前

```bash
go mod tidy
make build
```

CI 会检查 `go.mod` 是否 tidy、测试是否通过、`GOOS=wasip1` 下 vet 是否干净、wasm 能否编译，以及 golangci-lint。

## 提交 PR

- 一个 PR 做一件事
- 涉及行为变更的，同步更新 `README.md` / `README_zh-CN.md` 的配置表和 `CHANGELOG.md`
- 修复 `docs/known-issues.md` 里的条目时，请在 PR 描述里引用编号，并从该文件中移除对应条目

## 报 Bug

请附上：Higress 版本、插件配置（脱敏后）、相关的网关日志（`ai-budget-router:` 开头的行），以及 Redis 里对应 key 的实际值（`GET` + `TTL`）。
