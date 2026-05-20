# kirocc 3460 优化版源码包

这是一个基于 `kirocc` 的 3460 优化版源码包，用于把 Anthropic Messages API 请求转到 Kiro / Amazon Q CodeWhispererStreaming 链路，再返回 Anthropic 兼容 SSE/JSON，方便 Claude Code 接入 Kiro 会员模型。

> 重要：本包不包含任何 Kiro 凭据、access token、refresh token、SQLite 数据库、日志或本机二进制。使用者必须自己先登录 Kiro，让本机存在 Kiro CLI/IDE 的凭据数据库。

---

## 一句话结论

这版主要修代理层污染和工具调用死锁问题，让 Opus 4.7 / 1M 在 Kiro 链路下更像可用的 Claude Code 后端：更少 prompt 污染、更稳工具调用、更高 thinking budget。

---

## 这版更新了什么

修了 8 个会让模型变笨、卡死、或者工具调用失败后无法恢复的问题：

1. **清理历史 MCP 系统提示污染**
   - 历史 user 消息里的 `<system-reminder># MCP Server Instructions ...</system-reminder>` 会被清理。
   - 实测可减少一批无用 prompt tokens；本机测试样本约省 14%。

2. **保留有用系统提醒，删除无用 MCP 大段说明**
   - 只清理 MCP Server Instructions 这类大段工具说明。
   - currentDate 等普通系统提醒仍保留，避免误删有效上下文。

3. **工具参数 required 校验**
   - 模型乱发 `Write {}`、`Edit {}`、缺 `file_path/content/old_string/new_string` 时，代理层先拦截。
   - 坏 tool_use 不再直接穿透到 Claude Code，避免 CC 报 `InputValidationError` 后整轮停死。

4. **工具参数顶层 type 校验**
   - 例如 `AskUserQuestion.questions` 必须是 array，模型给 string 会被拦下。
   - 只做顶层 schema 校验，保持热路径轻，不做复杂递归 JSON Schema。

5. **非法 tool_use 自动回灌重试**
   - 第一次上游返回坏工具参数时，代理会把错误包装成 `tool_result(is_error=true)` 回灌给模型。
   - 模型有机会本轮自愈：补齐参数、换工具、或改成文本说明。

6. **第二次仍非法时降级返回可见错误**
   - 防止无限重试。
   - 如果模型连续两轮都吐坏 tool_use，代理返回明确 fallback，而不是 502 死锁。

7. **ToolSearch 输入坏不再 HTTP 400 杀整轮**
   - `ToolSearch` 输入解析失败时，返回标准 `tool_search_tool_result_error`。
   - 模型可以继续，而不是整轮请求直接挂掉。

8. **thinking budget 可在代理层强制抬高**
   - 新增 `KIROCC_FORCE_THINKING_BUDGET`。
   - 推荐：`100000` 用于 xhigh 稳定测试。
   - 激进：`160000` 用于 max，更聪明但更慢、更吃额度，也更容易撞上游容量。

---

## 模型名

推荐在 Claude Code 里使用：

```text
claude-opus-4-7[1m]
```

兼容别名：

```text
claude-opus-4-7
claude-opus-4.7[1m]
claude-opus-4.7
claude-opus-4-6[1m]
claude-opus-4.6[1m]
```

注意：`[1m]` 是给 Claude Code 看的上下文标识，代理会映射到 Kiro 上游模型名。

---

## 编译

需要 Go 1.24+，并启用 jsonv2：

```bash
cd kirocc-3460-optimized-source
GOEXPERIMENT=jsonv2 go test ./...
GOEXPERIMENT=jsonv2 go build -o ./bin/kirocc ./cmd/kirocc
```

---

## 启动 3460 服务

最简单方式：

```bash
cd kirocc-3460-optimized-source
chmod +x examples/start-kirocc-3460.sh
./examples/start-kirocc-3460.sh
```

默认会监听：

```text
http://127.0.0.1:3460
```

默认本地 API key：

```text
local-kirocc-test-token
```

如果你换了 `KIROCC_API_KEY`，Claude Code 里的 `ANTHROPIC_AUTH_TOKEN` 也要同步换成同一个值。

---

## 必要前提：Kiro 凭据

kirocc 默认读取 Kiro CLI/IDE 的 SQLite 凭据库。

默认路径：

| 系统 | 默认路径 |
|---|---|
| macOS | `~/Library/Application Support/kiro-cli/data.sqlite3` |
| Linux | `~/.local/share/kiro-cli/data.sqlite3` |

如果你的 Kiro 数据库不在默认路径，启动前指定：

```bash
export KIROCC_DB_PATH="/你的/kiro/data.sqlite3"
```

---

## Claude Code 配置示例

见：

```text
examples/claude-code-settings-3460.json
```

核心配置如下：

```json
{
  "alwaysThinkingEnabled": true,
  "env": {
    "ANTHROPIC_BASE_URL": "http://127.0.0.1:3460",
    "ANTHROPIC_AUTH_TOKEN": "local-kirocc-test-token",
    "ANTHROPIC_MODEL": "claude-opus-4-7[1m]",
    "ANTHROPIC_DEFAULT_OPUS_MODEL": "claude-opus-4-7[1m]",
    "ANTHROPIC_DEFAULT_OPUS_MODEL_SUPPORTED_CAPABILITIES": "effort,xhigh_effort,max_effort,thinking,adaptive_thinking,interleaved_thinking",
    "CLAUDE_CODE_EFFORT_LEVEL": "max",
    "CLAUDE_CODE_MAX_OUTPUT_TOKENS": "64000",
    "MAX_THINKING_TOKENS": "100000"
  },
  "model": "opus"
}
```

---

## thinking budget 怎么选

服务端环境变量最关键：

```bash
export KIROCC_FORCE_THINKING_BUDGET=100000
```

建议：

| 目标 | 值 | 说明 |
|---|---:|---|
| 稳定高思考 | `100000` | 推荐默认；相当于 xhigh 上限 |
| 激进 max | `160000` | 更吃额度、更慢，可能更容易遇到上游容量问题 |

如果你只改 Claude Code 的 `MAX_THINKING_TOKENS`，不一定真的传到 Kiro 上游。这个版本的重点就是在代理层用 `KIROCC_FORCE_THINKING_BUDGET` 强制注入。

---

## 快速测试

启动服务后执行：

```bash
cd kirocc-3460-optimized-source
chmod +x examples/curl-smoke-test.sh
./examples/curl-smoke-test.sh
```

它会做两件事：

1. 检查 `/health`。
2. 发一个 `/v1/messages` 请求到 `claude-opus-4-7[1m]`。

---

## 给别的 AI 的测试使用说明

你是评审这个代理层的 AI。不要只看 README，要验证以下点：

1. **源码测试**
   ```bash
   GOEXPERIMENT=jsonv2 go test ./...
   ```

2. **确认没有真实凭据**
   ```bash
   grep -R "sk-kirocc\|accessToken\|refreshToken" -n . \
     --exclude-dir=.git --exclude='README_3460_OPTIMIZED.md'
   ```
   允许源码里出现字段名 `accessToken/refreshToken`，但不应该出现真实 token 值。

3. **确认 thinking 注入**
   - 设置 `KIROCC_FORCE_THINKING_BUDGET=100000`。
   - 请求一次模型。
   - 如果开启 audit/debug，检查上游 payload 里是否出现：
     ```xml
     <max_thinking_length>100000</max_thinking_length>
     ```

4. **确认工具坏参数不会穿透**
   - 看 `internal/respconv/tool_input_validator.go`。
   - 看 `internal/app/messages/invalid_tool_retry.go`。
   - 重点验证 `Write/Edit/AskUserQuestion` 这类工具缺必填字段或字段类型错误时，不会直接传给 Claude Code。

5. **确认 ToolSearch 坏输入不会整轮 HTTP 400**
   - 看 `internal/app/messages/toolsearch.go`。
   - 预期是返回 `tool_search_tool_result_error`，而不是把整轮请求打挂。

6. **重点回归题**
   - 让模型执行文件读写任务，观察是否还出现：
     ```text
     InputValidationError: Write failed ... file_path/content missing
     InputValidationError: Edit failed ... old_string/new_string missing
     AskUserQuestion questions expected array but got string
     ```
   - 正常预期：模型会自愈重试或换工具，而不是直接停死。

---

## 不包含什么

为了方便分享，本包故意不包含：

- Kiro access token / refresh token
- Kiro SQLite 数据库
- `/tmp` 日志
- audit jsonl
- 本机 LaunchAgent plist
- 本机编译好的旧二进制
- 本机 A/B 测试脚本里的真实路径和 token

---

## 常见问题

### 1. Ubuntu 上跑不了怎么办？

先确认 Go 版本和 Kiro 凭据路径：

```bash
go version
ls ~/.local/share/kiro-cli/data.sqlite3
```

如果数据库路径不同：

```bash
export KIROCC_DB_PATH="/实际路径/data.sqlite3"
```

然后重新：

```bash
GOEXPERIMENT=jsonv2 go build -o ./bin/kirocc ./cmd/kirocc
```

### 2. Claude Code 报 auth conflict？

不要同时设置：

```text
ANTHROPIC_AUTH_TOKEN
ANTHROPIC_API_KEY
```

本代理推荐只设置：

```text
ANTHROPIC_AUTH_TOKEN
```

### 3. 4.7 很慢或者 502？

这通常是 Kiro/上游容量或模型压力问题。可以尝试：

- 把 `KIROCC_FORCE_THINKING_BUDGET=100000` 降到 `60000`。
- 临时切到 `claude-opus-4-6[1m]`。
- 避免同一账号同时开太多 Claude Code 窗口抢额度。

### 4. 这个版本会不会让模型一定变聪明？

不会保证模型本身变强。它解决的是代理层污染、thinking 预算被压低、工具坏调用卡死这些问题。模型本身的智商、Kiro 上游容量、账号额度仍然会影响效果。
