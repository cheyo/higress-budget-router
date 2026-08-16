---
name: Bug report
about: 插件行为不符合预期
title: ''
labels: bug
assignees: ''
---

## 现象

<!-- 期望什么、实际发生了什么 -->

## 环境

- Higress 版本：
- 插件版本 / 镜像 tag：
- Redis 版本 / 是否 Cluster：

## 插件配置

<!-- WasmPlugin 的 config 部分，请脱敏 password 等敏感字段 -->

```yaml

```

## 网关日志

<!-- 过滤 "ai-budget-router:" 开头的行。建议把日志级别调到 debug -->

```

```

## Redis 现场

<!-- 对应租户的 key，用 GET 和 TTL 各查一次 -->

```
GET  higress-ai-budget:{<tenant>}:<rule_name>:<period>  →
TTL  higress-ai-budget:{<tenant>}:<rule_name>:<period>  →
```

## 排查清单

在提交前请先确认（这几项占了绝大多数「插件不生效」的报告）：

- [ ] `priority` 大于 750（Higress 中**值越大越先执行**，配小了插件会在 `ai-proxy` 之后执行，静默不生效）
- [ ] `phase` 是 `UNSPECIFIED_PHASE`
- [ ] `dry_run` 不是 `true`
- [ ] 请求路径命中了 `enable_on_path_suffix`
- [ ] `tenant_source` 能取到值（`type: consumer` 需要 AUTHN 阶段有 key-auth）
- [ ] 请求 `content-type` 是 `application/json`
