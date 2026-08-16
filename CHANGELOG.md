# Changelog

本项目遵循 [Keep a Changelog](https://keepachangelog.com/zh-CN/1.1.0/) 与 [语义化版本](https://semver.org/lang/zh-CN/)。

## [Unreleased]

计划中（详见 [docs/known-issues.md](docs/known-issues.md)）：

- 修复非 JSON 请求体不计费的问题（拆分 `skip` / `noRewrite` 语义）
- 拒绝路径补写访问日志属性
- 把 `budget_billed_mode` 更名为 `budget_billed_model`
- 补完或删除 `x-higress-budget-remaining` / `x-higress-model-degraded` 响应头
- 可选：VM 内水位本地缓存 + `RegisterTickFunc` 异步刷新，降低每请求 Redis 往返

## [0.1.0] - 2026-08-16

首个版本。

### 新增

- **请求阶段主动降级**：按租户实时预算水位改写请求体 `model` 字段与 `x-higress-llm-model` 路由头
- **多级降级阶梯**：`degrade_levels` 按剩余预算比例配置，支持「只打标」「改模型」「直接拒绝」三种档位行为
- **响应阶段精确计费**：复用 SDK 的 `tokenusage` 解析 SSE 流式 usage，支持 OpenAI / Anthropic / Gemini / 豆包等多家协议
- **Redis 原子扣减**：Lua 脚本实现 `INCRBY` + 首次 `EXPIRE`，预算按固定窗口滚动重置；键带 hash tag 兼容 Redis Cluster
- **微单位整数记账**：单价按每百万 Token 配置，成本天然落在整数微单位上，规避 `INCRBYFLOAT` 精度漂移
- **五种租户来源**：`consumer` / `header` / `param` / `cookie` / `fixed`
- **三道防误伤**：同模型不改写、不降级到更贵的模型、`dry_run` 灰度开关
- **fail-open / fail-closed 可配**：Redis 异常时的行为由 `fail_open` 控制，默认可用性优先
- **8 个访问日志属性**：支持按租户统计降级率、档位分布与成本节省

### 说明

- 插件 `priority` 必须配 **800**（Higress 中值越大越先执行），大于 `ai-quota`(750) / `ai-token-ratelimit`(600) / `ai-proxy`(100)
- 请求阶段**只读不预扣**，扣减在响应阶段完成，影响下一次请求的路由决策
