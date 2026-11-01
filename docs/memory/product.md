# 产品与部署现状

更新时间：2026-11-01 07:52:24 +00:00。

本仓库是 `Mengxisuki/gpt-load` 对官方 `tbphp/gpt-load` 的 fork，用于 Oak 后续账号挂靠模式的接入基础。当前保留 GPT-Load 的渠道、订阅账号 OAuth/凭据导入、分组、AccessKey、路由及日志能力，没有新增用户自助挂靠、用户账号归属绑定或收费功能。

管理员可以录入用户已有的模型商账号，再按 Group 分配受限 AccessKey；管理 AUTH_KEY 拥有整个控制面权限，不得分发给挂靠用户。真实模型请求、账号独占及用户隔离仍需后续明确设计和验证。

用户明确选择先仅部署内网。`ssh wdm` 上已有独立 `gpt-load` systemd 服务，仅监听 `127.0.0.1:3001`，使用 `/var/lib/gpt-load` 的托管 SQLite 与自动生成密钥；不配置公网域名，不修改 NewApi/Caddy/Redis。已实际通过健康版本、前端与JS、匿名拒绝、管理读接口及重启密钥持久化验证。

分支 `upstream-main` 与官方 `upstream/main` 同源纯跟踪，`oak` 承载开发与专用发布工作流，原 `main` 保留。首次上线应用源码为 `5d1d71c246d22bdf3578bbfd8dd1296e569454a7`，版本为 `oak-5d1d71c246d2-0-1`；后续版本以服务器 `/health` 和 GitHub Actions 实际结果为准。

部署与接手命令见 [deploy/oak/README.md](../../deploy/oak/README.md)。GitHub 默认分支、环境 Secret 和首次 Actions 发布须在接通后更新本节；当前不能把本地分支和工作流文件当作已验证的自动发布。
