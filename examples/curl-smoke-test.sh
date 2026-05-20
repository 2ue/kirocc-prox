#!/usr/bin/env bash
set -euo pipefail

BASE_URL="${BASE_URL:-http://127.0.0.1:3460}"
API_KEY="${API_KEY:-local-kirocc-test-token}"
MODEL="${MODEL:-claude-opus-4-7[1m]}"

printf 'health: '
curl -fsS "$BASE_URL/health"
printf '\n\nmessages smoke test:\n'

curl -fsS "$BASE_URL/v1/messages" \
  -H "Authorization: Bearer $API_KEY" \
  -H 'Content-Type: application/json' \
  -d @- <<JSON
{
  "model": "$MODEL",
  "max_tokens": 256,
  "messages": [
    {"role": "user", "content": "只回答：kirocc smoke ok"}
  ]
}
JSON
printf '\n'
