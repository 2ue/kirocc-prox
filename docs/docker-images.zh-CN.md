# Docker 镜像发布

## 结论

本项目支持构建并发布多架构 Docker 镜像到：

- GitHub Container Registry: `ghcr.io/2ue/kirocc-prox`
- Docker Hub: `docker.io/<DOCKERHUB_USERNAME>/kirocc-prox`

发布由 GitHub Actions workflow `.github/workflows/docker-publish.yml` 执行。

## 本地构建

```bash
docker build \
  --build-arg GIT_SHA="$(git rev-parse --short HEAD)" \
  -t kirocc-prox:local .
```

当前项目依赖 Go `encoding/json/v2`，镜像构建会显式设置：

```text
GOEXPERIMENT=jsonv2
```

## 部署运行完整服务

`docker-compose.yml` 和 `docker-compose.deploy.yml` 都是远端镜像部署形态，会启动：

- `app`: 代理和管理后台
- `postgres`: PostgreSQL 持久化账号、设置、使用记录、配额快照
- `redis`: Redis 运行时调度状态

```bash
./scripts/init-deploy-env.sh .env.deploy
docker compose --env-file .env.deploy -f docker-compose.deploy.yml up -d
```

脚本会自动生成以下部署变量：

```text
KIROCC_DEPLOY_PROJECT
KIROCC_IMAGE
KIROCC_VERSION
KIROCC_PROXY_BIND
KIROCC_PROXY_HOST_PORT
KIROCC_ADMIN_BIND
KIROCC_ADMIN_HOST_PORT
KIROCC_DATA_DIR
POSTGRES_DB
POSTGRES_USER
POSTGRES_PASSWORD
KIROCC_POSTGRES_UID
KIROCC_POSTGRES_GID
REDIS_PASSWORD
KIROCC_API_KEY
KIROCC_ADMIN_KEY
KIROCC_REDIS_KEY_PREFIX
```

默认宿主端口：

```text
Proxy: http://127.0.0.1:9326
Admin: http://127.0.0.1:3457
```

如果要对外开放，修改 `.env.deploy`：

```text
KIROCC_PROXY_BIND=0.0.0.0
KIROCC_ADMIN_BIND=0.0.0.0
KIROCC_ADMIN_PUBLIC_URL=https://your-admin.example.com
```

PostgreSQL 和 Redis 没有 `ports` 映射，只在 compose 内部网络可访问。数据持久化使用本地目录映射，默认：

```text
./deploy-data/postgres -> /var/lib/postgresql/data
./deploy-data/redis    -> /data
```

没有使用 Docker named volumes；如果要迁移数据，直接迁移 `KIROCC_DATA_DIR` 指向的本地目录。
初始化脚本会创建这些目录并设置为容器可写，同时把 `KIROCC_POSTGRES_UID` / `KIROCC_POSTGRES_GID` 自动填为当前宿主用户，避免 bind mount 在 Linux 或 Docker Desktop 下因为宿主目录所有者不匹配导致 Postgres 启动失败。

为避免同机冲突，部署没有固定 `container_name`。Compose 会根据 `KIROCC_DEPLOY_PROJECT` 生成容器、网络和内部资源名；同一台机器部署多套时，改这个变量即可。

常用部署命令：

```bash
docker compose --env-file .env.deploy -f docker-compose.deploy.yml pull
docker compose --env-file .env.deploy -f docker-compose.deploy.yml up -d
docker compose --env-file .env.deploy -f docker-compose.deploy.yml ps
docker compose --env-file .env.deploy -f docker-compose.deploy.yml logs -f app
docker compose --env-file .env.deploy -f docker-compose.deploy.yml down
```

## GitHub Actions 发布

### 需要配置的仓库 secrets

```text
DOCKERHUB_USERNAME
DOCKERHUB_TOKEN
```

GHCR 使用 GitHub Actions 默认的 `GITHUB_TOKEN`，workflow 已声明：

```yaml
permissions:
  contents: read
  packages: write
```

### tag 触发

推送符合 `v*` 的 Git tag 会触发发布：

```bash
git tag v1.2.3
git push origin v1.2.3
```

生成镜像标签：

```text
ghcr.io/2ue/kirocc-prox:1.2.3
ghcr.io/2ue/kirocc-prox:latest
docker.io/<DOCKERHUB_USERNAME>/kirocc-prox:1.2.3
docker.io/<DOCKERHUB_USERNAME>/kirocc-prox:latest
```

预发布 tag 例如 `v1.2.3-rc.1` 不会更新 `latest`。

### 手动触发

也可以在 GitHub Actions 页面使用 `workflow_dispatch`，输入不带 `v` 的版本号，例如：

```text
1.2.3
```

## 镜像运行参数

镜像默认监听容器内：

```text
KIROCC_HOST=0.0.0.0
KIROCC_ADMIN_HOST=0.0.0.0
KIROCC_POOL_STRATEGY=least-inflight
```

数据库和 Redis 推荐通过环境变量配置：

```text
KIROCC_POSTGRES_DSN
KIROCC_REDIS_ADDR
KIROCC_REDIS_PASSWORD
KIROCC_REDIS_DB
KIROCC_REDIS_KEY_PREFIX
```

`docker-compose.yml` 已按 service name 配好默认值。
