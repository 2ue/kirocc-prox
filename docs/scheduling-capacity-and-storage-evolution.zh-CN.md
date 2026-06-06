# PostgreSQL + Redis 架构下的调度、容量与风险分析

日期：2026-06-06

## 目的

本文记录当前全量改造后的容量判断和运行风险。旧的本地文件持久化方案已经不是当前项目口径；当前项目的权威口径是：

- PostgreSQL 保存账号、凭据、settings、API key、quota 快照和 usage 记录。
- Redis 保存跨请求运行态：in-flight、cooldown、session affinity、reservation lease。
- 进程内结构只作为快速快照和短期查询缓存，不能作为权威存储。

完整迁移证据见 [`pgredis-complete-migration-audit.zh-CN.md`](./pgredis-complete-migration-audit.zh-CN.md)。

## 目标规模

| 项目 | 目标 |
|---|---|
| 账号数量 | 几十到约 100 个 |
| 请求速率 | 约 300 RPM |
| 机器规格 | 2C4G 或 4C8G |
| 请求形态 | Claude Code 长会话、SSE、tools、MCP、web search |
| 部署依赖 | PostgreSQL + Redis |

RPM 不是并发。并发取决于平均请求耗时：

```text
并发数 ~= RPM * 平均请求持续秒数 / 60
```

在 300 RPM 下：

| 平均请求耗时 | 估算并发 |
|---:|---:|
| 10s | 50 |
| 20s | 100 |
| 60s | 300 |
| 120s | 600 |

长流式请求越多，Redis in-flight 和上游连接压力越接近真正瓶颈。

## 当前模块职责

| 模块 | 职责 | 当前后端 |
|---|---|---|
| `internal/storage/postgres.go` | schema、账号、凭据、quota、settings、usage 表 | PostgreSQL |
| `internal/settings/settings.go` | 运行时可变设置 | PostgreSQL |
| `internal/usage/store_postgres.go` | usage append log 和聚合查询 | PostgreSQL |
| `internal/pool/runtime_state.go` | affinity、reservation、in-flight、cooldown | Redis |
| `internal/pool/conductor_default.go` | 获取账号、Redis reserve、释放 reservation | Redis + 本地快照 |
| `internal/pool/scheduler_default.go` | 账号快照、状态展示、cooldown 镜像 | 本地快照 + Redis |
| `internal/quota/poller_impl.go` | quota 周期刷新 | PostgreSQL 写入 |

## 调度策略

| 策略 | 行为 | 建议 |
|---|---|---|
| `round-robin` | 同优先级轮询 | 账号能力接近时可用 |
| `fill-first` | 优先消耗高优先级账号 | 只适合明确要按 tier 消耗 |
| `least-used` | 选择成功次数最少的账号 | 适合长期均衡，不直接感知当前并发 |
| `least-inflight` | 选择当前运行请求最少的账号 | 300 RPM 推荐 |
| `weighted-least-inflight` | 选择 in-flight / max_in_flight 比例最低的账号 | 混合账号容量时推荐 |

当前 Redis reserve 是关键边界：即使多个进程同时选择同一账号，也必须通过 Redis 原子脚本增加 reservation，不能只靠本地计数。

## 关键运行态解释

### in-flight 感知

in-flight 是“正在使用某账号请求上游但还没结束”的数量。它不是历史请求数，也不是 RPM。

当前由 Redis 保存账号级和模型级计数，并通过 reservation lease 防止进程异常造成永久占用。

### per-account 并发上限

`max_in_flight` 是账号级上限。Redis reserve 时会先检查当前账号 in-flight，达到上限则拒绝该账号，调度器继续寻找其他 ready 账号。

这比单进程内存计数更科学，因为多个进程共享同一 Redis 状态。

### least-inflight

least-inflight 选择当前运行请求更少的账号，避免某个账号被长流式请求拖住后还继续分配新请求。

对 300 RPM 和长会话场景，这是比普通轮询更稳的默认策略。

### cooldown

cooldown 是账号或模型暂时不可用的 TTL 状态。来源包括 429、结构化 rate limit、认证错误或显式禁用。

当前 cooldown 镜像到 Redis。模型级 cooldown 使用模型级 TTL，账号级 cooldown 使用账号级 TTL，避免一个模型限流误伤所有模型。

### quota poll 降频

quota poll 是后台周期性调用 Kiro `getUsageLimits`。账号多时不能无脑高频刷新，否则会制造额外上游压力。

当前默认间隔是 3 分钟，并且刷新全部账号时应排队限并发，不做全部账号同时并发刷新。

## 内存判断

在 100 个账号规模下，账号元数据本身不是主要内存压力。主要风险来自：

| 来源 | 风险 |
|---|---|
| 长流式请求 | 并发连接、响应缓冲、上游等待时间 |
| 大请求体 | request body 捕获、日志、OTel body event |
| usage memory ring | 默认 10000 条，通常可控 |
| dashboard 近期数据 | 只保留短窗口，通常可控 |
| prompt-cache tracker | 保存 fingerprint，不保存完整正文 |

2C4G 能跑，但需要控制日志体积、OTel body capture 和单账号并发。4C8G 更适合真实 300 RPM 长会话。

## 已优化点

| 优化 | 目的 |
|---|---|
| Redis reserve 成功后本地失败会 rollback | 避免 reservation 泄漏 |
| affinity 命中也必须 Redis reserve | 防止绕过全局并发上限 |
| reserve 前同步全部账号 in-flight | 避免本地陈旧状态影响选择 |
| reserve 成功后不再用本地旧计数二次拒绝 | 避免误拒 |
| Redis 调用移出账号锁 | 降低锁等待 |
| 默认 lease TTL 30 分钟 | 适配长流式请求 |
| quota 刷新排队限并发 | 避免全部账号同时打上游 |

## 仍需关注的风险

| 风险 | 说明 | 建议 |
|---|---|---|
| 超长 SSE 超过 lease TTL | lease 过期后可能释放正在运行的容量 | 观察 P95/P99 请求耗时，必要时提高 `-redis-lease-ttl` |
| Redis 不可用 | 无法可靠执行全局 reserve | 当前应启动失败或返回错误，不应降级到本地模式 |
| PostgreSQL 不可用 | 无法加载账号或写 usage | 当前应启动失败或请求记录写入报错 |
| 大 payload | 上游可能返回 400，本地也会占用内存 | 增加请求体大小限制和可观测错误透传 |
| 全量 quota 刷新 | 100 账号同时刷新会打爆上游 | 必须排队限并发 |
| 多实例 refresh 同一账号 | 可能重复刷新 token | 后续可补 Redis 分布式 refresh lock |
| usage 写入延迟 | 高并发时写库积压 | 监控写入错误和队列长度，必要时批量写入 |

## 结论

全量 PostgreSQL + Redis 架构比本地文件方案更适合 100 账号、300 RPM、长会话和多实例演进。当前必须保持“无降级、无兼容 fallback”的原则：依赖不可用就显式失败，不能悄悄回到本地内存或本地文件模式。

推荐默认启动：

```bash
GOEXPERIMENT=jsonv2 go run ./cmd/kirocc \
  -pool-strategy least-inflight \
  -redis-lease-ttl 30m
```

混合账号容量时使用：

```bash
GOEXPERIMENT=jsonv2 go run ./cmd/kirocc \
  -pool-strategy weighted-least-inflight
```
