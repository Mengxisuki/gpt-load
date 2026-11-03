package bifrost

import (
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
	for _, item := range gjson.GetBytes(spec.Body, "input").Array() {
		if item.Get("type").String() == "compaction" || item.Get("encrypted_content").Str != "" {
			failure := notSentConversionFailure(execution.ErrorCodeCriticalSemanticLoss, "start a new task; this target cannot reuse encrypted Responses history")
			return preparedAttempt{}, &failure
		}
		// 普通消息与函数回合可移植；内置工具执行记录不能伪装成文本历史。
		// 缺省 type 是 Responses 的简写消息形式，类型校验仍由原解码器负责。
		switch item.Get("type").String() {
		case "", "message", "reasoning", "function_call", "function_call_output":
		default:
			failure := notSentConversionFailure(execution.ErrorCodeCriticalSemanticLoss, "this Chat target supports message and function-call history only")
			return preparedAttempt{}, &failure
		}
	}
	// SDK 的通用 Compatible 策略会忽略不支持的工具；新路由不能据此静默失去能力。
	// 命名空间由现有 SDK 展平并恢复名称，原生托管工具或自定义语法则明确拒绝。
	// 只检查工具类型，不复制函数参数 schema 的验证或转换规则。
	tools := gjson.GetBytes(spec.Body, "tools").Array()
	for len(tools) > 0 {
		tool := tools[0]
		tools = tools[1:]
		switch tool.Get("type").String() {
		case "function":
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
	prepared.responsesRequest = request
	// 共用已有的全局指令、工具白名单、结果展开及推理参数处理。
	// 不新建另一个响应转换器，流式终态、用量及工具别名恢复仍由原实现承担。
	return finishConvertedPreparation(spec, channel.ProviderOpenAICompatible, prepared)
}
