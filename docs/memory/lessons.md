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


## 2026-11-01 18:53:12 +00:00 — Codex 挂靠测试不能复用普通凭据 Probe

现象：New API 调用挂靠 `accounts/:ref/test` 返回 `hosting_control_rejected`；执行层真实 HTTP 回归稳定复现 403。根因是 `hosting_http.go` 错误调用 `TestGroupCredential`，该入口明确拒绝订阅组；而 Codex 执行器也未声明 `OperationProbe`，只去掉门禁仍不能正确测试。

修复：挂靠入口独立使用 `TestHostingAccount` 构造最小原生 `ResponsesCreate`，指定所属账号和目录内模型，保留凭据准备/刷新、网络设置、安全结果分类及原通用探测禁令。暂停/删除账号和非本人请求在执行前拒绝，不返回恢复 proof，不重试上游失败。

验证：`TestHostingHTTPAccountProbeUsesSubscriptionResponses` 先红后绿，覆盖真实 HTTP 归属、请求契约、401/429/超时、暂停/删除零执行，并将入口实际 AttemptSpec 送入真实 CPA/凭据管理器，只隔离 HTTP transport。control、CPA、subscription 回归、vet 与构建通过。无数据库行为或认证契约变更，未部署或调用真实原厂；真实模型可用性仍需上线后验收。


## 2026-11-02 09:47:50 +00:00 — 连接测试不能只以传输成功确认可用

现象：旧挂靠探测只返回结果枚举，界面无法呈现发送内容与模型回复；HTTP 成功也可能对应无效、空文本或未完成响应。新增真实挂靠 HTTP 用例在旧实现上因缺少 `prompt/response_text` 失败。

修复：`internal/control/hosting_probe.go` 保留实际固定输入，按完成态、assistant 消息及 output_text 白名单提取文本；无有效文本为 inconclusive，不激活未验证账号。继续复用执行器已有 SSE 聚合，不在控制层重新解析流。只返回测试文本，不透传原厂错误、工具、推理、元数据或恢复证明。

验证：`internal/control/hosting_test.go` 的真实 HTTP、CPA/SSE 回归通过；New API DTO、前端文本展示同步。适用边界为 Codex 挂靠 Responses 测试，不改变普通管理探测或计费。没有访问真实原厂，不据模拟网络通过承诺真实账号连通。

## 2026-11-03 15:26:33 +00:00 — Responses 探测也需走完整反向转换

现象：为 OpenCode Chat 模型改了请求端点后，普通请求能成功，Responses 探测却被 `normalizeGatewayProtocolProbeResult` 判为协议无效。根因：探测走普通 Chat 请求结构，返回仍为 `chat.completion`，没有转换回声明的 Responses 协议。

修复：探测构造 Responses DTO 并复用 Compatible 的 Chat 执行与反向转换。`TestOpenCodeChatModelResponsesProbe` 在原路径以协议错误失败，修改后通过；同步保留原生 GPT 参数及未知模型原生路由。适用于声明客户端为 Responses、实际供应商仅提供 Chat 的模型探测，不能通过跳过协议校验伪造探测成功。

## 2026-11-04 00:57:13 +00:00 — 历史白名单不能阻断已有动态工具提升

已验证线上事实：北京时间 2026-11-04 08:49:18 起的 23 条 Kimi 请求已完成模型映射，但 GPT-Load 的 OpenCode 历史白名单返回 422，错误为 `this Chat target supports message and function-call history only`。执行记录均为 `not_sent`，New API 对外返回 502；订阅不足已回退钱包，并非终止原因。线上版本为 New API `960e688ca255`、GPT-Load `f2cf60f3498f`。这批请求没有 `request_log_bodies`，无法从日志判定具体 input 类型，不能断言全部由下述遗漏引起。

独立复现的代码遗漏：`prepareOpenCodeResponses` 将 `additional_tools` 当作不支持的执行历史，但 `compatible.go` 已实现动态声明提升。仅放行还不够，`allowed_tools` 校验发生在旧提升位置之前，会找不到动态函数。修复在原始工具类型校验后、工具选择校验前复用 `hoistCompatibleAdditionalTools`，顶层与动态声明统一校验，保留定义、调用和结果。不放行自定义/托管历史或加密状态；错误补充位置和已知类型，未知值不写日志。

验证：`TestOpenCodeResponsesStateAndToolContinuation` 新增动态函数及白名单用例先失败后通过，核对真实本机 HTTP 请求中的工具 schema、call_id、结果文本；拒绝嵌套不支持的工具并验证诊断脱敏。`go test ./internal/execution/bifrost ./internal/gateway ./internal/channel ./internal/dialect -count=1`、`go vet ./internal/execution/bifrost`、主程序构建通过。未提交、推送、部署或向真实厂商重放请求；原线上请求的具体失败类型仍待补充证据。官方 [Tool search](https://developers.openai.com/api/docs/guides/tools-tool-search) 将搜索调用与搜索结果定义为独立协议项，不能与动态函数声明混同。
