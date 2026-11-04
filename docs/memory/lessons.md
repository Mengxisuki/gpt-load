# 排障经验

## 2026-11-04 05:20:11 +00:00 — 模型端点表不能代替 Codex 接入及搜索能力核查

纠正前述排查前提：[OpenCode Codex 接入文档](https://opencode.ai/v2/docs/console/harnesses/codex/)明确说明 Console 为不接收 Responses 的模型转换请求，Go 示例为 `kimi-k3`、`wire_api="responses"`、`https://opencode.ai/inference/go/openai/v1`。因此不能从模型表中的 Chat 端点推断 Kimi/GLM 不能直连 Responses，也不能把另购搜索服务当作唯一方案。当前 GPT-Load 的 `resolveOpenCodeResponsesRoute` 按三个模型前缀强制 Chat，`prepareOpenCodeResponses` 拒绝 web_search；这是本地已确认的拦截点，是否应改为上游直通需结合真实工具行为验证。

另一方面，[开放 issue #57397](https://github.com/anomalyco/opencode/issues/57397)报告同一 Go Responses 入口因所选上游不同，存在转换路径丢弃 web_search、原生路径返回 web_search_call 的差异；已读取完整正文、评论及未合并 PR #42231 的历史补丁。此为报告者实测，非本项目验证或官方能力承诺。现行托管推理转换代码不在已查公开 dev 树内（本次树 SHA `55276f2108c8853f94cf16dc530e7754a6ce8318`），不能拿历史补丁当线上代码。

[Console Hosted Web Search](https://opencode.ai/v2/docs/console/websearch/)是已存在的独立搜索服务，文档要求工作区启用、每次成功搜索 $0.01；页面描述 OpenCode 客户端使用它，尚不能证明 Codex 的 Responses web_search 自动接到它。后续首先对比具体模型直连 Responses 与 Oak 转换路径，验收搜索调用、真实来源、流式终态和续轮；HTTP 200 或能回答普通问题不等于搜索通过。此次未调用真实模型、未改生产，不擅自丢工具、改 store 触发上游回退或扩大收费。

## 2026-11-04 05:12:49 +00:00 — 会话头缺失先核对入口与实际透传配置

只读核查 wdm：New API 的 OakGL/OakGL_GPT 渠道已配置 `header_override={"*":true}`；`relay/channel/api_request.go::processHeaderOverride` 按键启用透传，值不参与解析。GPT-Load 的 `safeRequestHeaders`/`safePassthroughHeaders` 未过滤 Session-Id 或 X-Opencode-Session。不能仅因适配器默认不复制这些头就认定线上丢头。

Caddy 精确请求记录证明：04:38:54 UTC 两条上游报缺会话头的 Chat 请求在入口就没有 Session-Id、Thread-Id 或 X-Opencode-Session；成功 GLM Chat 携带 X-Opencode-Session。两条被 web_search 拦截的 Responses 请求携带 Session-Id/Thread-Id，直接失败仍是本地托管工具合同拒绝。没有证据支持新增随机会话或强制改名；本轮未改业务代码或生产配置。

[OpenCode Go 文档](https://opencode.ai/docs/go/)说明要求稳定会话，支持 Codex 原生会话头，代理应保留。[OpenAI 搜索文档](https://developers.openai.com/api/docs/guides/tools-web-search)中的 Responses web_search 是服务端执行的托管工具；已查 Go/Zen 文档未确认 Kimi/GLM API 提供此合同，不能据客户端能联网推断 API 自带搜索。当前只完成源码、配置与入口日志核查，未重放供应商请求；搜索后端与完整适配方案仍待确定。

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

## 2026-11-04 05:04:25 +00:00 — 简单 Chat 成功不能验证携带托管工具的 Responses 请求

线上已运行 `oak-c2f7279be06b-41772333177-1`，启动时间为 04:21:59.487 UTC，上一条尾部事件修复已发布。用户指认的 04:31:15 Kimi 与 04:38:02 GLM Flash 请求均为 `/v1/responses`，只有四项普通消息历史，但 `tools[8]` 声明 `web_search`。无 custom 调用历史、旧响应引用或加密历史；被 `opencode_responses.go` 工具白名单拒绝，422 / not_sent，Caddy 对外也记录 422。不是重复 exec 或未发布问题。

对照成功请求 04:39:25 使用 `/v1/chat/completions`、model=glm-5.3，仅有 model/messages/max_tokens/stream，没有工具声明，走 OpenCode Go 原生 Chat；日志为 success、usage complete，New API 收到 done。它还携带 X-Opencode-Session，而两条指定 Responses 请求没有该头。04:38:54 的另外两条 GLM 上游请求明确返回 400：缺少 x-opencode-session；这是独立的渠道要求，不能当作两条 not_sent 请求的当前错误原因。另有一条 Kimi 上游 403，仅摘要不足以确定细因。

结论：可调用模型不等于兼容客户端默认工具集合。已知 exec 的桥接不等于实现 OpenAI 托管 web_search，即使模型尚未选择搜索，声明也会被拒绝。不能为通过请求而静默删除工具或伪装搜索结果。若继续实施，应先明确客户端不声明该托管能力，还是通过真实可执行的搜索后端实现完整合同；同时核对渠道的合法会话标识传递。此次只读核对日志与请求，未改业务、配置或重放任务。


## 2026-11-04 06:28:35 +00:00 — 原生转发不等于上游目标支持全部 Responses 参数

只读确认 wdm 已于 06:21:55 UTC 启动 `oak-739fc7992019-41784588956-1`，包含移除 OpenCode 本地转换的改动。用户请求 `202611040624117009409948268d9d6rhdvuvY7` 经 New API 将 GPT 别名映射为 kimi-k3，执行日志记录 OpenCode Go、native、openai-responses；上游返回 400 `reasoning.context: this reasoning option requires a native Responses target`，New API 对外 502。对应时间窗口的执行记录为 `17e02de5-9b3b-4ca7-bf68-8fa1dbc047b6`，150ms、response_started=true，失败来源 upstream。

入口正文只检查结构：reasoning 为 effort=high、summary=detailed、context=all_turns；stream=true、store=false，input 含 additional_tools 和普通消息。入口带 Session-Id/Thread-Id，订阅预扣不足已成功改走钱包。该次错误不是未部署、缺会话头或本地 web_search 拒绝；后续同窗口 Kimi 请求也返回相同参数错误。

边界：GPT-Load 的 native 指不在本层转换；上游报错中的 native Responses target 指其所选模型执行能力，二者不等同。官方支持 Codex 接入不能保证所有 GPT 专属参数都受非 GPT 路径支持。最小后续验证应比较同一合成原生请求有无 reasoning.context，再检验搜索/custom/续轮，不能未经验证恢复整体 Chat 转换或宣称删一个字段后完整工具链必然成功。本轮未改生产、未调用真实模型、未删除参数。


## 2026-11-04 06:45:51 +00:00 — Python 默认客户端标识会干扰 OpenCode 对照测试

现象：通过线上 GPT-Load 发出的两个合成 Kimi Responses 请求（无 reasoning、仅 high/detailed）都返回 403 非 JSON，执行日志为 `error code: 1010`。与用户失败请求使用同一渠道组和供应商凭据，但用户 Codex 请求可以进入参数验证并返回 reasoning.context 400。两者不能混同为模型能力或账号额度错误。

依据：[Cloudflare 1010 官方说明](https://developers.cloudflare.com/support/troubleshooting/http-status-codes/cloudflare-1xxx-errors/error-1010/)将该错误解释为客户端签名拒绝，明确提及 Python urllib 默认 User-Agent，并建议发送真实、描述性的应用标识。GPT-Load 原生过滤允许 User-Agent 透传。

验证：测试脚本设置 `User-Agent: Oak-Compatibility-Check/1.0`，不冒用 Codex/浏览器、不修改线上配置，再向相同本机 GPT-Load 入口发送两类合成请求。两者均 200/completed，输出 reasoning+message、完整用量；max_output_tokens=128、store=false。供应商报告 balance cost=0、消耗 Go Plus 套餐额度。本次结果排除了测试标识造成的阻碍，证明最小请求可接受 effort=high/summary=detailed，不代表完整 Codex 的搜索、custom、动态工具和续轮已验收。此条纠正此前“真实对照无法继续”的当前状态；原失败记录保留。


## 2026-11-04 06:55:27 +00:00 — Kimi 原生工具链可用，但当前路径未执行托管搜索

在 wdm 当前版本 `oak-739fc7992019-41784588956-1`，使用真实应用 UA、现有 New API 渠道 1 的数据面凭据，经回环 GPT-Load 向 OpenCode Go 发出 19 次合成 Responses 请求（06:51:24–06:54:53 UTC）。日志均为 group=2、credential=2、native/openai-responses；14 次 HTTP 200、5 次明确 400，无 403。测试没有经过 New API 用户 Key 入口，因此不代表本轮重新验证了 Key 映射/权限/扣费；没有改生产配置、执行生成代码或重放用户任务。成功请求报告 Go Plus 用量，消耗套餐额度。

| 验证 | 实际结果 |
| --- | --- |
| reasoning.context=all_turns | 400，复现 requires a native Responses target；前次无 context 的 high/detailed 已通过 |
| 普通 function，auto | 实际 function_call，提交其真实 call_id 的合成结果后准确回复结果 |
| input.additional_tools | 动态函数实际产生 function_call；不是仅接受声明 |
| custom text | 实际 custom_tool_call，input 为预期合成文本 |
| 动态 namespace/custom grammar，流式 | 保留 functions/exec，工具完成和终态各一次，续轮准确读取结果 |
| 普通函数流式与续轮 | 参数完成及工具完成各一次，每轮一个 completed，续轮准确读取结果 |
| Codex 真实 exec 文法，连续三回合 | 两轮调用 text(1)，call_id 不同，第三轮准确完成；只回传合成结果，不执行代码 |
| Codex 其他字段组合 | 上述三回合携带 high/detailed、include=reasoning.encrypted_content、parallel_tool_calls、text.format、prompt_cache_key、client_metadata；接受请求不等于所有提示参数都有等价语义 |
| client tool_search | 实际 tool_search_call，arguments.query 正确；未验证发现结果回传与调用被发现工具的完整续轮 |
| web_search，auto 与 required | 都返回 200，但无 web_search_call、无引用，模型明示没有搜索能力；required 也只返回普通文本，不能计为搜索成功 |
| 强制指定 function | 400，specified 与 thinking enabled 不兼容；同类 auto 成功 |
| 按类型强制 custom/web_search | 400，转换器仅接受 auto/none/required/named function；不能由此推断 custom 本身不支持 |

追溯：context 请求 `71dcad67-763e-4571-b54e-fd4477e98402`；搜索 auto `13a0b000-3cf2-4b4a-a06f-8851b491f547`、required `e116511f-04aa-4d01-a93f-2dbfae70fd2e`；真实 exec 文法三轮 `39996367-96d7-4297-8c17-5a83fc64e9a4`、`8e4d9ede-818f-4c9f-985b-0cd153262565`、`1e44b441-389f-4584-a4eb-169e47a48da2`。只存类型和验证结论，不归档原始推理/任务正文。

结论与边界：OpenCode 当前原生入口确实能转换普通、custom、动态工具及代表性续轮，不必因此恢复本地整套 Chat 桥接。当前需要解决的是已复现的参数差异和搜索执行缺口。没有上游内部日志，搜索是否被转换器忽略、受工作区配置限制或需要另一官方入口仍未确认；本次结果只适用于现有 Go 渠道和 Kimi，不能泛化为所有 OpenCode 模型无法搜索。未验收完整 Codex 端到端任务、实际 Key 映射资金链、并行工具压力、多种 grammar、tool_search 续轮或任意跨供应商历史迁移。下一步不应静默删搜索/改工具选择冒充无缝；应核实官方入口与工作区搜索集成，再决定最小参数兼容方案。


## 2026-11-04 09:09:16 +00:00 — 直接修改 Codex 模型名会同时改变客户端请求形状

用户澄清：指定的成功样本来自修改 Codex config.toml 的 model，目标是用平台 Key 映射替代反复编辑客户端配置。只读核对 Caddy 精确 request_id、New API 消费记录与 GPT-Load 执行记录：`202611040855556359250188268d9d65TYCR8nR` 为 deepseek-v4-pro，后续 `202611040858254383266148268d9d6RE7EIsXL`、`202611040901220321490008268d9d6dD3kfTY7` 为 kimi-k3，均原生 Responses 成功、用量完整，New API stream_status.response_status=completed 且无 token_model_mapping。中间另一条 gpt-6-luna 请求使用结构化 title/description 输出，不能误认成用户切换目标失败。

与原失败 `202611040624117009409948268d9d6rhdvuvY7` 对照：原请求模型 gpt-6.1-terra，reasoning 包含 high/detailed/context=all_turns，text.verbosity=low，动态 additional_tools 下声明 custom exec；直接配置 DeepSeek/Kimi 的样本只有 high/detailed、无 context、无 text，顶层 tools 提供 exec_command 等普通函数与 namespace。旧失败样本没有任何 web_search 声明，其即时失败不是搜索造成。新成功样本声明了 web_search，但 Kimi 后续请求历史只有普通回复与 reasoning、没有工具调用；不能据此宣称搜索已经执行。

补充受控合成对照：Kimi 保留 text.verbosity=low 与 high/detailed，仅不带 context，返回 200/completed。此前真实 exec/custom 动态声明三轮已通过。因此不能把所有客户端差异都当必删字段，也不能凭名称把 custom exec 捏造成客户端未声明的 exec_command。最小待实现方向是对最终目标处理已证实不兼容的参数，保留原客户端工具合同；模型选择/计费/日志仍由 New API 承担。本轮未改业务代码或生产配置。

此前被打断回合的已验证补充：同一现有 Go 凭据直连 `/inference/go/openai/v1/responses` 仍拒绝 reasoning.context；该入口与 `/zen/go/v1/responses` 的 Kimi web_search required 都只返回无搜索能力的普通回答，没有搜索事件。客户端源码 `packages/core/src/plugin/provider/opencode.ts` 使用 `/console/api/websearch`（query/providerID，Bearer，工作区可选 x-org-id）；现有凭据调用该接口返回 JSON 403 Forbidden。不能把换根地址当已证实修复，也不能把 403 确定归因于未启用、Key scope 或某一种鉴权原因。搜索仍是独立未解决能力，未验证任何权限提升/改配置，不重试绕过拒绝。来源为公开 OpenCode 树 `55276f2108c8853f94cf16dc530e7754a6ce8318` 及官方 Codex/搜索文档。


## 2026-11-04 09:27:48 +00:00 — 同为 OpenCode Go 模型不代表参数能力一致

真实补测：deepseek-v4-pro 保留 context=all_turns 和去掉 context 都返回 completed；glm-5.3 保留时 400、去掉后 completed，与此前 Kimi 证据一致。故修复未使用模型家族前缀或非 GPT 黑名单，只按最终具体型号处理 all_turns，未知值仍由上游校验。

本地 `TestOpenCodeMappedResponsesNativeTools/opencode_go/kimi-k3/stream=false` 加入现场拒绝条件后先失败，接入参数归一化后通过；两 OpenCode 渠道、普通/流式、完整工具历史和 DeepSeek/GPT 隔离均通过。执行层、渠道、网关、dialect 回归、OpenCode race、vet 和构建通过。新函数及测试中文注释密度检查通过。

New API 既有资源测试新增 `TestTokenModelMappingResponsesHotUpdate`：同一 Key、同一 Responses 流式请求，先 Kimi 后 GLM，真实持久更新后网络边界目标变化，渠道 C 映射继续生效，B 的两种价格分别扣款且日志保留请求 A/目标 B/计费 B。该项 SQLite 3.50.4 通过，既有模型映射管理/权限/缓存定向回归通过；本轮无数据库或计费业务改动，不声称重新执行了完整三库矩阵。第一次新流式夹具缺少 main 初始化的 StreamingTimeout 导致 ticker panic，已按既有流式测试显式设置并在 cleanup 恢复，未修改生产默认值。

此次真实供应商测试仅验证参数能力，本地新实现未发布到生产，完整真实 Key→GPT-Load→供应商任务仍需上线验收。当前不会自动改客户端工具集合，不支持承诺任意旧加密状态跨厂商迁移；搜索问题单独保留。


## 2026-11-04 09:33:42 +00:00 — 请求中的搜索声明不能证明当轮实际搜索

用户询问最后一次 Kimi 直接配置测试是否已搜索。重新精确核对 `202611040901220321490008268d9d6dD3kfTY7`：请求声明 web_search 且用户要求搜索，New API 记录 completed；Caddy 只保存入站请求，没有该响应流，不能从请求中的历史项缺少搜索就断定本轮没有搜索。之前基于该历史的表述证据不足。

进一步读取与时间/请求匹配的本地 Codex 对话回合 `01a1a5ca-97f9-7a33-ace0-81fcaf29ab0f`：实际可见回复明确表示没有联网搜索工具，回合无搜索工具事件，随后是普通知识回答。因此这个具体样本不是成功搜索的可复用链路。只记录验证结论，不保存用户问题正文、模型推理或无关其他账号任务。

后续排查应优先按 request_id 对齐客户端回合及必要的响应事件；只有 HTTP 200、声明 web_search 或用户要求搜索都不足以验收。不能把独立 Console 搜索接口的 403 当作原推理请求的错误，也不能把它直接判定为某种 Key 权限配置原因；完整搜索适配仍未完成。

## 2026-11-04 10:28:44 +00:00 — 同名 GPT 模型不代表相同的托管搜索能力

New API 的 GPT 搜索编排联调发现 gpt-6-luna 同时存在 Codex 与 OpenCode Go 组。真实请求日志确认：搜索成功落入 Codex，强制 web_search 失败落入 Go，错误提示该模型没有 hosted search。不能靠名称相同或单次成功认定能力一致；搜索后端应使用支持搜索的独占路由/别名。当前仅在 Codex 组的 gpt-6.1-sol 已完成 Kimi、GLM 主模型的真实搜索及下一轮历史验收。未改生产组配置。

Codex 上游另外拒绝 max_tool_calls，New API 不再默认注入；显式客户端约束仍保留并报告兼容错误。平台只限制自动发起 GPT 请求轮数，不承诺供应商内部搜索次数硬上限。正式实现位于 New API，GPT-Load 不新增搜索编排。配置、费用与边界详见[New API 方案](../../../new-api/docs/plans/api-key-model-mapping.md)。


## 2026-11-04 12:34:06 +00:00 — 缺头补齐应复用网关请求身份，避免共享固定值

用户 Gemini Chat 的 Caddy 入口无会话头；原配置重放仍 502，上游明确报缺 x-opencode-session。只在单次测试中补独立随机值便 200/stop，两层日志均成功。New API 的 set_header 参数操作在请求体透传路径被跳过，且线上不识别 {user_session_id}；配置字面量不能当成动态会话。代码证据是 New API 的 relay/compatible_handler.go 透传分支和 relay/channel/api_request.go 占位符解析；核对时线上版本为 7f22628c28df，未改线上配置。

用户选择 GPT-Load 自动补头。本地实现保留已有标识，无标识则使用网关 RequestID：handler 在入口生成 UUID，拒绝客户端伪造，全部重试复用；熵源失败的 untracked 不能当作会话默认值。新实现只改出站头副本，避免污染其他候选。普通、流式、探测与隔离边界测试及真实 Gemini 普通/流式均通过。不要把任意客户端自行传同名会话时的供应商缓存隔离视为本规则的保证；不要把无会话的独立请求宣称为同一对话自动识别。
