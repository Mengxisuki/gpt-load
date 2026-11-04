package bifrost

import (
	"strings"

	providerUtils "github.com/maximhq/bifrost/core/providers/utils"
	"github.com/maximhq/bifrost/core/schemas"

	"gpt-load/internal/channel"
	"gpt-load/internal/execution"
)

func (r *Runtime) usesCompatibleChat(spec execution.AttemptSpec) bool {
	// 仅显式 Compatible 渠道使用 Chat 转换，聚合网关自行处理协议适配。
	// 路由模式由注册表冻结，不能因模型名称或客户端工具触发隐式降级。
	// 普通探测保持既有路径，避免对非 Responses 探测错误地重建响应。
	return spec.RouteMode == execution.RouteConverted &&
		r.providerKind(spec) == channel.ProviderOpenAICompatible &&
		(spec.Operation == execution.OperationChatCompletion || spec.Operation == execution.OperationResponsesCreate)
}

func (r *Runtime) compatibleResponsesRequest(ctx *schemas.BifrostContext, request *schemas.BifrostResponsesRequest) (*schemas.BifrostResponsesResponse, *schemas.BifrostError) {
	chat, aliases, err := prepareCompatibleChatRequest(ctx, request)
	if err != nil {
		return nil, err
	}
	response, err := r.core.ChatCompletionRequest(ctx, chat)
	if err != nil || response == nil {
		return nil, err
	}
	converted := response.ToBifrostResponsesResponse()
	converted.BackfillParams(request)
	providerUtils.RestoreResponsesNamespaceToolCalls(aliases, &schemas.BifrostResponse{ResponsesResponse: converted})
	return converted, nil
}

// 对齐锁定 SDK core/v1.13.1 的 prepareResponsesRequest 中适用于 Compatible 的准备。
// 该渠道没有 SDK 自动缓存注入或 Anthropic 私有计费头元数据；转换入口恢复后可删除此衔接。
func prepareCompatibleChatRequest(ctx *schemas.BifrostContext, request *schemas.BifrostResponsesRequest) (*schemas.BifrostChatRequest, map[string]schemas.NamespaceToolAlias, *schemas.BifrostError) {
	ctx.SetValue(schemas.BifrostContextKeyBaseProviderType, schemas.OpenAI)
	// 此处实际发送 Chat，不能用模型的 Responses 能力判断 namespace 是否可直传。
	// Chat 不支持容器或动态声明；不先提升、展平会被 ToChatRequest 静默丢弃。
	// 名称、描述、历史和响应别名继续由 SDK 统一处理，避免另建映射规则。
	// 该入口只服务转换路线，不能据此修改原生 Responses 的工具格式。
	request, err := hoistCompatibleAdditionalTools(request)
	if err != nil {
		return nil, nil, err
	}
	// 默认 functions 解包为裸名字，其他 namespace 使用 SDK 的限定名字。
	request, err = providerUtils.UnwrapDefaultNamespaceTools(request)
	if err != nil {
		return nil, nil, err
	}
	request, err = providerUtils.FlattenResponsesNamespaceTools(ctx, request)
	if err != nil {
		return nil, nil, err
	}
	// 工具 schema 仍由 SDK 的 Chat 调度入口规范化；这里只修复协议形状。
	chat := request.ToChatRequest()
	chat.Input = mergeCompatibleToolTurns(chat.Input)
	preserveCompatibleToolTurnReasoning(chat.Input)
	return chat, request.NamespaceToolAliases, nil
}

// SDK 的同名准备步骤未导出；转为 Chat 前提升 additional_tools，保持请求隔离。
func hoistCompatibleAdditionalTools(request *schemas.BifrostResponsesRequest) (*schemas.BifrostResponsesRequest, *schemas.BifrostError) {
	var tools []schemas.ResponsesTool
	found := false
	for _, item := range request.Input {
		if item.Type == nil || *item.Type != schemas.ResponsesMessageTypeAdditionalTools {
			continue
		}
		found = true
		var declared []schemas.ResponsesTool
		if err := schemas.Unmarshal(item.AdditionalTools, &declared); err != nil {
			return nil, providerUtils.NewBifrostBadRequestError("invalid tools in additional_tools input item")
		}
		tools = append(tools, declared...)
	}
	if !found {
		return request, nil
	}
	copyRequest := *request
	params := schemas.ResponsesParameters{}
	if request.Params != nil {
		params = *request.Params
	}
	params.Tools = append(append([]schemas.ResponsesTool(nil), params.Tools...), tools...)
	copyRequest.Params = &params
	copyRequest.Input = make([]schemas.ResponsesMessage, 0, len(request.Input))
	for _, item := range request.Input {
		if item.Type == nil || *item.Type != schemas.ResponsesMessageTypeAdditionalTools {
			copyRequest.Input = append(copyRequest.Input, item)
		}
	}
	return &copyRequest, nil
}

// #827：SDK 会把同一助手回合的正文和工具调用拆开。先合并，再保留推理，避免复制。
// 仅合并含工具调用的连续助手片段；其他角色和无法合并的助手字段保留原分界。
func mergeCompatibleToolTurns(messages []schemas.ChatMessage) []schemas.ChatMessage {
	merged := make([]schemas.ChatMessage, 0, len(messages))
	for start := 0; start < len(messages); {
		if messages[start].Role != schemas.ChatMessageRoleAssistant {
			merged = append(merged, messages[start])
			start++
			continue
		}
		end := start
		hasTools, mergeable := false, true
		for end < len(messages) && messages[end].Role == schemas.ChatMessageRoleAssistant {
			message := messages[end]
			mergeable = mergeable && message.Name == nil
			if assistant := message.ChatAssistantMessage; assistant != nil {
				hasTools = hasTools || len(assistant.ToolCalls) > 0
				mergeable = mergeable && assistant.Refusal == nil && assistant.Audio == nil && len(assistant.Annotations) == 0
			}
			end++
		}
		if !hasTools || !mergeable || end-start == 1 {
			merged = append(merged, messages[start:end]...)
			start = end
			continue
		}
		message := schemas.ChatMessage{
			Role:                 schemas.ChatMessageRoleAssistant,
			ChatAssistantMessage: &schemas.ChatAssistantMessage{},
		}
		var contents []*schemas.ChatMessageContent
		var reasoning []string
		for _, part := range messages[start:end] {
			if part.Content != nil {
				contents = append(contents, part.Content)
			}
			if assistant := part.ChatAssistantMessage; assistant != nil {
				message.ToolCalls = append(message.ToolCalls, assistant.ToolCalls...)
				message.ReasoningDetails = append(message.ReasoningDetails, assistant.ReasoningDetails...)
				if assistant.Reasoning != nil && *assistant.Reasoning != "" {
					reasoning = append(reasoning, *assistant.Reasoning)
				}
			}
		}
		if len(contents) == 1 {
			message.Content = contents[0]
		} else if len(contents) > 1 {
			blocks := make([]schemas.ChatContentBlock, 0, len(contents))
			for _, content := range contents {
				if content.ContentStr != nil {
					blocks = append(blocks, schemas.ChatContentBlock{Type: schemas.ChatContentBlockTypeText, Text: content.ContentStr})
				} else {
					blocks = append(blocks, content.ContentBlocks...)
				}
			}
			message.Content = &schemas.ChatMessageContent{ContentBlocks: blocks}
		}
		if len(reasoning) > 0 {
			message.Reasoning = schemas.Ptr(strings.Join(reasoning, "\n"))
		}
		merged = append(merged, message)
		start = end
	}
	return merged
}

// #666：仅修正每次 attempt 新生成的 Chat 请求，不修改共享历史/响应转换。
// 完整文本跨整个助手回合优先；有加密状态时不将摘要当成完整推理。
// SDK 完整修复并通过同套回归后，连同专用 Chat 入口一起移除。
func preserveCompatibleToolTurnReasoning(messages []schemas.ChatMessage) {
	for start := 0; start < len(messages); {
		if messages[start].Role != schemas.ChatMessageRoleAssistant {
			start++
			continue
		}
		end := start
		var reasoning, summaries []string
		hasEncrypted := false
		for end < len(messages) && messages[end].Role == schemas.ChatMessageRoleAssistant {
			assistant := messages[end].ChatAssistantMessage
			end++
			if assistant == nil {
				continue
			}
			if assistant.Reasoning != nil && *assistant.Reasoning != "" {
				reasoning = append(reasoning, *assistant.Reasoning)
			}
			for _, detail := range assistant.ReasoningDetails {
				switch detail.Type {
				case schemas.BifrostReasoningDetailsTypeSummary:
					if detail.Summary != nil && *detail.Summary != "" {
						summaries = append(summaries, *detail.Summary)
					}
				case schemas.BifrostReasoningDetailsTypeEncrypted:
					hasEncrypted = hasEncrypted || detail.Data != nil && *detail.Data != ""
				}
			}
		}
		if len(reasoning) == 0 && !hasEncrypted {
			reasoning = summaries
		}
		if len(reasoning) > 0 {
			text := strings.Join(reasoning, "\n")
			for index := start; index < end; index++ {
				if assistant := messages[index].ChatAssistantMessage; assistant != nil && len(assistant.ToolCalls) > 0 {
					assistant.Reasoning = schemas.Ptr(text)
				}
			}
		}
		start = end
	}
}
