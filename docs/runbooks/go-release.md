# Go 构建、备份恢复与灰度准备

Status: current

## 适用范围

适用于 `Dockerfile.go` 和独立的 `docker-compose.go.yml`：Go 后端、原有 Vue、
PostgreSQL 18 与 Redis 7。Python 的 Dockerfile、Compose 和业务源码保留。
数据库与文件边界遵循 [ADR 0011](../adr/0011-explicit-go-release-and-quiesced-restore.md)。
本手册不表示已经发布镜像或切换生产流量。

每个任务/图片数据目录只能有一个活跃 Go 应用进程。Redis 只缓存成功文本响应，
不保存图片任务，也不提供跨实例账号并发调度。不要把 Compose 扩成多个应用副本。
容器通过编排器更换镜像；容器内的受管二进制在线安装默认关闭。

## 拉取 GHCR 镜像并启动

Go 专用预发布使用 `v<version>-go.<n>` 标签；当前配置固定为
`ghcr.io/heiyuan0801/chatgpt2api-go:v3.2.3-go.2`，发布目标为 Linux amd64 和
arm64。Docker 按宿主架构自动选择，不在 Compose 中固定 `platform`。
首次发布后需在 GitHub 软件包设置中把该包设为 Public，并通过匿名拉取验证。

不需要克隆源码或在服务器编译。下载同一标签的 Compose 和配置模板：

```bash
mkdir chat2api-go
cd chat2api-go
curl -fL https://raw.githubusercontent.com/heiyuan0801/chat2api/v3.2.3-go.2/docker-compose.go.yml -o docker-compose.go.yml
curl -fL https://raw.githubusercontent.com/heiyuan0801/chat2api/v3.2.3-go.2/deploy/go.env.example -o .env.go
chmod 600 .env.go
```

编辑 `.env.go`，把认证密钥、PostgreSQL 密码、Redis 密码三个 `replace_with_...`
占位值替换为各自独立的随机值，可用 `openssl rand -hex 32` 生成。默认仅监听
`127.0.0.1:3001`；需要直接远程访问时，将 `CHATGPT2API_GO_BIND` 改为
`0.0.0.0`。设置 `CHATGPT2API_BASE_URL` 为客户端可访问的地址，以生成图片和
文件链接。数据库与 Redis 不暴露宿主端口。

```bash
docker pull ghcr.io/heiyuan0801/chatgpt2api-go:v3.2.3-go.2
docker compose --env-file .env.go -f docker-compose.go.yml config -q
docker compose --env-file .env.go -f docker-compose.go.yml pull
docker compose --env-file .env.go -f docker-compose.go.yml up -d --wait postgres redis
# 仅首次部署空数据库时执行；现有兼容数据库不重复初始化。
docker compose --env-file .env.go -f docker-compose.go.yml run --rm --no-deps init
docker compose --env-file .env.go -f docker-compose.go.yml up -d --no-build --wait app
docker compose --env-file .env.go -f docker-compose.go.yml ps
```

使用 `.env.go` 中的 `CHATGPT2API_AUTH_KEY` 登录控制台。保留 `app-data` 和
`postgres-data` 命名卷；普通停止使用 `docker compose ... down`，不带 `-v`。
Redis 仅作可丢弃的文本缓存。备份、恢复和回滚需要完整源码中的运维脚本，
操作步骤见下文。

## 从源码构建与启动

需要 Docker Engine、Buildx、Compose v2 或更新版本。运维命令使用 Bash 和
`sha256sum` 或 `shasum`。数据库密码和 Redis 密码仅使用 URL 安全的字母、数字、
下划线或连字符；环境文件和备份属于敏感数据，不纳入 Git。

1. 在完整检出的源码中准备独立配置：

   ```bash
   cp deploy/go.env.example .env.go
   chmod 600 .env.go
   # 源码构建时把 CHATGPT2API_GO_IMAGE 改为 chatgpt2api-go:local。
   # 编辑私有认证密钥、两个服务密码、项目名和端口。
   docker compose --env-file .env.go -f docker-compose.go.yml config -q
   ```

2. 本地构建 Go 镜像。构建过程会从锁文件安装前端依赖、构建原 Vue，并编译
   原生 Go 命令；不使用宿主机的 `dist`、测试目录或账号数据。

   ```bash
   docker compose --env-file .env.go -f docker-compose.go.yml build app
   docker compose --env-file .env.go -f docker-compose.go.yml up -d --wait postgres redis
   docker compose --env-file .env.go -f docker-compose.go.yml run --rm init
   docker compose --env-file .env.go -f docker-compose.go.yml up -d --wait app
   bash deploy/go-ops.sh verify
   ```

   `init` 显式创建兼容表后退出，不启动 HTTP、后台任务或图片工作器。已有完整
   数据库正常启动不需要执行它。`verify` 检查带认证的设置读取、PostgreSQL 和
   Redis；预期输出 `Go HTTP, PostgreSQL and Redis checks passed.`。
   默认控制台地址为 `http://127.0.0.1:3001`，API 前缀为 `/v1`。

3. 确认实际版本和模型列表，再用独立测试 User Key 检查所需操作。不要仅以
   容器 healthy 判定真实上游可用。服务日志不要复制到公开 Issue；环境和 SQL
   备份可能包含凭据。

默认使用独立命名卷，不接管旧 Python 的数据库或 `data/`。升级时把配置中的
`CHATGPT2API_GO_IMAGE` 固定为经过验收的镜像摘要，保留旧镜像用于回退。
配置 `CHATGPT2API_BASE_URL` 后再使用公开图片链接。

## GitHub 构建与正式发布

`.github/workflows/go-verify.yml` 在本仓库 `main`、`codex/**` 推送、手动触发或被
发布 workflow 调用时执行验证。它构建 Vue、校验依赖/路由、编译 Go、打包
Linux/Darwin 的 amd64/arm64，并用实际 Linux 容器启动 PostgreSQL/Redis、备份、
独立恢复和回滚。被 Git 忽略的本地完整测试不随 CI 上传；CI 的 `go test ./...`
只覆盖干净源码中的包编译，不能冒充本地完整回归。

验证成功后，Actions 提供保留 7 天的下载产物：

- `go-native-bundles-<commit>`：四个平台的原生包和 `checksums.txt`。
- `go-container-linux-amd64-<commit>`：经过容器验证的 Docker 镜像归档，解压后
  可用 `docker load` 导入。该产物没有推送到镜像仓库。

仅在另行授权推送匹配 `VERSION` 的 `v*` 标签后，`docker-publish.yml` 才创建
Release、合并 Python/Go 包校验和，并推送独立的
`ghcr.io/<owner>/chatgpt2api-go` 多平台镜像。PR 和手动触发不会发布，即使手动
选择了标签。`main` 的验证构建是本次用户明确授权的规则例外。

包含 `-go.` 的 Go 专用预发布标签只执行 Go 镜像发布，跳过 Python 镜像与
混合运行时 Release；验证产物仍可在 Actions 下载。`VERSION` 必须与标签去掉
前缀 `v` 后完全一致。镜像同时标记固定版本、无 `v` 版本和 `latest`；部署使用
固定版本或摘要。普通 `v*` 标签保留原有 Python/Go 联合发布行为。

CI 原生包和镜像将更新检查绑定到当前 GitHub 发布仓库。独立原生打包时可用
`CHATGPT2API_RELEASE_REPOSITORY=owner/repository` 指定来源；默认仍是上游仓库。
不得把 Python 更新包用于 Go 受管运行目录。

## 修复旧镜像的账号同步网络错误

`v3.2.3-go.1` 缺少系统 HTTPS 根证书，账号与额度同步可能显示
`network_error at account_info (HTTP 0)`；该数字表示未收到 HTTP 响应，不能据此
判断 Token 无效或额度为零。`v3.2.3-go.2` 从 Go 构建镜像带入系统信任根，保持
TLS 证书验证开启。CI 检查运行时证书文件存在且包含 PEM 证书。

已有部署保留原环境文件、数据库与数据卷，只更新 `.env.go` 中的
`CHATGPT2API_GO_IMAGE`，然后执行：

```bash
docker compose --env-file .env.go -f docker-compose.go.yml pull app
docker compose --env-file .env.go -f docker-compose.go.yml up -d --no-build --wait app
```

不重新初始化已有数据库。进入账号页，仅对受影响账号执行同步，核对远程检查
成功及实际上游返回的图片额度。若仍失败，继续检查容器 DNS、出站网络和代理；
不关闭 TLS 验证，也不把网络错误当成账号失效删除。该修复不包含生图额度赠送
或固定额度，额度始终以上游结果为准。

## 停写备份

适用于该独立 Compose 自管的 PostgreSQL 与数据卷。不要用此命令操作其他应用
共用的数据库或 Redis。确保没有外部进程正在写同一 SQL/文件状态。

```bash
bash deploy/go-ops.sh backup /absolute/private/new-backup-directory
```

命令要求新建的绝对路径，先停止原来运行的应用，生成 PostgreSQL custom dump、
任务/图库/可编辑文件归档、版本、原镜像 ID 和校验和，验证后重新启动原应用。
原来停止的应用保持停止。备份失败不会替换业务数据；检查磁盘、服务与日志后
使用另一个新目录重试。备份目录权限为 700，文件默认 600。

此停写备份覆盖 SQL 与文件；不复制正在运行的 PostgreSQL 数据目录。Redis 可
丢弃，不归档。可编辑任务的 `files/.staging` 输入不进入备份；未完成任务不会
在恢复后自动重新提交。保留备份原件和捕获的旧镜像，不执行镜像清理。

## 恢复到独立空环境

恢复不是覆盖命令：运行中的应用、非空数据库或非空文件卷都会被拒绝。应使用
不同的 Compose 项目名、新数据卷和不同端口。复制必要配置时保留原 User Key
认证配置，给新部署设置独立 Redis namespace；不要把原配置文件放进备份归档。

```bash
export CHATGPT2API_GO_PROJECT=chatgpt2api-go-restored
export CHATGPT2API_GO_PORT=3002
export CHATGPT2API_REDIS_NAMESPACE=chatgpt2api-go-restored
bash deploy/go-ops.sh restore /absolute/private/backup-directory
docker compose --env-file .env.go -f docker-compose.go.yml up -d --wait app
bash deploy/go-ops.sh verify
```

恢复前不运行 `init`。命令先验证校验和、PG 归档及文件路径/类型，再检查空目标；
数据库在单事务内恢复，文件由归档流恢复。仅清空这个新项目专用 Redis 的缓存。
恢复成功后应用仍停止，必须显式启动并核对账号/User Key 稳定 ID、Call Record
数量、设置、图库下载和文件哈希。图片任务内部原账号引用也应保留。

开始写目标前会建立 `.restore-incomplete` 标记，后续任一步失败都会保留它；
实际容器入口拒绝启动。此时保留失败目标排查，换一个新项目/空卷重做；不要
仅删除标记然后上线。校验失败的归档和目标数据都不能作为恢复成功的证据。

## 镜像回退与 Python 退路

在同一个 Go 项目、兼容 SQL/文件格式的前提下，回退到备份捕获的旧本地镜像：

```bash
bash deploy/go-ops.sh rollback /absolute/private/backup-directory
bash deploy/go-ops.sh verify
```

命令验证备份和旧 Go 镜像身份，停止当前应用，用旧镜像启动并等待健康，保留
现有业务数据。之后把环境文件的镜像值也固定为该旧镜像，避免下次普通 `up`
又选中失败版本。旧镜像已被清理时，先导入经过校验的镜像归档；不要用可变
`latest` 代替先前版本。若旧镜像也不健康，保持入口停止并排查，不继续切流。

退回 Python 时也必须先停止 Go 及其后台维护，使用已备份并核对过的 Python
版本、数据库和文件副本重新启动。不要让两个任务所有者同时打开同一个目录，
也不要恢复旧 SQL 后继续使用较新的文件状态。本轮隔离验证覆盖原 Python
Repository 读取恢复后的 PG18 数据，未执行目标生产主机的 Python/Go 切换。

## 灰度准备与目标环境门禁

本地验证通过后，先为灰度准备独立域名/路由、项目、数据库、文件卷、Redis
namespace、专用 User Key 和专用 Upstream Account。原生产池不能同时调度这些
专用账号。先验证完整控制台和任务链路，再讨论流量比例；不要把任务提交与
轮询随机分发给两个拥有不同文件状态的实例。

反向代理应保留 Authorization、原始 Host 与协议，并关闭流式响应缓冲。示例
适用于目标主机上的 Nginx location，实际域名/证书仍由部署者配置：

```nginx
location / {
    proxy_pass http://127.0.0.1:3001;
    proxy_http_version 1.1;
    proxy_set_header Host $host;
    proxy_set_header X-Forwarded-Proto $scheme;
    proxy_set_header Authorization $http_authorization;
    proxy_buffering off;
    proxy_cache off;
    proxy_read_timeout 1300s;
    proxy_send_timeout 1300s;
}
```

正式切流前在目标环境验证 TLS/反代后的登录、模型、文本 SSE、参考图编辑、
实际图片下载、超时原账号续查、账号额度、进程重启和回滚。记录失败率和耗时，
上游工具失败与本地执行错误分开统计。当前没有指定目标主机、域名或流量比例，
因此本手册仅准备灰度，不能据此宣称生产已经上线或具备持续容量结论。
