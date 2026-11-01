# 排障经验

## 2026-11-01 07:52:24 +00:00 — 原生健康、管理API与测试工具分别核对

现象：GPT-Load缺少前端构建时仍可返回成功健康页；管理API采用code=0而非NewApi的success；本机Go回归的容器合同测试因Docker Compose2.31.0不支持--no-env-resolution失败。

根因：健康只代表进程启动，API响应合同和开发工具要求属于各自项目，不能沿用另一个项目的判断。Go官方模块代理在本机也出现连接超时，切换可用镜像后才完成依赖下载。

规避：发布同时验证准确版本、真实首页/JS、匿名拒绝和带密钥的管理及数据库读取。冻结依赖、按项目指定Go与pnpm构建；不为本机Compose版本问题修改上游业务或跳过合同测试。服务器首次实际探测和重启持久化已通过；后续用隔离的Docker Compose2.39.4实际复跑完整Go测试，全部通过，没有修改测试或生产Docker配置。

适用边界：本项目原生发布与本机开发环境。没有模型账号时不把启动、目录或健康成功写成真实模型调用成功。发布器中断要跨越同一个恢复边界，临时链接须独立且可清理，InterruptedError不能被网络重试吞掉；对应回归见`deploy/oak/tests/test_release.py`。

## 2026-11-01 14:40:54 +00:00 — 私有门禁须覆盖专用目录，重置互斥不能锁住后续观测

现象：普通模型目录隔离后，Codex 专用目录仍有独立空组放行代码；在整个重置函数上加互斥会阻塞已成功操作的重放，已有并发观测测试无法完成。

根因：`clientcatalog.Candidates` 独立枚举路由；成功消费后还会等待额度刷新，但消费结果此时已经持久化。把整个消费和只读观测视作同一临界区扩大了互斥范围。

修复：普通/Codex 目录、WebSocket 建连和续轮、个人模型视图统一调用 `ConfigSnapshot.AllowsGroup`。消费只在操作建立前与解绑串行；prepared 持久化后由数据库未决状态阻止删除，释放互斥允许成功结果查询和重放。测试见 `TestHandlerHostingPreventsUnownedExecution`、`TestHostingVisibleModels`、`TestConsumeCredentialResetCreditReplayReportsPendingWhileObservationRefreshRuns`、`TestHostingHTTPResetReadAndDeleteGuard`。

适用边界：当前单实例 gpt-load 挂靠执行。测试替换外部 HTTP/执行器，未验证真实原厂消费；不能把本地幂等测试当作厂商行为承诺。
