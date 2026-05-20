<!--
  kirocc-pro · admin UI 新功能图谱
  ──────────────────────────────────
  这份文档是给已经会启动 kirocc 的人看的。基础 8-fix 协议加固在
  README_3460_OPTIMIZED.md，本文只讲 admin / 运维相关的扩展能力。
-->

```
   ╔═══════════════════════════════════════════════════════════════╗
   ║   _    _                                                       ║
   ║  | | _(_) _ __ ___    ___    ___      _ __  _ __ ___           ║
   ║  | |/ / || '__/ _ \  / __|  / __|    | '_ \| '__/ _ \          ║
   ║  |   <| || | | (_) || (__  | (__  _  | |_) | | | (_) |         ║
   ║  |_|\_\_||_|  \___/  \___|  \___|(_) | .__/|_|  \___/          ║
   ║                                      |_|                       ║
   ║                                                                ║
   ║         admin UI · feature codex · v2026.05                    ║
   ╚═══════════════════════════════════════════════════════════════╝
```

> **30 秒摘要。** 你有一台 kirocc 在跑。这份文档教你怎么把它从「孤狼模式（一个号一个 token）」升级成「可视化舰队（几十个号、不同代理、按地区路由、按 key/设备分账、远程自动化）」。所有东西都在 `http://127.0.0.1:3457/admin/` 那个赛博绿底界面里，下面解释它每一面都管什么。

---

## 一眼图谱 · feature matrix

| 能力 | 谁用得上 | 怎么开 | 持久化在 |
|---|---|---|---|
| **多 API 密钥**（过期 + 额度） | 给团队发 key 不想一并暴露所有号 | 系统设置 → 认证密钥 → 添加 | `settings.json` |
| **用量归属**（key / device / model） | 想知道是谁烧的钱 | 自动 | `usage.sqlite` |
| **Token 曲线**（24h/7d/15d/30d） | 看趋势找尖峰 | 自动 | （从 SQLite 查）|
| **每账号代理**（proxy_url） | 几十个号怕同 IP 触发风控 | 添加账号时填 / PATCH | `credentials.json` |
| **静态偏好地区** | 你大概知道用户在哪 | 系统设置 → 网络 → 偏好地区 | `settings.json` |
| **GeoIP 自动路由** | 客户端散布全球，想就近选号 | `-geoip-mmdb /path/to/file` | MMDB 文件（外挂）|
| **kirocc 优化 knob** | 想调 thinking budget 但不想重启 | 系统设置 → kirocc 优化 → 保存 | `settings.json` |
| **远程 API 自动化** | 不想登 UI，写脚本批量加号 | 任何 curl/SDK | — |

---

## 90 秒试一遍

```bash
# 1. 跑起来（最小配置 + 全功能）
./dist/kirocc                                         \
  -creds-json   ~/.config/kirocc/credentials.json     \
  -admin                                              \
  -admin-port   3457                                  \
  -admin-key    "$(openssl rand -hex 16 | tee -a /tmp/key.txt)"  \
  -port         9326                                  \
  -settings     ~/.config/kirocc/settings.json        \
  -usage-db     ~/.config/kirocc/usage.sqlite

# 2. 打开 http://127.0.0.1:3457/admin/
#    密钥粘贴 /tmp/key.txt 里那串

# 3. 顺一遍五个二级菜单：
#    系统设置 → 运行时         看绑定的端口 / GeoIP 状态
#    系统设置 → 认证密钥        加一个 30d 过期 + 1M tokens 限额的 key
#    系统设置 → 网络            填 preferred_region = us-east-1
#    系统设置 → kirocc 优化     填 force_thinking_budget = 80000
#    系统设置 → 远程 API        复制 curl 速查
```

---

## ─── 故事 01 ───────────────────────────────────────────

> **「我有 50 个号被风控了。」**
> 30 个号挂在同一台服务器、同一个出口 IP 调 Kiro 配额接口，半天后全部 403。

### 解决方案：每账号绑独立代理

每个 `pool.CredentialFile` 加 `proxy_url` 字段。代理只作用在**鉴权面 HTTP**——也就是 token 刷新、OAuth token-exchange、`getUsageLimits` 配额查询。`/v1/messages` 本身走 kiroclient 全局连接（暂未走 per-cred；如需要告诉我）。

```
   credential A ────► proxy X ──► auth.kiro.dev
                                  q.us-east-1.amazonaws.com

   credential B ────► proxy X ──► （共享 client 池，复用连接）

   credential C ────► proxy Y ──►

   credential D ────► (空，走默认 HTTPS_PROXY 或直连)
```

**支持协议**：`http` · `https` · `socks5` · `socks5h`
**多账号共享**：同一 `proxy_url` 字符串的 client 自动池化，不会建多套 HTTP/2 连接。

```bash
# 给已有账号绑代理
curl -X PATCH \
  -H "Authorization: Bearer $KEY"   \
  -H "X-Requested-With: XMLHttpRequest" \
  -H "Content-Type: application/json"   \
  -d '{"proxy_url": "socks5://192.168.1.100:1080"}' \
  http://127.0.0.1:3457/admin/accounts/kiro-alice
```

```
╭─ TIP ──────────────────────────────────────────────────────────╮
│  OAuth 加号的时候就在 modal 里填代理 URL，出生就是干净的。    │
│  比加完号再 PATCH 改要稳——避免那一次 token-exchange 暴露 IP。 │
╰────────────────────────────────────────────────────────────────╯
```

---

## ─── 故事 02 ───────────────────────────────────────────

> **「美东用户为啥每次都被分到欧洲账号？延迟翻倍。」**
> 池里 us-east-1 / us-west-2 / eu-west-1 都有账号，但 round-robin 不知道客户端在哪。

### 解决方案：地区路由（B1 静态 + B2 GeoIP）

#### 决策链一图

```
   ┌──────────┐
   │ request  │
   └────┬─────┘
        │
        ▼
   GeoIP 加载了？──── no ────┐
        │                    │
       yes                   ▼
        ▼            preferred_region 设了？─── no ──┐
   client IP                  │                       │
   ↓                         yes                      ▼
   IP→国家码→AWS region        ▼                  无 hint
        │                  用这个                  ↓
        ▼                      │              原 strategy
   region "us-east-1"          │             (round-robin)
        │                      │
        └────────┬─────────────┘
                 ▼
        Conductor.Acquire(ctx, ...)
           ↓
        FilterByRegion(ready, hint) 两阶兜底：
           ① 精确匹配（cred.Region == "us-east-1"）
           ② 大洲前缀（"us-*" 全收，west-2 也算）
           ③ 还是空？不过滤，走全集
           ↓
        Selector.Pick(filtered, model)
```

#### B1 · 静态 preferred_region

最简单的开法。在 UI **系统设置 → 网络 → 偏好地区** 填 `us-east-1`，所有客户端都按这个走。适合「我大部分用户在哪里我心里有数」场景。

#### B2 · GeoIP 自动路由

```
   操作步骤
   ─────────
   ① https://www.maxmind.com/en/geolite2/signup   注册账号
   ② 复制 license key
   ③ 下载 GeoLite2-Country.mmdb（约 6MB）
   ④ 启动加 -geoip-mmdb /path/to/GeoLite2-Country.mmdb
   ⑤ UI「运行时」会显示 ✓ GeoLite2-Country · 构建于 X 天前
```

**国家 → 区域映射表**

```
   ┌────────────────────────┬──────────────────┐
   │ 北美                   │ us-east-1        │
   │   US · CA · MX         │                  │
   ├────────────────────────┼──────────────────┤
   │ 南美                   │ sa-east-1        │
   │   BR AR CL CO PE ...   │                  │
   ├────────────────────────┼──────────────────┤
   │ 西欧                   │ eu-west-1        │
   │   GB IE FR DE NL ...   │                  │
   ├────────────────────────┼──────────────────┤
   │ 中东欧 + 俄            │ eu-central-1     │
   │   PL RU UA TR ...      │                  │
   ├────────────────────────┼──────────────────┤
   │ 中东                   │ me-south-1       │
   │   IL AE SA QA ...      │                  │
   ├────────────────────────┼──────────────────┤
   │ 东亚                   │ ap-northeast-1   │
   │   JP · KR              │                  │
   ├────────────────────────┼──────────────────┤
   │ 大中华                 │ ap-east-1        │
   │   CN HK TW MO          │                  │
   ├────────────────────────┼──────────────────┤
   │ 东南亚                 │ ap-southeast-1   │
   │   SG MY ID TH VN PH    │                  │
   ├────────────────────────┼──────────────────┤
   │ 大洋洲                 │ ap-southeast-2   │
   │   AU · NZ              │                  │
   ├────────────────────────┼──────────────────┤
   │ 南亚                   │ ap-south-1       │
   │   IN · PK · BD         │                  │
   ├────────────────────────┼──────────────────┤
   │ 非洲                   │ af-south-1       │
   │   ZA · NG · KE         │                  │
   └────────────────────────┴──────────────────┘
   （完整列表见 internal/georegion/georegion.go）
```

私网 / loopback / 链路本地 IP 直接跳过——不会去查表浪费时间。

#### 月度 MMDB 自动更新

```bash
# /etc/cron.d/kirocc-geoip ── 每月 1 号 03:00 拉新版
0 3 1 * * curl -sSL "https://download.maxmind.com/app/geoip_download?\
edition_id=GeoLite2-Country&license_key=YOUR_KEY&suffix=tar.gz" \
  | tar xz --strip-components=1 -C /tmp                          \
  && mv /tmp/GeoLite2-Country.mmdb ~/.config/kirocc/             \
  && systemctl restart kirocc
```

```
╭─ WARN ─────────────────────────────────────────────────────────╮
│  GeoLite2-Country 只到国家级。东西海岸都是 US → us-east-1。   │
│  想分东西，要升级到 GeoLite2-City（更大、月更）然后改          │
│  georegion.countryToRegion 用 subdivision 字段。               │
╰────────────────────────────────────────────────────────────────╯
```

---

## ─── 故事 03 ───────────────────────────────────────────

> **「想给团队 3 个人各发一把 key，过 30 天自动失效。Bob 用太多了想限他 1M tokens。」**

### 多 API 密钥 with 过期 + 额度

| 字段 | 类型 | 描述 |
|---|---|---|
| `id` | 12 hex 字符 | 创建时自动生成 |
| `label` | 字符串 | 你给的可读名字 |
| `key` | `sk-` + 32 hex | 明文密钥，**只在创建/轮换那一刻返回一次** |
| `enabled` | bool | 禁用立即拒收 |
| `expires_at` | unix 秒 | `0` = 永不过期 |
| `quota_limit` | int64 | input+output tokens 上限，`0` = 不限 |
| `used_tokens` | int64 | 每次成功请求自动累加 |

**鉴权拦截链**

```
   client ──► Authorization: Bearer sk-...
                       │
                       ▼
           settings.ValidateAPIKey
                       │
        ┌──────────────┼──────────────┐
        ▼              ▼              ▼
    not found     expired          over_quota
       401        401              429
                "API key expired"  "API key over quota"
                       │
                       ▼  pass
                  ctx.api_key_id = id
                  ctx.device_id  = sha256(ip+ua)
                       │
                       ▼
                /v1/messages 继续
```

```
╭─ DANGER ───────────────────────────────────────────────────────╮
│  创建/轮换返回的明文 key 只显示一次。关闭那个绿色 reveal-block  │
│  之后只有掩码 sk******xx 可看。丢了只能 rotate。               │
╰────────────────────────────────────────────────────────────────╯
```

**UI 创建流（系统设置 → 认证密钥 → + 添加密钥）**

```
   ┌─── 添加 API 密钥 ────────────────────────────────────┐
   │ 标签 *           : ci-bot                            │
   │ 过期时间          : [ 永不 ][7d][30d ▣][60d][90d][1y][自定义] │
   │ 额度上限          : [ 不限 ][100k][1M ▣][10M][自定义] │
   │                                                      │
   │ » 30 天后（2026/06/19 15:30）· 上限 1M tokens        │
   │                                                      │
   │                          [ 取消 ] [ 创建 ]           │
   └──────────────────────────────────────────────────────┘
```

预设 chip 比原生 datetime-local 友好太多——选好下面实时显示「过期于 2026/06/19 15:30」预览。

---

## ─── 故事 04 ───────────────────────────────────────────

> **「凌晨 3 点客户问『我们昨天烧了多少 token』。」**

### 用量按 key / device / model 三维度分账

每个 `/v1/messages` 落 `usage_records` 表，带：

```
   ┌─────────────────┬─────────────────────────────────┐
   │ api_key_id      │ 匹配的 dynamic key id           │
   │                 │ ""  = legacy -api-key           │
   ├─────────────────┼─────────────────────────────────┤
   │ device_id       │ sha256(client_ip + user_agent)  │
   │                 │ 截取前 12 hex 字符              │
   ├─────────────────┼─────────────────────────────────┤
   │ resolved_model  │ 上游实际路由的模型              │
   ├─────────────────┼─────────────────────────────────┤
   │ credential_id   │ 池调度命中的账号                │
   └─────────────────┴─────────────────────────────────┘
```

**3 个分组端点**

```bash
curl -H "Authorization: Bearer $KEY" \
  "http://127.0.0.1:3457/admin/usage?window=24h&group=api_key"
# {
#   "totals":  {"requests": 1234, "input_tokens": 5e6, "output_tokens": 8e5},
#   "rows": [
#     {"api_key_id":"7ef367c45c4b","label":"ci-bot","requests":845,...},
#     {"api_key_id":"3a8b2f1e9c0d","label":"alice","requests":389,...}
#   ]
# }

curl ".../admin/usage?window=24h&group=device"   # 按设备指纹
curl ".../admin/usage?window=24h&group=model"    # 按模型
```

**仪表盘**：5 张 LED 卡片实时显示——`24h 请求 / 24h tokens / 活跃 API 密钥 / 活跃设备 / TOP API key`。

**Token 用量曲线**（带 4 个 range chip）

```
   ┌──── Token 用量曲线 ─── 24h ┃ 7d ┃ 15d ┃ 30d ─────────┐
   │                                                       │
   │  10M ┤      ╭╮                            input ━━━   │
   │      │     ╭╯╰╮                           output ━━   │
   │   5M ┤   ╭╮╯  ╰─╮       ╭─╮                           │
   │      │  ╭╯╯     ╰──╮  ╭╯ ╰╮                          │
   │   0  └──────────────────────────────────────────►     │
   │      0:00   06:00   12:00   18:00   24:00            │
   │                                                       │
   └───────────────────────────────────────────────────────┘
```

SQLite 索引 `(api_key_id, ts)` / `(device_id, ts)` 已建，30 天数据集毫秒级查询。

---

## ─── 故事 05 ───────────────────────────────────────────

> **「想写个脚本：跑 CI 时自动加新号，结束自动 disable。不想登 UI。」**

### 远程 API · curl + Python · 任意 OS 通杀

所有接口在 `http://<admin>:<port>/admin/*`，鉴权 `Authorization: Bearer <admin-key>`。变更类操作（POST/PUT/PATCH/DELETE）还要加 `X-Requested-With: XMLHttpRequest`——这是绕 CSRF 中间件的，不绕就 403。

#### 端点速查（已按高频排序）

```
   ┌─────────────────────────────────────────────────────────────┐
   │ ACCOUNTS                                                    │
   │   GET    /admin/accounts                                    │
   │   POST   /admin/accounts                       单条添加     │
   │   POST   /admin/accounts/import                批量导入     │
   │   GET    /admin/accounts/{id}                  详情         │
   │   PATCH  /admin/accounts/{id}                  改 proxy 等  │
   │   DELETE /admin/accounts/{id}                               │
   │   POST   /admin/accounts/{id}/refresh          强刷配额     │
   │   POST   /admin/accounts/{id}/disable                       │
   │   POST   /admin/accounts/{id}/enable                        │
   │                                                             │
   │ API KEYS                                                    │
   │   GET    /admin/api-keys                                    │
   │   POST   /admin/api-keys                       创建（含     │
   │                                                expires/quota）│
   │   PATCH  /admin/api-keys/{id}                  改属性       │
   │   POST   /admin/api-keys/{id}/rotate           轮换         │
   │   DELETE /admin/api-keys/{id}                               │
   │                                                             │
   │ USAGE                                                       │
   │   GET    /admin/usage?group=model|api_key|device            │
   │   GET    /admin/usage/timeline?window=&bucket=              │
   │   GET    /admin/usage/recent?limit=&status=                 │
   │                                                             │
   │ CREDS FILE                                                  │
   │   GET    /admin/credsfile                                   │
   │   PUT    /admin/credsfile                      覆写整文件   │
   │   GET    /admin/credsfile/download                          │
   │                                                             │
   │ OAUTH                                                       │
   │   POST   /admin/oauth/start         {provider, extras}      │
   │   GET    /admin/oauth/status?state=                         │
   │   POST   /admin/oauth/manual_callback                       │
   │                                                             │
   │ SETTINGS                                                    │
   │   GET    /admin/settings                                    │
   │   PUT    /admin/settings                       partial patch │
   │   GET    /admin/optimizations                  fix 状态+knob │
   └─────────────────────────────────────────────────────────────┘
```

#### Recipe 1 · 一键批量导入

```bash
KEY="your-admin-key"
BASE="http://127.0.0.1:3457"

# cockpit-tools 导出的 credentials.json 直接喂
curl -X POST \
  -H "Authorization: Bearer $KEY" \
  -H "Content-Type: application/json" \
  -H "X-Requested-With: XMLHttpRequest" \
  -d "{\"mode\":\"append\",\"accounts\":$(cat credentials.json)}" \
  "$BASE/admin/accounts/import"
# → {"added": 42, "skipped": 3}
```

#### Recipe 2 · CI 临时号生命周期

```bash
# 启动 CI 前：加号
echo '{"id":"ci-build-'$BUILD_ID'", "label":"ci-build-'$BUILD_ID'",
       "priority":1, "kiro_auth_token_raw":{...}}' | \
  curl -X POST -d @- -H ... "$BASE/admin/accounts"

# 跑完：删号
curl -X DELETE -H ... "$BASE/admin/accounts/ci-build-$BUILD_ID"
```

#### Recipe 3 · Python 监控脚本

```python
import os, time, json
import requests

BASE = os.environ["KIROCC_ADMIN_URL"]   # http://127.0.0.1:3457
KEY  = os.environ["KIROCC_ADMIN_KEY"]
H = {
    "Authorization":     f"Bearer {KEY}",
    "Content-Type":      "application/json",
    "X-Requested-With":  "XMLHttpRequest",
}

# 拿 24h 按 key 统计，打印 TOP 5
r = requests.get(f"{BASE}/admin/usage?window=24h&group=api_key", headers=H)
rows = r.json()["rows"]
rows.sort(key=lambda x: x["input_tokens"] + x["output_tokens"], reverse=True)

print(f"\n{'label':<25} {'reqs':>8} {'tokens':>12}")
print("─" * 50)
for r in rows[:5]:
    tok = r["input_tokens"] + r["output_tokens"]
    print(f"{r['label']:<25} {r['requests']:>8} {tok:>12,}")
```

```
╭─ TIP ──────────────────────────────────────────────────────────╮
│ admin UI 系统设置 → 远程 API 子页里有同样的 curl/Python 速查， │
│ 但 URL 已经替换成你这台机器的实际地址，直接复制就能跑。       │
╰────────────────────────────────────────────────────────────────╯
```

---

## ─── 故事 06 ───────────────────────────────────────────

> **「想给老板演示 kirocc 到底做了什么。光说『8 个 fix』他听不懂。」**

### kirocc 优化可视化

系统设置 → kirocc 优化 子页面把全部 8 个 fork fix + 7 个 env knob 摊在一张大表上：

```
   ┌─── 协议修复（Always-On）─────────────────────────────┐
   │ [ FIX 01 ] MCP system-reminder 清理      ALWAYS ON  │
   │   剥离 MCP 历史大段块，prompt token 省 14%          │
   │                                                      │
   │ [ FIX 02 ] 保留非 MCP 系统提醒           ALWAYS ON  │
   │   currentDate / 用户偏好 保留                        │
   │                                                      │
   │ [ FIX 03 ] tool 输入 required 字段校验   ALWAYS ON  │
   │   Write {} / Edit {} 这种缺字段不穿透                │
   │                                                      │
   │ [ FIX 04 ] tool 输入顶层类型校验         ALWAYS ON  │
   │ [ FIX 05 ] 非法 tool_use 自动回灌        ALWAYS ON  │
   │ [ FIX 06 ] 二次非法降级为可见错误         ALWAYS ON  │
   │ [ FIX 07 ] ToolSearch 错误隔离           ALWAYS ON  │
   │                                                      │
   │ [ FIX 08 ] 代理层 thinking budget 兜底  ENV 已配置  │
   │   KIROCC_FORCE_THINKING_BUDGET = 80000              │
   │   设置后代理无条件注入到上游                         │
   └──────────────────────────────────────────────────────┘

   ┌─── 可调参数（settings.json 持久化）─────────────────┐
   │ 强制 thinking budget 下限 [ 80000           ]       │
   │ Thinking prompt 模式      [ tool      ▼     ]       │
   │ Thinking tool 续写        [ 默认      ▼     ]       │
   │ System 模式               [             ]           │
   │ 上游 Origin               [             ]           │
   │ 模型映射 (JSON)           [             ]           │
   │ 审计日志路径              [             ]           │
   │                                  [ 保存 ]            │
   └──────────────────────────────────────────────────────┘
```

**关键设计**：UI 写入 `settings.json` 持久化，启动时若对应 `KIROCC_*` env var **未显式设置**，从 settings 加载到 env。**显式 env 永远赢**——这意味着 systemd unit 里写死的 env 不会被 UI 误改。

启动日志告诉你哪些 settings 值被加载了：

```
2026-05-20 18:19:26 INF optimizations applied from settings vars=[
  KIROCC_FORCE_THINKING_BUDGET
  KIROCC_EXPERIMENT_THINKING_PROMPT
  KIROCC_AUDIT_LOG
]
```

---

## 系统设置侧栏地图

```
   ┌─ kirocc-pro · 管理后台 ─────────────────┐
   │  ▤  仪表盘                              │
   │  ◍  账号                                │
   │  ⎘  认证文件                            │
   │  ↗  使用统计                            │
   │  ⚙  系统设置  ◄── 二级菜单展开 ────────┐│
   │      ›  运行时       /settings/runtime ││
   │      ›  认证密钥     /settings/auth    ││
   │      ›  远程访问     /settings/remote  ││
   │      ›  系统         /settings/system  ││
   │      ›  网络         /settings/network ││
   │      ›  流式传输     /settings/streaming│
   │      ›  kirocc 优化  /settings/optimizations│
   │      ›  远程 API     /settings/api     ││
   └──────────────────────────────────────────┘
```

直接 URL 跳：

- `#/settings/auth` → 加 API key
- `#/settings/optimizations` → 调 thinking budget
- `#/settings/api` → 复制 curl 速查
- `#/settings/network` → 改 preferred_region

---

## 数据落盘三件套

```
   ╔════════════════════╦════════════════════════════════╗
   ║ credentials.json   ║ -creds-json                    ║
   ║                    ║ 多账号池：token / proxy_url /  ║
   ║                    ║ profile ARN / region           ║
   ╠════════════════════╬════════════════════════════════╣
   ║ settings.json      ║ -settings                      ║
   ║                    ║ API 密钥列表 / 网络偏好 /      ║
   ║                    ║ 优化 knob / 偏好地区           ║
   ╠════════════════════╬════════════════════════════════╣
   ║ usage.sqlite       ║ -usage-db                      ║
   ║                    ║ 历史请求 + api_key_id +        ║
   ║                    ║ device_id 索引                 ║
   ╚════════════════════╩════════════════════════════════╝
```

老 schema 启动时自动 ALTER TABLE 加新列——无需手动迁移。

---

## 安全注意事项 · 三条红线

```
╭─ DANGER ───────────────────────────────────────────────────────╮
│  ① admin-key 即 web 登录密码 + Bearer token。写进自动化脚本前 │
│    确认环境安全。环境变量 + 严格 chmod 600 是最低标准。       │
│                                                                │
│  ② 非 loopback 暴露必须 TLS（-admin-tls-cert/-key）+ 强        │
│    admin-key。kirocc 默认绑 127.0.0.1，改 0.0.0.0 前三思。     │
│                                                                │
│  ③ KIROCC_AUDIT_LOG 会写完整请求/响应（含 prompt 内容和模型   │
│    输出）到文件。生产关掉，仅 debug 用。                       │
╰────────────────────────────────────────────────────────────────╯
```

附加注意：

- **创建/轮换 API key 时**返回的明文是**唯一一次展示**——丢了只能 rotate。
- **设备指纹** `sha256(ip+ua)[:12]` 是 48 位熵，能区分设备但不能反推 IP。如果对反推都担心，在反向代理层把 `X-Forwarded-For` 剥掉。
- **OAuth proxy 字段**走 token-exchange POST，不走浏览器跳转——浏览器端 OAuth 流量永远是用户自己的 IP，不受 proxy_url 影响。

---

## 想继续聊什么？

接下来还想加的（按工作量从小到大）：

- 一个独立的 **deploy token**（区别于 admin login key），权限只限 `/admin/accounts/*` 写，便于 CI 用；
- **实时日志流**（SSE，admin 新增「日志」顶级 tab，赛博终端式滚动）；
- **GeoLite2-City 升级**——把 US 拆成 east / west，按 state 细分；
- 把 **per-cred proxy 也覆盖到 `/v1/messages` 上游路径**（现在只覆盖鉴权面）；
- 一份 **英文版本文档**（如果你的目标用户是国际开源社区）。

告诉我接哪个，我接着做。
