#!/usr/bin/env sh
set -eu

out="${1:-.env.deploy}"
out_dir=$(dirname -- "$out")
if [ "$out_dir" != "." ]; then
  mkdir -p "$out_dir"
fi

if [ -e "$out" ]; then
  echo "$out already exists; refusing to overwrite it." >&2
  exit 1
fi

rand_hex() {
  bytes="$1"
  if command -v openssl >/dev/null 2>&1; then
    openssl rand -hex "$bytes"
    return
  fi
  if command -v python3 >/dev/null 2>&1; then
    python3 - "$bytes" <<'PY'
import secrets
import sys

print(secrets.token_hex(int(sys.argv[1])))
PY
    return
  fi
  chars=$((bytes * 2))
  LC_ALL=C tr -dc 'a-f0-9' < /dev/urandom | head -c "$chars"
  printf '\n'
}

: "${KIROCC_DEPLOY_PROJECT:=kirocc-prox-deploy}"
: "${KIROCC_IMAGE:=ghcr.io/2ue/kirocc-prox}"
: "${KIROCC_VERSION:=latest}"
: "${KIROCC_PROXY_BIND:=127.0.0.1}"
: "${KIROCC_PROXY_HOST_PORT:=9326}"
: "${KIROCC_ADMIN_BIND:=127.0.0.1}"
: "${KIROCC_ADMIN_HOST_PORT:=3457}"
: "${KIROCC_ADMIN_PUBLIC_URL:=}"
: "${KIROCC_DATA_DIR:=./deploy-data}"
: "${POSTGRES_DB:=kirocc_pro}"
: "${POSTGRES_USER:=kirocc}"
: "${POSTGRES_PASSWORD:=kc_pg_$(rand_hex 24)}"
host_uid=$(id -u 2>/dev/null || printf '70')
host_gid=$(id -g 2>/dev/null || printf '70')
if [ "$host_uid" = "0" ]; then
  host_uid=70
  host_gid=70
fi
: "${KIROCC_POSTGRES_UID:=$host_uid}"
: "${KIROCC_POSTGRES_GID:=$host_gid}"
: "${REDIS_PASSWORD:=kc_rd_$(rand_hex 24)}"
: "${KIROCC_REDIS_DB:=0}"
: "${KIROCC_REDIS_KEY_PREFIX:=kirocc:deploy:}"
: "${KIROCC_REDIS_LEASE_TTL:=30m}"
: "${KIROCC_API_KEY:=sk-kc-$(rand_hex 24)}"
: "${KIROCC_ADMIN_KEY:=kc_admin_$(rand_hex 24)}"
: "${KIROCC_POOL_STRATEGY:=least-inflight}"
: "${KIROCC_QUOTA_POLL_INTERVAL:=3m}"
: "${KIROCC_PROMPT_CACHE:=false}"
: "${KIROCC_PROMPT_CACHE_TARGET_READ_RATIO:=0.90}"
: "${KIROCC_PROMPT_CACHE_REPORTS:=}"

umask 077
cat > "$out" <<EOF
KIROCC_DEPLOY_PROJECT=$KIROCC_DEPLOY_PROJECT
KIROCC_IMAGE=$KIROCC_IMAGE
KIROCC_VERSION=$KIROCC_VERSION

KIROCC_PROXY_BIND=$KIROCC_PROXY_BIND
KIROCC_PROXY_HOST_PORT=$KIROCC_PROXY_HOST_PORT
KIROCC_ADMIN_BIND=$KIROCC_ADMIN_BIND
KIROCC_ADMIN_HOST_PORT=$KIROCC_ADMIN_HOST_PORT
KIROCC_ADMIN_PUBLIC_URL=$KIROCC_ADMIN_PUBLIC_URL

KIROCC_DATA_DIR=$KIROCC_DATA_DIR

POSTGRES_DB=$POSTGRES_DB
POSTGRES_USER=$POSTGRES_USER
POSTGRES_PASSWORD=$POSTGRES_PASSWORD
KIROCC_POSTGRES_UID=$KIROCC_POSTGRES_UID
KIROCC_POSTGRES_GID=$KIROCC_POSTGRES_GID

REDIS_PASSWORD=$REDIS_PASSWORD
KIROCC_REDIS_DB=$KIROCC_REDIS_DB
KIROCC_REDIS_KEY_PREFIX=$KIROCC_REDIS_KEY_PREFIX
KIROCC_REDIS_LEASE_TTL=$KIROCC_REDIS_LEASE_TTL

KIROCC_API_KEY=$KIROCC_API_KEY
KIROCC_ADMIN_KEY=$KIROCC_ADMIN_KEY
KIROCC_POOL_STRATEGY=$KIROCC_POOL_STRATEGY
KIROCC_QUOTA_POLL_INTERVAL=$KIROCC_QUOTA_POLL_INTERVAL

KIROCC_PROMPT_CACHE=$KIROCC_PROMPT_CACHE
KIROCC_PROMPT_CACHE_TARGET_READ_RATIO=$KIROCC_PROMPT_CACHE_TARGET_READ_RATIO
KIROCC_PROMPT_CACHE_REPORTS=$KIROCC_PROMPT_CACHE_REPORTS
EOF

mkdir -p "$KIROCC_DATA_DIR/postgres" "$KIROCC_DATA_DIR/redis"
chmod 0777 "$KIROCC_DATA_DIR" "$KIROCC_DATA_DIR/postgres" "$KIROCC_DATA_DIR/redis" 2>/dev/null || true

echo "Wrote $out"
echo "Created $KIROCC_DATA_DIR/postgres and $KIROCC_DATA_DIR/redis"
echo "Start with: docker compose --env-file $out -f docker-compose.deploy.yml up -d"
