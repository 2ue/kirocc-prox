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

## 本地运行完整服务

默认 `docker-compose.yml` 会启动：

- `kirocc-pro`: 代理和管理后台
- `kirocc-pro-postgres`: PostgreSQL 持久化账号、设置、使用记录、配额快照
- `kirocc-pro-redis`: Redis 运行时调度状态

```bash
docker compose up -d
```

默认端口：

```text
Proxy: http://127.0.0.1:9326
Admin: http://127.0.0.1:3457
```

建议生产环境至少设置：

```bash
export POSTGRES_PASSWORD='change-me'
export KIROCC_ADMIN_KEY='change-me'
docker compose up -d
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
