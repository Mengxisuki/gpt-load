package bifrost

import (
	"fmt"
	"net/http"

	"github.com/tidwall/gjson"

	"gpt-load/internal/channel"
	"gpt-load/internal/dialect"
	"gpt-load/internal/execution"
	"gpt-load/internal/protocol"
)

// prepareOpenCodeResponses 将已声明的 Chat 模型接入现有 Compatible 转换器。
// 接收完整消息的无状态任务，保留函数调用；不尝试跨供应商迁移旧会话。
// 三个模型家族共享协议转换，不维护 GPT 到每个具体型号的两两转换表。
// GPT 专属推理选项沿用 Compatible 行为，不伪造目标的 effort 或预算。
// 请求的最终模型不变，客户端仍收到 Responses 结构和原有用量证据。
// 执行结果的 UpstreamProtocol 记录为 Chat，既有日志可区分原生与转换。
// 传入 prepared 只承载已校验的供应商和凭据，不在这里重新选择账号。
// 返回失败意味着尚未发送请求；上游调用及退款语义仍交给既有执行链。
// 不读取模型价格目录，也不改写输入/输出 Token 数量。
func (r *Runtime) prepareOpenCodeResponses(spec execution.AttemptSpec, prepared preparedAttempt, query string) (preparedAttempt, *execution.AttemptResult) {
	// 能力来自已经校验的目标路由，而不是客户端可伪造的请求参数。
	// 不为其他网关或非 Responses 操作隐式添加转换能力。
	if (spec.ChannelID != string(channel.OpenCodeGo) && spec.ChannelID != string(channel.OpenCodeZen)) || spec.ClientProtocol != protocol.OpenAIResponses {
		failure := notSentConversionFailure(execution.ErrorCodeTargetConversionNotSupported, "gateway conversion is not supported")
		return preparedAttempt{}, &failure
	}
	// 复用网关路径解析：base_url 可能已有 /v1，也可能是 /zen/go 或租户路径。
	// 不手动拼接斜杠，探测和执行必须命中相同的 Chat 接口。
	typedURL, err := resolveMultiProtocolGatewayTargetURL(r.fixedConfig.targetBaseURL, "/v1/chat/completions", query)
	if err != nil {
		failure := notSentUnaryFailure(execution.ErrorKindInvalidRequest, "invalid gateway target")
		return preparedAttempt{}, &failure
	}
	// 用独立结构设置本次执行信息，不改动冻结的 AttemptSpec。
	// 客户端协议保持不变，响应端会选择已有的反向转换器。
	prepared.mode, prepared.upstreamProtocol = channel.RouteConverted, protocol.OpenAICompletions
	prepared.typedURL, prepared.clientProtocol = typedURL, spec.ClientProtocol

	// 探测也走 Chat，避免“探测成功”与模型真实可用接口不一致。
	if spec.Operation == execution.OperationProbe {
		prepared.responsesRequest = newProbeRequest(prepared.provider, channel.ProviderOpenAICompatible, spec.UpstreamModel).ToResponsesRequest()
		return prepared, nil
	}
	// 复用入口的状态识别，防止直接调用执行器时遗漏 previous_response_id 等约束。
	// 完整消息历史仍可用于同一目标的后续工具回合，不以消息数量判断是否新任务。
	// 无效正文返回固定错误，避免把用户输入或加密历史拼进日志。
	metadata, err := dialect.NewOpenAIResponses().InspectRequest(&dialect.ParsedRequest{Method: http.MethodPost, Path: "/v1/responses", Body: spec.Body})
	if err != nil {
		failure := notSentUnaryFailure(execution.ErrorKindInvalidRequest, "invalid Responses request")
		return preparedAttempt{}, &failure
	}
	if metadata.RouteRequirement == execution.RouteRequirementNative {
		failure := notSentConversionFailure(execution.ErrorCodeCriticalSemanticLoss, "start a new task with complete messages; this target cannot reuse Responses server state")
		return preparedAttempt{}, &failure
	}
	// 加密推理与压缩状态不能交给其他厂商；明确拒绝，不能让 SDK 静默丢弃。
	// 明文历史和普通工具调用继续交给现有转换器，不改写用户内容。
	tools := gjson.GetBytes(spec.Body, "tools").Array()
	for index, item := range gjson.GetBytes(spec.Body, "input").Array() {
		if item.Get("type").String() == "compaction" || item.Get("encrypted_content").Str != "" {
			failure := notSentConversionFailure(execution.ErrorCodeCriticalSemanticLoss, "start a new task; this target cannot reuse encrypted Responses history")
			return preparedAttempt{}, &failure
		}
		// 普通消息与函数回合可移植；内置工具执行记录不能伪装成文本历史。
		// 缺省 type 是 Responses 的简写消息形式，类型校验仍由原解码器负责。
		switch item.Get("type").String() {
		case "", "message", "reasoning", "function_call", "function_call_output", "custom_tool_call", "custom_tool_call_output":
			// custom 的声明、非空输入和调用关联在 typed DTO 建立后统一验证。
			// 此处允许解析并不表示接受任意自定义工具。
		case "additional_tools":
			// 动态声明不是厂商执行历史，现有 Compatible 会把定义提升到 tools。
			// 同样校验其中的工具，避免嵌套自定义或托管工具被 SDK 静默丢弃。
			// 它不等同于 tool_search_output；搜索调用与结果仍需单独的协议适配。
			// 检查原始数组形状，再解码 DTO，避免缺失或非数组工具被当成空声明。
			declared := item.Get("tools")
			if !declared.IsArray() {
				failure := notSentUnaryFailure(execution.ErrorKindInvalidRequest, "additional_tools.tools must be an array")
				return preparedAttempt{}, &failure
			}
			tools = append(tools, declared.Array()...)
		default:
			// 诊断只报告位置与已知协议类型，不回显任意客户端值或历史正文。
			// 未知类型统一命名，避免 type 字段被用来把敏感内容写入日志。
			kind := "unknown"
			switch item.Get("type").String() {
			case "item_reference", "tool_search_call", "tool_search_output",
				"web_search_call", "web_fetch_call", "file_search_call", "computer_call", "computer_call_output",
				"code_interpreter_call", "local_shell_call", "local_shell_call_output", "image_generation_call",
				"mcp_call", "mcp_list_tools", "mcp_approval_request", "mcp_approval_response", "advisor_call":
				kind = item.Get("type").String()
			}
			failure := notSentConversionFailure(execution.ErrorCodeCriticalSemanticLoss, fmt.Sprintf("Responses input[%d] type %s cannot be converted to Chat; use a compatible native model", index, kind))
			return preparedAttempt{}, &failure
		}
	}
	// SDK 的通用 Compatible 策略会忽略不支持的工具；新路由不能据此静默失去能力。
	// 命名空间由现有 SDK 展平并恢复名称，原生托管工具和未适配的自定义语法明确拒绝。
	// 只检查工具类型，不复制函数参数 schema 的验证或转换规则。
	// 顶层和动态声明共享同一检查队列，避免两处能力规则逐渐不一致。
	for len(tools) > 0 {
		tool := tools[0]
		tools = tools[1:]
		switch tool.Get("type").String() {
		case "function", "custom":
			// custom 仅进入下方 exec 适配校验，未知 grammar 仍在派发前拒绝。
		case "namespace":
			tools = append(tools, tool.Get("tools").Array()...)
		default:
			failure := notSentConversionFailure(execution.ErrorCodeCriticalSemanticLoss, "this Chat target supports function tools only; hosted or custom tools require a compatible native model")
			return preparedAttempt{}, &failure
		}
	}
	// 工具检查通过后才构造 SDK DTO，避免先丢字段再验证导致假通过。
	request, err := buildConvertedResponsesRequest(spec, prepared.provider)
	if err != nil {
		failure := notSentUnaryFailure(execution.ErrorKindInvalidRequest, err.Error())
		return preparedAttempt{}, &failure
	}
	// 在工具选择校验之前提升动态定义，否则 allowed_tools 找不到声明的函数。
	// 复用原 helper 的请求副本与定义合并语义，不另建转换或丢弃历史。
	request, toolErr := hoistCompatibleAdditionalTools(request)
	if toolErr != nil {
		failure := notSentUnaryFailure(execution.ErrorKindInvalidRequest, "invalid tools in additional_tools input item")
		return preparedAttempt{}, &failure
	}
	// 先把支持的 exec 变成标准函数，再复用现有工具选择和历史转换。
	// bridge 跟随本次 attempt，用于响应端恢复 custom 类型与原始输入。
	request, prepared.execBridge, err = prepareOpenCodeExecBridge(request)
	if err != nil {
		failure := notSentConversionFailure(execution.ErrorCodeCriticalSemanticLoss, err.Error())
		return preparedAttempt{}, &failure
	}
	prepared.responsesRequest = request
	// 共用已有的全局指令、工具白名单、结果展开及推理参数处理。
	// 不新建另一个响应转换器，流式终态、用量及工具别名恢复仍由原实现承担。
	return finishConvertedPreparation(spec, channel.ProviderOpenAICompatible, prepared)
}
