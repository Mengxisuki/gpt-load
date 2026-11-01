# 项目入口

用户可见回复、文档和自有注释默认使用简体中文。优先复用当前 Go、Vue 及上游构建能力，不修改不相关业务源码或依赖。

- 产品范围：[docs/memory/product.md](docs/memory/product.md)。开始任务先读，历史结论以当前源码与实际验证为准。
- 重要取舍：[docs/memory/decisions.md](docs/memory/decisions.md)。仅追加已落地事实。
- 排障经验：[docs/memory/lessons.md](docs/memory/lessons.md)。排障或改部署前检索。
- 私有账号执行：[docs/hosting-api.md](docs/hosting-api.md)。修改挂靠、凭据或调度前读取。
- 部署、分支与验证：[deploy/oak/README.md](deploy/oak/README.md)。生产暂时仅回环访问。

`upstream-main` 保持官方纯源码；定制、部署与 CI 文件只进入 `oak`，推送会触发自动发布。不得将管理密钥、渠道凭据、数据库、运行日志或本地构建产物提交到 Git。

本次新增部署入口不替代官方 CI。修改既有工作流前先检查 `internal/webui` 的工作流合同测试；修改数据库或业务能力时按影响范围补实际集成验证。
