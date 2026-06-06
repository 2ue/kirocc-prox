# PostgreSQL + Redis 完全迁移审计

日期：2026-06-06

## 目标

本次审计按最严格口径执行：系统自身不再保留 SQLite / 本地 JSON 文件作为运行态或持久化后端，也不保留单账号 SQLite fallback。所有系统持久化进入 PostgreSQL，所有跨请求/跨进程调度状态进入 Redis。

允许保留的 JSON 仅限 HTTP 请求体导入格式，例如用户在管理后台粘贴或上传 `accounts.json`，服务端解析后写入 PostgreSQL。JSON 不再作为系统存储后端。

## 审计口径

必须删除或禁用的类型：

- SQLite 依赖和驱动。
- 读取 Kiro CLI 本地数据库的导入路径。
- 单账号 SQLite pool。
- JSON credentials 文件读写 store。
- JSON 文件刷新器。
- 文件型 settings backend。
- SQLite usage store。
- 旧启动 flag：`-db`、`-creds-json`、`-usage-db`、`-settings`。
- 旧子命令：`kirocc creds`。
- Admin 认证文件编辑 API / UI。

允许存在的类型：

- `redis-db` 里的 `db` 字样，这是 Redis database number，不是 SQLite。
- 旧环境变量名仅出现在拒绝清单中，作用是启动时报错，防止误以为旧配置仍然生效。

## 关键源码结论

| 模块 | 当前后端 | 证据文件 |
| --- | --- | --- |
| 账号与凭据 | PostgreSQL | `internal/storage/postgres.go`、`cmd/kirocc/main.go` |
| settings | PostgreSQL JSONB | `internal/settings/settings.go`、`cmd/kirocc/main.go` |
| usage records | PostgreSQL | `internal/usage/store_postgres.go`、`internal/usage/aggregator.go` |
| in-flight | Redis | `internal/pool/runtime_state.go`、`internal/pool/conductor_default.go` |
| cooldown | Redis | `internal/pool/runtime_state.go`、`internal/pool/scheduler_default.go` |
| session affinity | Redis | `internal/pool/runtime_state.go`、`internal/pool/conductor_default.go` |
| reservation lease | Redis | `internal/pool/runtime_state.go` |

## 已删除的旧运行路径

| 旧能力 | 处理结果 |
| --- | --- |
| `cmd/kirocc/creds_cmd.go` JSON 文件子命令 | 删除 |
| `internal/admin/handlers_credsfile.go` 认证文件编辑 API | 删除 |
| `internal/admin/handlers_local_kiro.go` 本机 Kiro 数据库导入 | 删除 |
| `internal/auth/db.go` Kiro CLI 本地数据库读取 | 删除 |
| `internal/auth/db_test.go` SQLite 读取测试 | 删除 |
| `internal/auth/refresh_test.go` 旧 AuthManager 测试 | 删除 |
| `internal/pool/single_account.go` 单账号 pool | 删除 |
| `internal/pool/store_json_io.go` credentials JSON 文件读写 | 删除 |
| `internal/pool/store_json_test.go` JSON 文件 store 测试 | 删除 |
| `internal/pool/refresh_test.go` JSONFileRefresher 测试 | 删除 |
| `internal/usage/store_sqlite.go` SQLite usage store | 删除 |
| `internal/usage/store_sqlite_test.go` SQLite usage 测试 | 删除 |
| `modernc.org/sqlite` 依赖 | `go mod tidy` 后移除 |

## 已改造的旧入口

| 入口 | 当前行为 |
| --- | --- |
| `-db` | flag 未定义，启动失败 |
| `-creds-json` | flag 未定义，启动失败 |
| `-usage-db` | flag 未定义，启动失败 |
| `-settings` | flag 未定义，启动失败 |
| `kirocc creds` | positional argument 被拒绝，启动失败 |
| `KIROCC_DB_PATH` | 启动时报错 |
| `KIROCC_CREDS_JSON` | 启动时报错 |
| `KIROCC_USAGE_DB` | 启动时报错 |
| `KIROCC_SETTINGS` | 启动时报错 |
| `/admin/credsfile` | `410 Gone`，明确提示账号存储在 PostgreSQL |
| `/admin/import/local-kiro` | 路由已移除 |
| Admin UI 认证文件页 | 已移除 |
| Admin UI 本机 Kiro 导入 | 已移除 |

## 文档一致性处理

用户要求“不要凭直觉，文档”，因此本次不仅审计代码，也同步了面向使用者的文档，避免文档继续指导使用旧本地持久化模式。

已更新：

| 文档 | 当前口径 |
| --- | --- |
| `README.md` | 以 PostgreSQL + Redis 为唯一架构，启动示例使用 `docker-compose.dev.yml` 和默认 `9326/3457` 端口 |
| `README.zh-CN.md` | 中文快速启动、路由、配置、调度和 Admin 说明全部改为 PostgreSQL + Redis |
| `README_3460_OPTIMIZED.md` | 改为 fork hardening 说明，不再包含旧部署路径 |
| `docs/admin-features.zh-CN.md` | Admin 功能矩阵改为 PostgreSQL 持久化、Redis 运行态 |
| `docs/scheduling-capacity-and-storage-evolution.zh-CN.md` | 改为迁移后的容量、风险与调度说明 |
| `docs/usage-cost-and-history-plan.zh-CN.md` | usage 权威存储改为 PostgreSQL，成本方案基于 PostgreSQL 快照 |
| `docs/prompt-cache-report-profiles.zh-CN.md` | settings 来源改为 PostgreSQL settings |
| `assets/architecture-light.svg` / `assets/architecture-dark.svg` | 架构图认证侧改为 PostgreSQL + Token Refresh |
| `web/dashboard/index.html` | 旧 DB Path 展示改为 PostgreSQL / Redis |

## Redis 调度修复

本次迁移不仅切换后端，还修复了旧单进程思路在 Redis 架构下的缺陷：

1. Redis reserve 成功后，如果本地计数更新失败，会释放刚拿到的 Redis reservation，避免 lease 泄漏。
2. Redis 模式下，本地 session affinity 命中也必须走 Redis reserve，不能绕过 Redis 的全局 in-flight 上限。
3. Redis 模式下先同步全部账号 in-flight，再执行 Ready / selector，避免本地陈旧计数导致账号永远不被重新同步。
4. Redis reserve 成功后，本地只更新展示计数，不再用陈旧本地计数重复拒绝。
5. 模型级 cooldown 写 Redis 时使用模型级 TTL，不再误用账号级 TTL。
6. MarkSuccess / MarkRateLimit 不在持有账号锁时调用 Redis，降低并发锁等待。
7. 默认 `redis-lease-ttl` 调整为 `30m`，降低长流式请求中 reservation 过早过期导致超卖并发的风险。
8. Acquire 成功后为 Redis reservation 增加续租 goroutine，长 SSE 请求会持续延长 reservation 和 in-flight key TTL；Release 时取消续租并释放计数，避免 lease 到期后 in-flight 计数在持续流量下偏移。

## 审计命令

源码与依赖命中检查：

```bash
rg -n "SQLite|sqlite|modernc\\.org/sqlite|DefaultDBPath|db_path|OpenDB|ReadCredentials|local-kiro|LocalKiro|localKiro|btnLocalKiro|kiro-cli/data\\.sqlite3|KIROCC_DB_PATH|-db\\b|usage\\.sqlite|-usage-db|credentials\\.json|-creds-json|AuthManager|NewAuthManager|NewSingleAccount|LoadFromJSON|SaveToJSON|JSONFileRefresher|settings\\.json|-settings\\b|KIROCC_SETTINGS|KIROCC_CREDS_JSON|KIROCC_USAGE_DB" cmd internal go.mod go.sum -g'*.go' -g'*.js' -g'*.html' -g'go.mod' -g'go.sum'

go list -m all | rg "sqlite|modernc"
```

文档与示例命中检查：

```bash
rg -n "settings\\.json|credentials\\.json|usage\\.sqlite|Kiro CLI|本地数据库|认证文件|DB Path|AuthManager|SQLite|sqlite" README.md README.zh-CN.md README_3460_OPTIMIZED.md docs web assets -g'*.md' -g'*.html' -g'*.svg'
```

预期结果：

- 不应再出现 SQLite 依赖或 SQLite 运行代码。
- 允许出现 `KIROCC_DB_PATH`、`KIROCC_CREDS_JSON`、`KIROCC_USAGE_DB`、`KIROCC_SETTINGS`，但只能出现在 `internal/config/config.go` 的拒绝清单中。
- 允许出现 `redis-db`，这是 Redis database number。
- 允许审计文档自身出现旧关键词，因为它们用于说明“删除了什么、拒绝了什么”。
- 普通 README、Admin 功能文档、容量分析文档和静态 dashboard 不应继续把旧路径描述为可用配置。

功能验证：

```bash
node --check internal/admin/html/app.js
GOEXPERIMENT=jsonv2 go test ./...
GOEXPERIMENT=jsonv2 go test -run '^$' -tags e2e ./internal/e2e
docker compose -f docker-compose.dev.yml ps
curl -fsS http://127.0.0.1:3457/admin/health
curl -fsS http://127.0.0.1:3457/admin/settings
curl -sS -o /tmp/kirocc-credsfile.out -w '%{http_code}' http://127.0.0.1:3457/admin/credsfile
docker exec kirocc-pro-postgres-dev psql -U kirocc -d kirocc_pro -tAc "SELECT count(*) FROM accounts;"
docker exec kirocc-pro-redis-dev redis-cli PING
```

## 当前运行态验收标准

服务启动后必须满足：

- `/admin/health` 返回 `multi_account=true`。
- `/admin/settings` 的 `server.account_store` 为 `postgresql`。
- `/admin/settings` 的 `server.runtime_store` 为 `redis`。
- PostgreSQL `accounts` 表存在且账号数与后台一致。
- Redis `PING` 返回 `PONG`。
- 日志包含 `pool: postgres+redis mode`。
- `/admin/credsfile` 返回 `410 Gone`。

## 结论

按本审计口径，系统自身存储与调度架构必须完全由 PostgreSQL + Redis 承载；任何 SQLite / JSON 文件存储 fallback 都不允许保留。已删除所有 SQLite 运行代码、SQLite 依赖、本机数据库导入路径、JSON 文件存储路径和单账号 fallback。后续若新增账号导入能力，只能作为请求输入解析，最终必须写入 PostgreSQL。
