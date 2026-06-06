# kirocc-pro Fork Hardening Notes

This document records the proxy hardening carried by this fork. It is not a deployment guide. For current startup, storage and scheduling instructions, use [`README.md`](./README.md) and the PostgreSQL + Redis audit in [`docs/pgredis-complete-migration-audit.zh-CN.md`](./docs/pgredis-complete-migration-audit.zh-CN.md).

## Scope

The fork keeps the upstream Anthropic-to-Kiro translation goal and adds proxy-side protections for long Claude Code sessions, tool flows, model routing and operator visibility.

## Protocol Fixes

| # | Fix | Effect |
|---|---|---|
| 1 | Strip historical MCP `system-reminder` blocks | Reduces repeated prompt ballast in long sessions |
| 2 | Preserve non-MCP system reminders | Keeps normal reminders such as date/context hints |
| 3 | Validate top-level required tool input fields | Bad tool calls are rejected before crossing upstream |
| 4 | Validate top-level tool input types | Prevents common malformed tool arguments |
| 5 | Reflow invalid `tool_use` as `tool_result(is_error=true)` | Gives the model one self-repair round |
| 6 | Return a visible error after repeated invalid tool use | Bounds retry loops and avoids opaque failures |
| 7 | Isolate ToolSearch errors | A failed discovery call does not kill the whole turn |
| 8 | Enforce a proxy-side thinking budget floor | Keeps long-context thinking behavior predictable |

The proxy also synthesizes a trace-backed session id when `X-Claude-Code-Session-Id` is absent.

## Current Runtime Assumption

All durable state belongs in PostgreSQL, and all cross-request runtime state belongs in Redis. Fork hardening must not reintroduce local-file storage paths or a single-account fallback.

## Verification

Use the repository-wide checks from the migration audit:

```bash
node --check internal/admin/html/app.js
GOEXPERIMENT=jsonv2 go test ./...
GOEXPERIMENT=jsonv2 go test -run '^$' -tags e2e ./internal/e2e
```

Run the dependency residue check from the migration audit before release.
