# Oak 内网部署

本仓库用于后续账号挂靠接入。当前部署提供 GPT-Load 原有的管理界面、渠道及凭据管理、分组与 AccessKey；尚未实现 Oak 用户自助挂靠、用户归属隔离和挂靠收费。不要把管理密钥发给挂靠用户。

## 访问方式

服务器为 `ssh wdm`，程序仅监听 `127.0.0.1:3001`，暂不设置域名、Caddy 反代或公网防火墙规则。同机 NewApi 后续可通过该地址连接，当前未自动新增 NewApi 渠道。

在自己的电脑建立隧道后访问 <http://127.0.0.1:13001>：

```sh
ssh -N -L 127.0.0.1:13001:127.0.0.1:3001 wdm
```

管理密钥由应用首次启动生成。仅在自己的终端读取，勿粘贴到工单或日志：

```sh
ssh wdm 'cat /var/lib/gpt-load/auth.key'
```

OAuth 回调端口 `1455`、`54545`、`51121` 不开放公网。远程浏览器可按授权对话框说明手动粘贴完整回调 URL，或按实际渠道建立对应的 SSH 隧道。

## 分支

| 分支 | 职责 |
| --- | --- |
| `upstream-main` | 纯跟踪官方 `upstream/main`，不加入 Oak 定制和部署文件 |
| `oak` | 开发、配置和 CI/CD；推送后通过门禁才发布 |
| `main` | 保留首次克隆内容，不作为定制发布入口 |

`origin` 是 `Mengxisuki/gpt-load`，`upstream` 是 `tbphp/gpt-load`。官方更新先快进跟踪分支，再评估合入开发分支，不自动把官方新提交发布到服务器：

```sh
git fetch --no-tags upstream main
git switch upstream-main
git merge --ff-only upstream/main
git push origin upstream-main
git switch oak
git merge upstream-main
# 处理冲突并验证后推送；此推送会触发部署。
git push origin oak
```

GitHub 默认分支应设为 `oak`，这样手动 workflow 入口与日常 PR 基准都落在开发分支。既有上游工作流保持原样；本 fork 的独立工作流使用 GitHub 托管 Ubuntu Runner，不依赖官方私有 self-hosted Runner。

## 服务器布局

| 路径 | 用途 |
| --- | --- |
| `/opt/gpt-load/releases/<版本>/` | root 拥有的只读程序、许可和版本元数据 |
| `/opt/gpt-load/current` | 当前发布目录的原子切换链接 |
| `/var/lib/gpt-load/` | 独立 SQLite、管理密钥与凭据加密密钥；应用用户独占 |
| `/etc/gpt-load/gpt-load.env` | root 专用环境文件，不进入仓库或构建包 |
| `/etc/systemd/system/gpt-load.service` | 独立非登录用户 `gpt-load` 的服务 |
| `/usr/local/sbin/gpt-load-ci-deploy` | root 安装的受限发布入口，对应本目录 `release.py` |
| `/var/backups/gpt-load/<版本>/` | 发布前的整目录状态、环境、旧路径及收到的包 |

不复用 NewApi 数据库、Redis、持久目录或部署密钥。新实例使用上游默认托管 SQLite，数据目录 `0700`，密钥与数据库 `0600`。**数据库和 `encryption.key` 必须成套保存**；丢失密钥将无法解密已有账号凭据。

服务内存软上限 `512 MiB`、硬上限 `768 MiB`，Go 软内存限制 `384 MiB`。实际并发能力需导入测试账号后评估，这些配置不代表已验证业务容量。

## CI/CD 接入

工作流为 [oak-deploy.yml](../../.github/workflows/oak-deploy.yml)。`oak` 推送自动检查、构建并发布；手动触发默认只构建，勾选 `deploy` 才连接服务器。

前端复用上游 `.github/actions/web-ci`，冻结安装 pnpm 依赖，执行 lint、格式、类型及真实前端构建；后端执行 vet、根模块与独立 CPA 模块测试。构建 Go 静态 Linux amd64 程序，执行原生烟测和发布格式测试后才上传产物。

首次配置需要：

1. GitHub 仓库默认分支为 `oak`，启用 fork 的 Actions。
2. 在仓库创建环境 `oak-production`，部署分支只允许 `oak`。
3. 为该环境添加 `GPT_LOAD_DEPLOY_SSH_KEY`，内容为独立私钥 `~/.ssh/gpt_load_github_actions_ed25519`。私钥由用户在 GitHub 或已授权 CLI 中录入，不进入聊天、代码或 artifact。
4. 服务器公钥限定为 `restrict,command="/usr/local/sbin/gpt-load-ci-deploy"`。已有管理 SSH 密钥不受影响，发布密钥不能执行交互 Shell、读取密钥或转发端口。
5. `known_hosts` 通过既有受信 `ssh wdm` 获取。Ed25519 指纹为 `SHA256:PEA/ySV7KRfOg+4Gkq2AeB1wGeVvvXucaGPt7rOa6bg`，不得关闭主机校验或扫描后盲目信任。

只有实际 workflow 构建及部署成功，才算 CI/CD 已接通；仅有文件、分支和 Secret 不代表运行成功。

## 发布与失败恢复

发布器先持锁、核对旧服务、接收有界输入，校验摘要、文件白名单、版本和 ELF 平台，再以应用用户运行 `version`。之后停止 GPT-Load、成套备份状态与配置、切换 `current` 并启动。只操作 GPT-Load，不重启 NewApi、Caddy 或数据库服务。

验收同时检查 systemd 活跃、回环监听、准确 `/health` 版本、真实前端入口与 JS、匿名管理请求拒绝、管理会话及数据库读取。`/health` 自身不持续探测模型上游；真实模型调用需另行配置账号验证。

新程序尚未尝试启动时，失败可恢复旧链接和服务；**一旦尝试启动就可能迁移数据库**，此后失败只停止新服务并报告恢复点，不盲目降级或覆盖数据。需要恢复时，管理员先停服，核对版本兼容，再决定修复前进或成套恢复状态、环境与旧版本。

备份保留在服务器，不自动清理；初次实例体积小，但上线请求日志增长后需评估保留策略与异机备份。当前没有配置周期备份或异机副本。

## 运维与验证

```sh
ssh wdm 'systemctl status gpt-load --no-pager'
ssh wdm 'journalctl -u gpt-load -n 100 --no-pager'
ssh wdm '/usr/local/sbin/gpt-load-ci-deploy check'
# 更新环境后只重启本应用，不使用内置update绕过CI替换定制程序。
ssh wdm 'systemctl restart gpt-load'
```

本地检查与构建（Go、Node 和 pnpm 版本以 `go.mod`、`web/package.json` 为准）：

```sh
python3 -B -m unittest discover -s deploy/oak/tests -v
pnpm --dir web install --frozen-lockfile
pnpm --dir web run lint
pnpm --dir web run format
pnpm --dir web run build
go vet ./...
go test -count=1 -timeout=15m . ./internal/...
(cd third_party/cpaembedded && GOWORK=off go test -count=1 ./...)
```

容器合同测试会运行 `docker compose config --no-env-resolution`，需使用支持此参数的 Docker Compose；本次原生部署不会在生产服务器安装 Docker。
