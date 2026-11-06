# DeepSeek 工具历史与 User-Agent 故障验证

验证时间：2026-11-05 07:56:27 +00:00。

后续状态：2026-11-05 08:45:46 +00:00 已完成本地定向修复与真实上游验收，见末尾“候选修复验收”。以下原始实验保留用于追溯；其中“尚未实现”描述的是首次诊断时点，不能覆盖最新状态。

## 结论与范围

针对请求 `202611050630575069771958268d9d69CsdqLbg`，已用合成历史复现：OpenCode Go 的 DeepSeek V4 Pro Responses 入口不能正确配对被 developer 消息隔开的并行工具结果。两份结果均存在且 call_id 正确，仍报第二个调用缺少结果。将同一 developer 消息放在全部结果之后即可完成。图片不是必要触发条件，也不需要删除图片才能通过。

绕过 New API 和 GPT-Load、直连 `https://opencode.ai/zen/go/v1/responses`，普通与流式请求均得到相同对照结果。因此该故障可独立发生于上游接口，不依赖本地网关转换。上游内部源码与运行日志不可见，尚不能指定其内部具体函数；“消息边界提前结束工具结果配对”是与观测一致的机制解释。

另外，DeepSeek V4.1 Flash Chat 的 403／1010 已通过单独改变 User-Agent 复现：Python 默认 UA 被拒绝，桌面应用 UA 和真实命名的诊断 UA 均成功。此结论限定当前上游规则，不推断所有 403 都由 UA 导致。

共执行 17 次短合成请求，8 次完成、9 次校验或访问拒绝，使用既有 Go Plus 额度。未重放用户任务、执行模型工具、修改生产配置或发布代码。前 4 次探索夹具缺少 reasoning_text，暴露了独立的思考历史要求；随后补齐该字段，正式矩阵据此排除干扰。原故障请求已有 reasoning 项，不能用夹具错误替换原故障原因。

## 对照结果

Pro 案例均为两个正确配对的工具调用。每个独立案例使用随机测试会话，任务固定要求返回 OK；图片为生成的 16×16 红色 PNG。经 GPT-Load 的正式记录均确认组 2、凭据 2、opencode_go、模型身份未变化、一次执行尝试。

| 入口 | 结果类型 | developer 位置 | stream | 实际结果 |
| --- | --- | --- | --- | --- |
| GPT-Load | 两个文本结果 | 无 | false | 200、completed、OK |
| GPT-Load | 两个文本结果 | 两结果之间 | false | 400、第二个调用缺少结果 |
| GPT-Load | 图片与文本 | 无 | false | 200、completed、OK |
| GPT-Load | 图片与文本 | 两结果之间 | false | 400、第二个调用缺少结果 |
| GPT-Load | 图片与文本 | 全部结果之后 | false | 200、completed、OK |
| GPT-Load | 两个文本结果 | 全部调用之前 | false | 200、completed、OK |
| 直连 OpenCode | 两个文本结果 | 两结果之间 | false | 400、第二个调用缺少结果 |
| 直连 OpenCode | 两个文本结果 | 全部结果之后 | false | 200、completed、OK |
| 直连 OpenCode | 两个文本结果 | 两结果之间 | true | 400、第二个调用缺少结果 |
| 直连 OpenCode | 两个文本结果 | 全部结果之后 | true | 200、OK、response.completed，终态 completed |

关键 GPT-Load 请求 ID：文本失败 `72e950b0-cb9b-4dc4-85bb-8a400e3aabf3`；图片失败 `d6c62abd-4f2d-462f-919e-988995f49fdf`；保留图片且后置消息成功 `9bb7caa7-20bd-4878-9570-6b8a8db5d930`。直连上游的普通失败 ID `be9949d8-8363-40f2-8e47-8e8222885ad7`，流式失败 ID `1c82fadb-d84b-44fc-8c53-dea305773f7d`；直连成功响应没有返回 request-id 头，不能编造关联 ID。

Flash 对照使用 `/v1/chat/completions`、`deepseek-v4.1-flash`、`max_tokens=64`、相同的“Reply only OK.”正文。前两个案例同凭据、同会话，仅 UA 不同。

| User-Agent | 结果 | GPT-Load 请求 ID |
| --- | --- | --- |
| Python-urllib/3.11 | 403、error code: 1010 | 0496457c-ed12-47df-b775-d7739e2a0a6b |
| 故障现场的 Codex Desktop UA | 200、stop、OK | b685647f-70a0-4415-baee-3b7c3d4d99e4 |
| Oak-Compatibility-Diagnostics/1.0 | 200、stop、OK | 6b74df81-7222-4178-8b52-69348c67c636 |

## 最小复现正文

以下为直连测试使用的普通请求。使用既有上游凭据发往上述官方入口，并设置 JSON 内容类型、非空独立 `x-opencode-session` 及现场应用 UA。凭据只在服务器内存使用，不写入本文件。

```json
{
  "model": "deepseek-v4-pro",
  "stream": false,
  "input": [
    {"role": "user", "content": "Synthetic compatibility check. Read the provided results and reply only OK."},
    {"type": "reasoning", "content": [{"type": "reasoning_text", "text": "I will read both synthetic results and acknowledge completion."}], "summary": []},
    {"type": "function_call", "call_id": "call_diag_a", "name": "read_a", "arguments": "{}", "status": "completed"},
    {"type": "function_call", "call_id": "call_diag_b", "name": "read_b", "arguments": "{}", "status": "completed"},
    {"type": "function_call_output", "call_id": "call_diag_a", "output": "A"},
    {"type": "message", "role": "developer", "content": [{"type": "input_text", "text": "Both tool results are supplied. Reply only OK."}]},
    {"type": "function_call_output", "call_id": "call_diag_b", "output": "B"}
  ],
  "tools": [
    {"type": "function", "name": "read_a", "description": "Return synthetic text", "parameters": {"type": "object", "properties": {}, "additionalProperties": false}},
    {"type": "function", "name": "read_b", "description": "Return synthetic text", "parameters": {"type": "object", "properties": {}, "additionalProperties": false}}
  ]
}
```

失败返回 `No tool output found for tool call call_diag_b.`。只交换 input 最后两项，即把 developer 消息放到 B 结果之后，返回 completed、OK。两份正文分别改为 `stream=true` 后，前者仍返回 400，后者收到完整 SSE 终态。这里的 reasoning_text 为人工合成夹具，不是实际模型的私有推理。

## 修复方向与未验证边界

已验证的兼容方向：对确切 OpenCode Responses 工具历史，在所有已配对结果齐全的前提下，维持同一批工具结果连续，再保留中间 developer 消息；保留每个 call_id、结果内容和 developer 文本。不得删除工具结果、图片或指令以掩盖问题。本轮仅验证顺序对照，尚未实现此适配；不能直接推广为全渠道任意消息重排。正式实现还应覆盖缺失结果、重复 call_id、多个批次和取消，以及指令作用范围。

诊断脚本应使用真实标识自己的 UA，例如已验证的 `Oak-Compatibility-Diagnostics/1.0`。本轮没有要求平台全局覆盖用户 UA，也没有改变线上头部策略。

本轮未验证 DeepSeek 实际理解图片内容；图片案例只证明该历史形状可被接受并完成。未验证任意中途用户消息、其他消息角色、Kimi/GLM 或不同 DeepSeek 版本是否存在同样限制。

## 2026-11-05 08:45:46 +00:00 — 候选修复验收

用户授权修复后，已在 `internal/execution/bifrost/opencode_history.go` 实现定向排序，由 `passthrough.go` 的 Responses 创建分支接入。只处理 OpenCode Go 最终目标 deepseek-v4-pro、连续多个普通 function_call 与完整唯一结果。结果稳定保序，中间 developer 消息保持各自内容与相对次序并移到本批结果之后。第一份结果前已有消息时不移动；缺失/重复 ID、用户/助手/system/custom/动态工具及未知事件边界均保持原样。工具与扩展字段使用 RawMessage，不引入浮点精度损失。实际修改记录固定事件及请求 ID。

`user_agent.go` 复用网关默认身份，只替换 Go 生成/探测中来自客户端的 Python-urllib 默认 UA；其他 UA、模型列表、Zen/其他渠道及管理员显式覆盖保持原优先级。原始请求头使用副本，不影响其他候选。无需新配置、依赖或数据库迁移。

修复前，`TestOpenCodeToolHistoryOnWire` 的普通/流式案例均返回原始缺结果 400；`TestOpenCodePythonUserAgent` 的普通/流式案例返回 403/1010。修复后，两项以及边界测试全部通过。边界覆盖不完整/重复结果、指令角色、未知类型、不同渠道/型号/协议、多个批次、共享请求隔离、完整字段与幂等。

完成验证：

```sh
go test ./internal/execution/bifrost ./internal/gateway ./internal/channel ./internal/execution -count=1
go test -race ./internal/execution/bifrost -run 'TestOpenCode|TestRequestUserAgentOnWire' -count=1
go vet ./internal/execution/bifrost ./internal/channel/...
go build ./...
```

真实验收通过本地候选 runtime → 独立回环 SSH 隧道 → 线上旧 GPT-Load → OpenCode，使用现有数据面凭据，凭据仅在进程内存和子进程环境传递。本地入口仍使用原始失败顺序与 `Python-urllib/3.11`，由真实候选代码完成排序和 UA 处理。没有使用测试侧提前排序或手工改 UA 冒充修复。

| 案例 | UTC 完成时间 | GPT-Load 请求 ID | 结果 |
| --- | --- | --- | --- |
| Pro 文本／普通 | 08:43:04 | 7d794cc7-f783-435f-93fc-499075ff0da3 | 200／completed |
| Pro 文本／流式 | 08:43:06 | 0c724189-e402-4cd8-8a03-0f0f2cf38649 | 200／response.completed |
| Pro 图片／普通 | 08:43:08 | b3a8df7b-a299-4439-af7d-1276ff3d1925 | 200／completed |
| Pro 图片／流式 | 08:43:10 | 50663bbe-6fa4-467f-8625-44c38b254d4b | 200／response.completed |
| Flash Chat／普通 | 08:43:12 | 40fd83b9-3d4d-486a-afb2-0ebeae92cb16 | 200／stop |
| Flash Chat／流式 | 08:43:14 | 88f655c4-31a4-4ce0-a957-7e41305ecd09 | 200／DONE |

6 条执行日志均为组 2、凭据 2、opencode_go、success、单次尝试，实际协议与型号一致。`TestOpenCodeCompatibilityLive` 默认跳过，只有显式配置 `OAK_HISTORY_LIVE_URL`、`OAK_HISTORY_LIVE_KEY`、独立 `OAK_HISTORY_LIVE_REQUEST_ID` 才执行。再次运行会消耗供应商额度。测试输出只报告终态，不执行任何模型工具，也不保存用户数据或推理。

本地修复已验证，临时隧道已清理；未提交、推送或部署，线上二进制没有被本轮更新。既有用户任务、New API 资金和映射链未重放，不能把此次候选联调描述为完整客户端重新验收。
