# kirocc-pro Admin 功能文档

日期：2026-06-06

## 当前架构口径

Admin UI 和 Admin API 当前以 PostgreSQL + Redis 为唯一运行架构：

| 数据或状态 | 后端 |
|---|---|
| 账号、OAuth token、proxy_url、region、max_in_flight | PostgreSQL |
| quota 快照 | PostgreSQL |
| API key、网络偏好、缓存上报 profile、优化 knob | PostgreSQL |
| 请求记录和 token 用量历史 | PostgreSQL |
| session affinity、in-flight、cooldown、reservation lease | Redis |

JSON 只作为“文件导入”或 HTTP 请求体格式存在。导入成功后，权威数据在 PostgreSQL。

## 启动

本地开发先启动依赖：

```bash
docker compose -f docker-compose.dev.yml up -d
```

启动服务：

```bash
GOEXPERIMENT=jsonv2 go run ./cmd/kirocc \
  -pool-strategy least-inflight \
  -admin \
  -admin-port 3457 \
  -port 9326
```

打开：

```text
http://127.0.0.1:3457/admin/
```

## 功能矩阵

| 功能 | 入口 | 持久化或运行态 |
|---|---|---|
| 账号添加、编辑、删除 | 账号页 | PostgreSQL |
| 文件导入 | 账号页 | 导入请求体解析后写 PostgreSQL |
| OAuth 添加账号 | 账号页 | PostgreSQL |
| 强刷 quota | 账号页 | 队列刷新，结果写 PostgreSQL |
| 每账号代理 `proxy_url` | 账号表单和详情 | PostgreSQL |
| 单账号并发上限 `max_in_flight` | 账号表单和详情 | PostgreSQL 配置，Redis 执行 |
| in-flight 展示 | 账号页、仪表盘 | Redis |
| cooldown 展示 | 账号页、仪表盘 | Redis + 本地快照 |
| API key 管理 | 系统设置 -> 认证密钥 | PostgreSQL |
| 网络偏好 | 系统设置 -> 网络 | PostgreSQL |
| 缓存上报 profile | 系统设置 -> 缓存上报 | PostgreSQL |
| kirocc 优化 knob | 系统设置 -> kirocc 优化 | PostgreSQL |
| 请求记录 | 使用记录 | PostgreSQL |

## 系统设置二级 Tab

| Tab | 作用 |
|---|---|
| 运行时 | 展示当前代理端口、Admin 端口、PostgreSQL/Redis 状态、GeoIP 状态 |
| 认证密钥 | 创建、轮换、禁用动态 API key |
| 远程访问 | 展示 Admin URL、认证方式和远程调用注意事项 |
| 系统 | 通用运行参数 |
| 网络 | preferred_region、GeoIP 相关配置 |
| 流式传输 | SSE 和响应行为相关开关 |
| 缓存上报 | 路径到 profile 的映射、profile 表单、JSON 编辑 |
| kirocc 优化 | thinking budget、模型映射、实验 knob |
| 远程 API | curl / Python 示例 |

## 账号管理

### 添加账号

支持两条路径：

1. OAuth：后台发起登录流程，回调后写入 PostgreSQL。
2. 文件导入：上传或粘贴 JSON / JSONL。后端支持常见 camelCase / snake_case 字段，统一归一化后校验，校验通过才写入 PostgreSQL。

导入示例：

```bash
curl -X POST \
  -H "Authorization: Bearer $ADMIN_KEY" \
  -H "X-Requested-With: XMLHttpRequest" \
  -H "Content-Type: application/json" \
  --data-binary @accounts.json \
  http://127.0.0.1:3457/admin/accounts/import
```

### 修改账号

常见字段：

| 字段 | 说明 |
|---|---|
| `label` | 运维展示名称 |
| `priority` | 调度优先级 |
| `disabled` | 是否手动禁用 |
| `disable_cooling` | 是否跳过 cooldown |
| `proxy_url` | 认证、刷新、quota 查询使用的每账号代理 |
| `max_in_flight` | 单账号并发上限，0 表示不限 |

后台保存时会先更新 PostgreSQL，失败则回滚调度器内存态，避免 UI 显示与权威存储分裂。

## 使用记录

每个 `/v1/messages` 和 `/api/<route>/v1/messages` 请求都会写入 `usage_records`。

请求记录应包含：

| 字段 | 说明 |
|---|---|
| `path` | 实际请求路径 |
| `credential_id` | 调度命中的账号 |
| `api_key_id` | 匹配的动态 API key |
| `device_id` | 设备指纹 |
| `requested_model` | 客户端请求模型 |
| `resolved_model` | 实际路由模型 |
| `input_tokens` | 可见输入 token |
| `output_tokens` | 输出 token |
| `cache_read_tokens` | 缓存读取 token |
| `cache_write_tokens` | 缓存创建 token |
| `status` | `success`、`rate_limited`、`auth_error`、`upstream_error` 等 |
| `error` | 错误详情，表格中折叠展示，点击可看完整内容 |
| `first_token_ms` | 首字 token 延迟 |
| `latency_ms` | 总耗时 |
| `trace_id` | 排障关联 ID |

错误请求也必须记录。状态栏只展示状态，不把错误信息塞进状态列。

## 缓存上报

缓存上报模块是下游 usage 模拟模块，不修改上游 Kiro payload。

默认路径：

| 路径 | Profile |
|---|---|
| `/v1/messages` | `default` |
| `/api/cc` | `cc` |
| `/api/ha` | `ha` |
| `/api/na` | `na` |

自定义路由统一加 `/api` 前缀。例如配置自定义路由名 `<name>`，控制台展示和实际生效都是 `/api/<name>`。标准 `/v1` 不加前缀。

Profile 可独立控制：

- input 上报策略。
- output 上报策略。
- cache read 上报策略。
- cache creation 上报策略。
- 是否启用本地 cache fingerprint 计算。
- 采样目标、采样上限和边界 jitter。

详细算法见 [`prompt-cache-report-profiles.zh-CN.md`](./prompt-cache-report-profiles.zh-CN.md)。

## 远程 API

变更类请求必须带：

```text
Authorization: Bearer <admin-key>
X-Requested-With: XMLHttpRequest
```

高频端点：

```text
GET    /admin/accounts
POST   /admin/accounts
POST   /admin/accounts/import
PATCH  /admin/accounts/{id}
DELETE /admin/accounts/{id}
POST   /admin/accounts/{id}/refresh
POST   /admin/accounts/{id}/disable
POST   /admin/accounts/{id}/enable

GET    /admin/api-keys
POST   /admin/api-keys
PATCH  /admin/api-keys/{id}
POST   /admin/api-keys/{id}/rotate
DELETE /admin/api-keys/{id}

GET    /admin/usage/recent
GET    /admin/usage?window=24h&group=model
GET    /admin/usage?window=24h&group=api_key
GET    /admin/usage?window=24h&group=device
GET    /admin/usage?window=24h&group=cred

GET    /admin/settings
PUT    /admin/settings
```

## 安全注意事项

- `admin-key` 同时是 Web 登录凭据和 Bearer token，不能放进不受控日志。
- Admin 端口默认只绑定 `127.0.0.1`；非本机访问必须使用 TLS、强 key 和防火墙。
- `KIROCC_AUDIT_LOG` 会写完整 prompt/response，只用于排障。
- 创建或轮换 API key 时，明文只展示一次。
- 账号详情属于管理员接口，不应暴露给普通使用者。

## 验收

```bash
curl -fsS http://127.0.0.1:3457/admin/health
curl -fsS http://127.0.0.1:3457/admin/settings
curl -sS -o /tmp/kirocc-credsfile.out -w '%{http_code}' \
  http://127.0.0.1:3457/admin/credsfile
```

预期：

- `/admin/settings` 返回 `server.account_store=postgresql`。
- `/admin/settings` 返回 `server.runtime_store=redis`。
- `/admin/credsfile` 返回 `410`。
