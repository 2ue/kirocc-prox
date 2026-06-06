# Prompt Cache 上报 Profile

## 目标

本模块用于把下游 Anthropic 兼容响应里的 usage 上报做成可插拔、可配置的本地模块：

- 不修改实际发往上游 Kiro 的 payload。
- 可以按请求路径选择不同的上报 profile。
- 可以独立控制 `input_tokens`、`output_tokens`、`cache_read_input_tokens`、`cache_creation_input_tokens`。
- 可以选择是否启用本地 prompt-cache 指纹计算。
- profile 名称完全由配置决定；代码里没有“高缓存/低缓存”这类档位概念。

## 默认路径

默认配置参考 `~/Desktop/procode/kiro.rs` 的几个缓存路径，并按本项目的 `/api/` 自定义路由规则落地：

| kiro.rs 路径 | 本项目默认路径 | 默认 profile | 行为 |
|---|---|---|---|
| `/v1/messages` | `/v1/messages` | `default` | 本地模拟缓存上报；下游 input/output 使用原始值，cache read/write 保留计算值 |
| `/cc/v1/messages` | `/api/cc/v1/messages` | `cc` | 本地模拟缓存上报；下游 `input_tokens` 在 256 上限内采样，减少的差值转入 cache read；cache write 采样到 3000 附近 |
| `/ha/v1/messages` | `/api/ha/v1/messages` | `ha` | 本地模拟缓存上报；只改下游 `input_tokens`，cache write 保留本地计算值 |
| `/na/v1/messages` | `/api/na/v1/messages` | `na` | 不做本地模拟补足；保留真实上游 cache usage |

对应的模型和 token 估算端点也由同一前缀承载：

```text
/v1/models
/v1/messages
/v1/messages/count_tokens

/api/cc/v1/models
/api/cc/v1/messages
/api/cc/v1/messages/count_tokens

/api/ha/v1/models
/api/ha/v1/messages
/api/ha/v1/messages/count_tokens

/api/na/v1/models
/api/na/v1/messages
/api/na/v1/messages/count_tokens
```

自定义 route 会默认归一到 `/api` 下，例如配置自定义路由名 `<name>` 最终生效为 `/api/<name>`。标准 `/v1` 路径保持原样，不会被加 `/api` 前缀。

## 默认配置

PostgreSQL settings 初次初始化或缺少 `prompt_cache_reports` 字段时，会自动写入下面这份默认配置。若用户显式保存：

```json
{
  "prompt_cache_reports": {}
}
```

则表示主动关闭路径/profile prompt-cache 上报，重启后不会被恢复默认。

默认值：

```json
{
  "prompt_cache_reports": {
    "routes": {
      "/v1/messages": "default",
      "/api/cc": "cc",
      "/api/ha": "ha",
      "/api/na": "na"
    },
    "profiles": {
      "default": {
        "enabled": true,
        "simulate_cache": true,
        "synthesize_stable_prefix": true,
        "target_read_ratio": 0.95,
        "input": { "mode": "raw" },
        "output": { "mode": "raw" },
        "cache_read": { "mode": "preserve" },
        "cache_creation": { "mode": "preserve" },
        "token_scale": 1.6,
        "max_simulated_input_tokens": 300000,
        "cap_jitter_min_tokens": 12000,
        "cap_jitter_max_tokens": 24000,
        "scale_min_input_tokens": 20000
      },
      "cc": {
        "enabled": true,
        "simulate_cache": true,
        "synthesize_stable_prefix": true,
        "target_read_ratio": 0.95,
        "input": {
          "mode": "sample-max",
          "max_tokens": 256,
          "move_delta_to_cache_read": true
        },
        "output": { "mode": "raw" },
        "cache_read": { "mode": "preserve" },
        "cache_creation": {
          "mode": "sample-target",
          "target_tokens": 3000,
          "normal_max_multiplier": 1.2
        },
        "token_scale": 1.6,
        "max_simulated_input_tokens": 300000,
        "cap_jitter_min_tokens": 12000,
        "cap_jitter_max_tokens": 24000,
        "scale_min_input_tokens": 20000
      },
      "ha": {
        "enabled": true,
        "simulate_cache": true,
        "synthesize_stable_prefix": true,
        "target_read_ratio": 0.95,
        "input": {
          "mode": "sample-max",
          "max_tokens": 256,
          "move_delta_to_cache_read": true
        },
        "output": { "mode": "raw" },
        "cache_read": { "mode": "preserve" },
        "cache_creation": { "mode": "preserve" },
        "token_scale": 1.6,
        "max_simulated_input_tokens": 300000,
        "cap_jitter_min_tokens": 12000,
        "cap_jitter_max_tokens": 24000,
        "scale_min_input_tokens": 20000
      },
      "na": {
        "enabled": false,
        "simulate_cache": false,
        "synthesize_stable_prefix": true,
        "target_read_ratio": 0.95,
        "input": { "mode": "raw" },
        "output": { "mode": "raw" },
        "cache_read": { "mode": "preserve" },
        "cache_creation": { "mode": "preserve" },
        "token_scale": 1.6,
        "max_simulated_input_tokens": 300000,
        "cap_jitter_min_tokens": 12000,
        "cap_jitter_max_tokens": 24000,
        "scale_min_input_tokens": 20000
      }
    }
  }
}
```

## 配置入口

优先级从高到低：

1. 启动参数 `-prompt-cache-reports`。
2. 环境变量 `KIROCC_PROMPT_CACHE_REPORTS`。
3. PostgreSQL settings / 系统设置页“缓存上报”。

启动参数示例：

```bash
kirocc \
  -prompt-cache-reports '{"routes":{"/v1/messages":"default","cc":"cc"},"profiles":{"default":{"enabled":true,"simulate_cache":true,"synthesize_stable_prefix":true,"target_read_ratio":0.95},"cc":{"enabled":true,"simulate_cache":true,"synthesize_stable_prefix":true,"target_read_ratio":0.95,"input":{"mode":"sample-max","max_tokens":256,"move_delta_to_cache_read":true},"cache_creation":{"mode":"sample-target","target_tokens":3000,"normal_max_multiplier":1.2}}}}'
```

## 字段策略

每个 profile 可以配置四个字段：

```json
{
  "input": {},
  "output": {},
  "cache_read": {},
  "cache_creation": {}
}
```

字段策略支持：

- `raw`：使用上游或原始 usage 值。
- `preserve`：保留本地计算后的值。
- `sample-max`：把 `max_tokens` 当作上限，在上限内按低/中/高区间确定性采样；不是每次都取最大值。
- `sample-target`：先用 `target_tokens * normal_max_multiplier` 得到常规上限，再按区间采样；cache creation 会按是否已有 cache read 使用不同分布。

`input.move_delta_to_cache_read=true` 时，如果 `input_tokens` 被采样压低，差值会转入 `cache_read_input_tokens`。

## 缓存计算边界

本地 prompt-cache tracker 只存短期 fingerprint，不存完整请求正文，也不修改上游请求。

会尝试计算缓存的前提：

- 当前路径匹配到启用的 profile。
- profile 的 `simulate_cache=true`。
- 存在账号、会话、模型三个 scope 维度。
- 输入 token 达到模型最低可缓存门槛。
- 请求存在 `cache_control: {"type":"ephemeral"}`，或 profile 显式设置 `synthesize_stable_prefix=true`。
- 上游没有真实 cache usage，或者真实 cache usage 为 0。

写入 tracker 的时机：

- 下游响应成功。
- 本地模拟 usage 确实被用于最终上报。
- 失败请求、上游真实 cache usage 非 0、禁用 profile 都不会提交 tracker。

## 和 kiro.rs 的关系

当前实现参考了 `kiro.rs` 的缓存计算方法：

- 按账号、会话、模型隔离 scope。
- flatten request blocks。
- canonicalize 后累计 SHA-256 fingerprint。
- 使用 5m / 1h TTL。
- 按模型判断最低可缓存 token。
- 使用目标读缓存比例和确定性 jitter。
- 成功上报后才更新 tracker。

但本项目没有引入内置缓存档位或模式名。`default`、`cc`、`ha`、`na` 只是默认配置里的 profile 名称；后续也可以完全换成运维自定义的 profile 名称。
