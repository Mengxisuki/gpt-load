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


## 2026-11-04 01:26:35 +00:00 — 新任务也会携带 grammar 自定义工具声明

已验证：新请求的 Caddy 原始正文显示客户端模型为 GPT，经 New API 映射到 Kimi；input 仅含一项 additional_tools 和普通消息，无 previous_response_id、conversation 或加密历史。动态工具声明包含 functions 命名空间的 exec，类型 custom，format.type=grammar、syntax=lark；另有 12 个普通函数工具。线上 GPT-Load 已发布 0d7c0e5b，additional_tools 已通过，随后的工具类型检查因 custom 拒绝，执行记录为 not_sent，客户端最终收到 502。

结论：这次不是缓存、未发布或旧会话，而是当前 Chat 转换仅支持函数工具，无法表达客户端的 grammar 自定义工具。检查覆盖所有声明，不需要真正调用 exec 就会失败。此前“先解决新任务”的实现并没有覆盖此客户端完整工具合同；仅修动态工具声明不足以支持该请求。

后续边界：完整支持需要评估 custom 声明、输入、普通/流式返回及下一轮调用结果的双向适配，并明确 grammar 约束如何保留，不能简单放行后让 SDK 丢弃或删除工具冒充成功。本轮仅确诊，未实现该适配或改生产。Caddy 已按大小轮转，查询时需扫描当前及 gzip 归档并按 request_id 字段精确匹配，不能仅全文搜索：后续聊天正文可能引用旧请求 ID。只提取类型、位置及已知工具名，原始任务与 grammar 正文不进入仓库。

## 2026-11-04 01:55:03 +00:00 — Chat 线协议不能沿用 Responses 的命名空间能力判断

现象：实现已知 exec 适配后，混合默认 custom 与其他 namespace 同名函数的真实 HTTP 测试发现，普通命名空间工具仍被静默丢弃。根因：`prepareCompatibleChatRequest` 根据模型的 Responses 命名空间能力决定是否展平，但随后实际调用 `ToChatRequest`；SDK 默认认为 OpenAI 支持 namespace，而 Chat wire 无法保留容器。

修复：实际 Compatible Chat 发送前始终提升动态工具、解包默认空间、复用 SDK 展平其他空间，回包先恢复名字再识别 custom exec。同步更新旧测试中错误的丢工具预期。`TestOpenCodeExecParallelStream` 证明两个 custom 与一个其他空间的同名普通函数可并行保留，`TestOpenCodeExecMappedGateway` 验证真实入口映射、响应恢复和日志协议。

另一已复现边界：SDK 可将工具结束与用量拆成不同事件，格式错误后立即退出会丢已消费用量。流恢复先停止发布内容，继续读取上游尾部，再带观察到的用量报错；普通响应同样附带消费证据。`TestOpenCodeExecRejectsMalformedOutput` 覆盖损坏参数、重复字段、尾随 JSON 及独立晚到用量尾块，不泄漏源码、不发送 completed。

验证：执行层、网关、渠道、dialect、provideradapter 相关回归及 exec 定向竞态检查通过。本机真实 HTTP 覆盖两轮文本/数组结果、工具选择、顶层/默认空间、原生 GPT 隔离与坏声明零派发。只对已知 grammar 作等价转换，不能泛化任意 custom 工具。上述是本地实现事实，取代前条“尚未实现”的当前状态；线上调用仍待发布后验收。已确认等价的公开工具文法作为协议常量和测试夹具入库，不含实际任务正文。

## 2026-11-04 02:12:35 +00:00 — 同一请求先失败后成功需对齐发布窗口

现象：用户在 exec 适配发布附近观察到七次 GPT→Kimi 失败后一次成功。Caddy 原始 JSON 比较确认，前五次正文完全相同；后两次失败与最终成功正文也完全相同，两组仅 `client_metadata.x-codex-turn-metadata` 不同，输入消息与工具声明相同。

已验证根因：02:07:34–40 UTC 的五次请求仍由旧版 `0d7c0e5b` 处理，因 custom 声明返回 422，执行状态 `not_sent`，New API 对外 502。02:07:41.438 收到停止信号，02:07:43–44 两次请求连接回环 3001 被拒绝。02:07:44.919 新版 `26de9480` 就绪，紧接着用户的重复请求成功经 Chat 转换由 Kimi 完成；并非新版对相同业务输入随机通过或失败。

同时两个在途原生 GPT 流在停止时被取消。发布器 `deploy/oak/release.py` 采用 stop→备份→切换→start；`internal/app/app.go` 的 Stop 先取消请求再 drain，所以日志中的 graceful drain 不能解释成所有生成任务都自然完成。排障应对齐 Caddy 的真实请求 ID、正文差异、两层执行日志与 startup.ready 版本，不能仅凭 HTTP 200 判定 SSE 完整，也不能仅凭最新健康版本判断历史失败运行在哪一版。

本次只读线上日志和数据库，没有重放真实请求或修改服务。后续验证应在 ready 之后发起；若需要消除发布中断，属于部署生命周期的独立改造，不能通过修改模型适配规则掩盖。

## 2026-11-04 02:19:16 +00:00 — exec 初次成功不等于续轮历史正确

已确认新问题：02:14:15 UTC 的请求运行在 `26de9480`，GPT→Kimi 映射生效，但 `convertHistory` 报 `duplicate exec history call_id`，422 / not_sent，New API 对外 502。该问题不属于前条 02:07 的发布窗口。原始 input[21]、input[22] 是两个完全相同的 custom 调用，call_id 都为 `exec:0`；input[23]、input[24] 是对应两个输出，输出内容相同，仅结果项 ID 不同。不是已完成的两个顺序回合复用了 ID，而是两个调用后跟两个结果。

前驱请求始于 02:12:38 UTC、02:13:23 完成，输入没有 custom 调用历史；02:13:25 的下一请求已经带上述重复记录。可以确认重复产生在前驱响应到后续回传之间，不能仅凭后续请求归咎于模型、转换器或客户端。Caddy 当前只留存请求，GPT-Load 也没有该响应流记录，因此重复来源尚未证实。现有历史拒绝避免了歧义执行，不能直接去重或放行而掩盖重复执行风险。

源码线索但非线上定论：锁定 SDK 的 Chat→Responses 转换每遇到 finish_reason 都可能再次发出工具完成事件，需用重复结束块及带用量尾块复现并核对客户端可见事件次数；不能据此断言真实上游就发送了这种序列。本轮未修改业务代码、生产配置或重放任务。后续诊断见 `docs/handoff.md`。

## 2026-11-04 04:14:15 +00:00 — 带 role 的重复结束块会重新发布工具完成事件

新请求的线上事实：04:07:06 UTC 的请求仍运行 `26de9480`，映射 Kimi 后因 `duplicate exec history call_id` 被本地拒绝，not_sent / 422，New API 502。input[9]/[10] 为同 ID、同源码的 custom 调用，[11]/[12] 为同内容结果，再次出现上条的历史形状。未留存生成这些调用的原始响应流，不能断言真实上游一定使用下述尾块。

本地已复现：合成上游先发一次 exec 与 finish，再发仅带 role=assistant 和 finish 的尾块。SDK 会转发带 role 的尾块，Chat→Responses 状态机再次关闭同一工具；客户端可见 input.delta、input.done、output_item.done 均出现两次。按 done 生成续轮历史后，真实执行器报同样的 duplicate exec history call_id。仅空 delta 的重复结束块会被 SDK 过滤，不能泛称所有重复 finish 都触发该缺陷。

修复：`compatible_stream.go` 以已经生成的首个终态为边界，继续收取尾部用量及上游错误，但不再转换无输出的尾块；终态后的正文、推理或工具增量明确报错并保留已观察用量，禁止静默截断或重复执行。普通函数和 custom 共用边界，不放宽历史重复校验，不擅自去重已有历史。

验证：`TestOpenCodeExecTerminalTrailer/repeated_role_finish` 先红（两次事件且续轮重复 ID）后绿，覆盖尾部用量、晚到错误、新输出拒绝及实际续轮；普通并行函数测试也检查每个调用只完成一次。执行层、网关、渠道、dialect 回归、exec/Compatible 定向竞态、vet 与构建通过。仅本地修改，未提交、推送、部署或调用真实模型；线上触发条件及新版本实测仍待确认。
