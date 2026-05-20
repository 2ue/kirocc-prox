#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"

: "${KIROCC_HOST:=127.0.0.1}"
: "${KIROCC_PORT:=3460}"
: "${KIROCC_API_KEY:=local-kirocc-test-token}"
: "${KIROCC_EXPERIMENT_THINKING_PROMPT:=minimal}"
: "${KIROCC_FORCE_THINKING_BUDGET:=100000}"
: "${KIROCC_UPSTREAM_ORIGIN:=AI_EDITOR}"
: "${KIROCC_LOG_FILE:=}"

export KIROCC_HOST
export KIROCC_PORT
export KIROCC_API_KEY
export KIROCC_EXPERIMENT_THINKING_PROMPT
export KIROCC_FORCE_THINKING_BUDGET
export KIROCC_UPSTREAM_ORIGIN
export KIROCC_LOG_FILE

mkdir -p ./bin
GOEXPERIMENT=jsonv2 go build -o ./bin/kirocc ./cmd/kirocc

printf 'kirocc starting: http://%s:%s\n' "$KIROCC_HOST" "$KIROCC_PORT"
printf 'api key: %s\n' "$KIROCC_API_KEY"
printf 'thinking budget: %s\n' "$KIROCC_FORCE_THINKING_BUDGET"
printf 'Claude Code ANTHROPIC_BASE_URL=http://%s:%s\n' "$KIROCC_HOST" "$KIROCC_PORT"
printf 'Claude Code ANTHROPIC_AUTH_TOKEN=%s\n' "$KIROCC_API_KEY"

exec ./bin/kirocc
