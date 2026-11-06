# OpenCode 原生 Responses 兼容验收

## 2026-11-05 18:46:51 +00:00 — 挂靠额度顺延修复待配套发布

本轮只读确认 wdm 运行 `oak-255abf88459d-42005704315-1`，New API 运行 `oak-1ea2b201e07c-42083430069-1`。下面旧任务的“未提交/未发布”不能覆盖该实测版本。

已在 `internal/gateway/handler.go` 接入全请求安全拒绝证据及保留头过滤，在既有 `model_cooldown_test.go` 验证冷却零发送、HTTP/SSE、伪造头与前序未知结果；New API 配套识别、错误日志及资金回归已完成。完整证据、命令和发布后验收见 [New API 排障记录](../../new-api/docs/diagnostics/hosting-resource-fallback.md)。相关两包回归、定向 race、vet 及根程序构建通过；无配置、依赖或数据库结构变更。

尚未提交、推送或部署，待用户明确发布授权。获批后核对 diff/暂存区，只提交本任务路径，保护此前未跟踪 `docs/diagnostics/`。先发布 GPT-Load，再发布 New API，验证真实耗尽账号优先时顺延、双服务请求关联及一次结算；不重放用户任务或人为耗尽余额。

## 最新补充：2026-11-05 08:45:46 +00:00 — DeepSeek 历史与 Python UA 修复已在本地验收

用户授权修复已复现的两类故障。`opencode_history.go` 经 `passthrough.go` 接入原生 Responses，精确限定 OpenCode Go / deepseek-v4-pro；只对完整唯一配对的并行结果后置中间 developer 消息，保留正文与边界。`user_agent.go` 对 Go 生成/探测的客户端 Python 默认 UA 使用网关身份，管理员显式配置优先。没有新增配置、数据库或依赖。

两类网络回归修复前分别复现 400、403，修复后通过。四包完整回归、定向 race、vet 和 `go build ./...` 通过；本地候选执行器→SSH 隧道→线上旧 GPT-Load→OpenCode 的 6 次合成联调全部完成（Pro 文本、Pro 图片、Flash Chat，各普通/流式），未重放用户任务。隧道已清理。具体矩阵与边界见[诊断记录](diagnostics/deepseek-tool-history.md)。

当前仅本地实现，未提交、推送或部署；线上仍需发布才具备修复。用户要求发布时，先审查本次文件与已有文档修改，按既有流程精确提交；oak 推送会自动部署，不直接覆盖生产程序。发布后再用同类合成请求验收，不能把本地候选联调等同于线上已更新。以下更早的跨模型/搜索工作继续独立保留。

## 最新补充：2026-11-04 12:52:26 +00:00 — 自动补会话头已发布并完成线上重放

用户最终要求仅在 GPT-Load 的 OpenCode 渠道自动补 x-opencode-session。实现位于 `internal/execution/bifrost/opencode_session.go`，由普通及流式 Execute 共用，探测同样生效。已有非空值保留；原生会话别名优先于随机网关 RequestID；无身份时拒绝 untracked。无新配置、数据库或依赖。New API 的占位符草稿不再继续，当前代码已无该草稿。

四包（bifrost、gateway、channel、execution）回归、OpenCode race、vet、go build ./... 通过。本地 Gemini 普通/流式合成联调已通过。最新只读核对线上已为 a2d8705dd793，包含自动补头。经用户授权，把原 Caddy 留存的 234 字节 Gemini Chat 正文不加任何会话头，经网站入口再次提交，返回 200/stop；Caddy 确认正文逐字一致、入口无会话头，GPT-Load 日志 success。自动补头故障已验收；本轮没有发布服务或改配置。

New API 渠道 4 之前配置的字面量 {user_session_id} 不属于动态标识；未来若启用该参数操作，会被 GPT-Load 当成已有值保留，届时应移除。未经要求不改线上配置、不推送 oak。以下搜索与完整跨模型任务的旧交接不能覆盖本段服务版本；本次 Chat 验收不代表全部工具/搜索流程已重新验证。

更新时间：2026-11-04 10:28:44 +00:00。用户要求 Codex 请求 gpt-6.1-terra，仅配置 Key 映射 kimi-k3 即可无感完成新任务及同目标工具续轮；本轮要求继续验证。完整无感切换仍未交付。

## 最新搜索进展

用户已批准用 GPT 执行真实搜索并回交目标模型。New API 已本地实现搜索配置、内部工具循环、分模型账单和关联日志；Kimi/GLM 的真实搜索与公开历史下一请求均通过，未部署。搜索后端当前验证为仅路由到 Codex 的 gpt-6.1-sol；gpt-6-luna 同时存在 Go 组，会发生能力不一致分流。第一版阶段缓冲 SSE；HTTP 显式历史范围及发布检查见[New API 交接](../../new-api/docs/handoff.md)。临时回环代理和 SSH 隧道已清理。下面之前的搜索阻碍属于排障历史，不能当作当前尚未实现；普通参数改动仍需与 New API 一并发布后验收真实客户端。

## 当前状态

- 线上仍为 `oak-739fc7992019-41784588956-1`。本地已实现精确 Kimi K3 / GLM 5.3 的 context=all_turns 参数兼容，尚未提交、推送或部署。
- 先前默认 Python UA 的 403/1010 已排除，真实描述性 `Oak-Compatibility-Check/1.0` 可测试；不要继续要求用户手工抓包才能推进。
- 本轮 19 次真实合成请求：14 次 HTTP 200、5 次明确参数 400。普通函数/custom/动态声明、流式和代表性续轮成功；真实 exec 文法连续两次调用后第三轮完成，call_id 不重复，工具完成与流式终态各一次。
- `reasoning.context=all_turns` 稳定复现原 400；不带 context 的 high/detailed 可用。强制指定函数与 Kimi thinking 冲突，auto 可用。按类型强制 custom/web_search 也被上游拒绝。
- web_search 的 auto/required 均 200 但没有搜索事件或引用，模型明确无搜索工具；此为当前路径验收失败，不能泛化为 OpenCode 所有入口/模型都不支持搜索。
- client tool_search 首次调用成功，完整发现结果续轮未测。使用现有凭据直达回环 GPT-Load，未经过 New API 用户 Key，未重新验证实际计费链；只发送合成任务，未执行生成代码，消耗少量套餐额度。

## 新对照与下一步

用户提供 08:55:55 UTC 的 DeepSeek 和随后 Kimi 直接配置成功样本，已核对 Caddy/New API/GPT-Load。直接配置目标时 Codex 不发 reasoning.context，改用顶层普通函数；原 GPT→Kimi 失败请求带 context、动态 custom exec，且根本没有 web_search 声明。text.verbosity=low 单独保留可成功，custom 原生续轮此前也成功；不要扩大删除范围或造出客户端未声明的工具。详细 ID 和证据见 lessons 最新条目。

1. 已完成本地最小参数修复与测试：`opencode_parameters.go` 精确限定 kimi-k3、glm-5.3；DeepSeek V4 Pro 实测接受 context，保持透传。日志事件 `opencode.reasoning_context.normalized` 可追溯请求。完整网关先红后绿、四包回归、OpenCode race、vet/build 通过；New API `TestTokenModelMappingResponsesHotUpdate` 及既有映射管理/权限/缓存定向回归通过。后续需用户要求发布后再验证真实 Key 链路，不能把本地已完成当作线上已修复。
2. 已对齐用户最后的直接配置搜索测试 `202611040901220321490008268d9d6dD3kfTY7` 与本地 Codex 实际回复：模型明确没有联网工具，回合无搜索事件；不是已成功搜索样本。不能再仅凭 Caddy 请求历史推断当轮输出。搜索独立验收。新旧官方推理入口都没有执行本次 Kimi 搜索；现有 Go 凭据调用官方 `/console/api/websearch` 为 403 Forbidden，原因未定。已询问现有客户端搜索服务，但用户最新优先要求比较直接配置与平台映射，不能继续把普通调用失败归因于搜索。
3. 补 client tool_search 结果续轮、混合/并行工具以及实际 New API Key→GPT-Load→Kimi 流式任务验收。不能只以 200 或本地模拟通过认定无感切换完成。

## 关键文件与保护

详细能力矩阵及执行日志 ID：`docs/memory/lessons.md` 的 2026-11-04 06:55:27 +00:00 条目。既有方案：`../new-api/docs/plans/api-key-model-mapping.md`。原生入口：`internal/channel/modules/opencode_{go,zen}.go`；模拟回归：`internal/execution/bifrost/opencode_presets_test.go`；现有参数覆盖：`internal/gateway/parameter_override_array_test.go`。

GPT-Load 当前业务修改为 passthrough.go 接入、opencode_parameters.go 及边界测试、opencode_presets_test.go 的现场回归；README/记忆已同步。New API 新增测试位于 controller/resource_relay_test.go，未改业务；New API 的 Caddy、文档和测试变更属于此前任务，必须保留。oak push 自动部署，未经要求不提交或推送。当前测试脚本临时位于本机 /tmp/oak-compat-{verify,auto,stream,codex}.py，只是本轮诊断辅助，不作为长期运行依赖；凭据仅远程读取到内存。
