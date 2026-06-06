# 请求记录、缓存账本与成本统计方案

日期：2026-06-06

## 目的

本文明确请求记录、缓存 usage 和后续成本统计的边界：

1. 请求记录必须能看到请求路径、账号、API key、设备、模型、输入输出 tokens、缓存读写 tokens、状态、错误信息和首字 token 延迟。
2. usage 历史以 PostgreSQL 为权威存储，内存 ring 只服务近期查询加速。
3. 后续成本统计应保存请求发生时的价格快照，不能只在前端用当前价格临时回算历史。

## 当前 usage 账本

后端请求结束时会发布 `usage.Record`，并写入：

- 内存 ring：用于后台近期记录快速展示。
- PostgreSQL `usage_records`：用于历史查询、聚合和排障。

记录字段包括：

| 字段 | 说明 |
|---|---|
| `path` | 实际请求路径 |
| `credential_id` | 调度命中的账号 |
| `api_key_id` | 下游动态 API key |
| `device_id` / `device` | 设备指纹和 User-Agent 摘要 |
| `requested_model` / `resolved_model` | 请求模型和实际路由模型 |
| `input_tokens` / `output_tokens` | 普通输入输出 tokens |
| `cache_read_tokens` / `cache_write_tokens` | 下游 usage 中的缓存读写 tokens |
| `status` | 成功、限流、认证错误、上游错误等 |
| `error` | 错误详情 |
| `first_token_ms` | 首字 token 延迟 |
| `latency_ms` | 总耗时 |
| `trace_id` | 排障关联 ID |

错误请求也要写入 usage，只要请求已经进入代理处理流程。这样后台才能看到失败路径和上游错误原因。

## 已落地修复

| 项目 | 当前要求 |
|---|---|
| 账号列 | 请求记录展示真实账号标识，不做无意义拼接 |
| 缓存列 | 展示 cache read 和 cache creation |
| 路径列 | 展示实际请求 path |
| 错误列 | 独立列展示，限制列宽，点击查看完整内容 |
| 状态列 | 只展示状态，不混入错误正文 |
| 首字 token | 使用 `first_token_ms` 字段 |
| 错误请求 | 正常写入 PostgreSQL usage_records |

## 成本统计建议

成本统计不应只在前端计算，原因：

- 历史价格会变化，用当前价格回算会导致历史成本不稳定。
- 聚合维度会跨模型，缺少逐请求成本快照就无法准确计价。
- cache creation 未来可能区分不同 TTL 价格，当前 token 汇总不足以重算所有细节。
- Kiro credits 和 Claude API USD 价格不是同一套账单体系，不能混成真实账单。

建议新增独立 `internal/pricing` 模块：

```json
{
  "pricing": {
    "enabled": true,
    "currency": "USD",
    "source": "builtin",
    "remote_url": "",
    "version": "2026-06-06",
    "last_synced_at": "",
    "unknown_model_policy": "zero",
    "models": {}
  }
}
```

建议以微美元整数入库：

- `cost_input_micros`
- `cost_output_micros`
- `cost_cache_read_micros`
- `cost_cache_write_micros`
- `cost_total_micros`
- `pricing_model`
- `pricing_version`
- `pricing_source`

## 价格来源

建议支持三层来源：

| 来源 | 说明 |
|---|---|
| 内置默认价格 | 覆盖当前内置模型映射，启动即可估算 |
| PostgreSQL settings 覆盖 | Admin UI 提供表单和 JSON 编辑 |
| 远程同步 | 从固定 schema 的价格 JSON 拉取，失败不影响代理请求 |

不建议爬网页 HTML 获取价格。稳定做法是使用固定 schema 的 JSON 数据源。

## 展示建议

| 页面 | 建议展示 |
|---|---|
| 请求记录 | 单请求 estimated USD |
| 按模型统计 | tokens、cache tokens、estimated USD |
| 按 API key 统计 | 团队分摊 |
| 按设备统计 | 定位异常设备 |
| 账号详情 | Kiro credits + 近 24h / 7d estimated USD |

账号页面必须区分：

- Kiro credits：上游 quota/订阅视角。
- estimated USD：本地按价格表和 response usage 估算。

## 实施计划

| 阶段 | 内容 |
|---|---|
| P0 | 请求记录补齐账号、缓存、路径、错误、首字 token |
| P1 | 新增 pricing 模块和 PostgreSQL 成本快照字段 |
| P2 | Admin UI 增加价格/成本二级 tab |
| P3 | 增加价格同步 API 和同步状态 |
| P4 | 评估美元预算限制 |

P1 之后，请求结束时必须用当时生效的价格写入成本快照。后续价格变化不应改写历史请求成本。
