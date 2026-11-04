package bifrost

import (
	"bytes"
	"encoding/json"
	"net/http"
	"sort"
	"time"

	providerUtils "github.com/maximhq/bifrost/core/providers/utils"
	"github.com/maximhq/bifrost/core/schemas"
)

// #665：从完整 Chat chunk 转换，保留原 Responses 执行器的生命周期和客户端编码。
// SDK 修复后应连同专用入口删除；终态/用量/晚到错误的回归继续保留。
func (r *Runtime) compatibleResponsesStream(ctx *schemas.BifrostContext, request *schemas.BifrostResponsesRequest) (chan *schemas.BifrostStreamChunk, *schemas.BifrostError) {
	chat, aliases, err := prepareCompatibleChatRequest(ctx, request)
	if err != nil {
		return nil, err
	}
	// Chat SDK 会合成零用量尾块；原始事件用于区分“未返回用量”和“明确返回零”。
	ctx.SetValue(schemas.BifrostContextKeyAllowPerRequestRawOverride, true)
	ctx.SetValue(schemas.BifrostContextKeySendBackRawResponse, true)
	upstream, err := r.core.ChatCompletionStreamRequest(ctx, chat)
	if err != nil {
		err.ExtraFields.RawResponse = nil
		return nil, err
	}
	if upstream == nil {
		return nil, nil
	}
	output := make(chan *schemas.BifrostStreamChunk, 1)
	go func() {
		defer close(output)
		state := schemas.AcquireChatToResponsesStreamState()
		defer schemas.ReleaseChatToResponsesStreamState(state)
		var pending *schemas.BifrostResponsesStreamResponse
		var usage *schemas.BifrostLLMUsage
		var finalExtra schemas.BifrostResponseExtraFields
		usageSeen := false
		// 格式失败后不再向客户端发工具内容，但继续读取尾部用量再报告失败。
		// SDK 可能把同一上游块的 finish 与 usage 拆开，立即退出会丢失已发生的消费。
		var conversionFailure *schemas.BifrostError
		send := func(chunk *schemas.BifrostStreamChunk) bool {
			select {
			case output <- chunk:
				return true
			case <-ctx.Done():
				return false
			}
		}
		emit := func(response *schemas.BifrostResponsesStreamResponse) bool {
			// 已有格式错误后只消费上游，不再发布后续内容或终态。
			// 返回 true 表示继续读取，不表示本次请求已成功。
			if conversionFailure != nil {
				return true
			}
			providerUtils.RestoreResponsesNamespaceToolCalls(aliases, &schemas.BifrostResponse{ResponsesStreamResponse: response})
			return send(&schemas.BifrostStreamChunk{BifrostResponsesStreamResponse: response})
		}
		for {
			select {
			case <-ctx.Done():
				return
			case chunk, open := <-upstream:
				if !open {
					if !sdkStreamEndedNormally(ctx) || ctx.Err() != nil {
						return
					}
					if conversionFailure == nil {
						// 没有格式错误时验证 finish，不能把缺失终态的上游流报告为成功。
						if pending == nil {
							send(&schemas.BifrostStreamChunk{BifrostError: compatibleStreamError("Chat stream ended without a finish reason")})
							return
						}
						pending.Response.Usage = nil
						// 只附带真正观察到的用量，SDK 合成的零值不算消费证据。
						if usageSeen && usage != nil {
							pending.Response.Usage = usage.ToResponsesResponseUsage()
						}
						pending.ExtraFields = finalExtra
						pending.ExtraFields.RequestType = schemas.ResponsesStreamRequest
						// 发布完整快照，忽略增量的客户端也能读取全部函数调用。
						emit(pending)
					}
					if conversionFailure != nil {
						// 错误和用量一起送回既有执行链，计费无需另建特殊通道。
						// 未观察到用量时保持未知，不能臆造 Token 数或改成未派发。
						if usageSeen {
							conversionFailure.ExtraFields.BilledUsage = usage
						}
						send(&schemas.BifrostStreamChunk{BifrostError: conversionFailure})
					}
					return
				}
				if chunk == nil {
					send(&schemas.BifrostStreamChunk{BifrostError: compatibleStreamError("invalid Chat stream chunk")})
					return
				}
				if chunk.BifrostError != nil {
					chunk.BifrostError.ExtraFields.RawResponse = nil
					send(chunk)
					return
				}
				response := chunk.BifrostChatResponse
				if response == nil {
					send(&schemas.BifrostStreamChunk{BifrostError: compatibleStreamError("missing Chat stream response")})
					return
				}
				usageSeen = takeCompatibleUsagePresence(&response.ExtraFields.RawResponse) || usageSeen
				if response.Usage != nil {
					usage = response.Usage
					// 原始捕获可能被 SDK 的上限截断；正数累计值仍是明确的上游用量证据。
					usageSeen = usageSeen || usage.TotalTokens > 0 || usage.PromptTokens > 0 || usage.CompletionTokens > 0
				}
				finalExtra = response.ExtraFields
				// 首个终态之后只收集尾部用量，不能再让 SDK 重复关闭同一工具。
				// 部分尾块带 role/finish_reason，SDK 会转发它并再次生成工具 done。
				// 普通函数及文本共用此边界，避免重复发布终态。
				if pending != nil {
					// 真正的新输出不是尾部元数据，明确失败，不能静默截断成成功。
					// 保留首个错误并继续读取用量；已发布的工具身份不再重复发送。
					if conversionFailure == nil && !compatibleStreamTrailer(response) {
						conversionFailure = compatibleStreamError("Chat stream produced output after finish reason")
					}
					continue
				}
				if !compatibleToolIndexesKnown(response, state) {
					send(&schemas.BifrostStreamChunk{BifrostError: compatibleStreamError("Chat tool call has no ID or known index")})
					return
				}
				started := time.Now()
				events := chatToResponsesStreamEvents(response, state)
				orderCompatibleToolCompletions(events)
				schemas.AddStreamConvert(ctx, time.Since(started))
				for _, event := range events {
					if event.Type == schemas.ResponsesStreamResponseTypeCompleted || event.Type == schemas.ResponsesStreamResponseTypeIncomplete {
						pending = event
						continue
					}
					if !emit(event) {
						return
					}
				}
			}
		}
	}()
	return output, nil
}

// compatibleStreamTrailer 只接受不含生成内容的终态后块。
// 空 choices、空 delta、重复 role 或 finish_reason 均可携带尾部用量。
// 文本、推理、拒绝内容及工具增量都意味着模型仍在输出，不能当尾块丢掉。
// 元数据与多模态字段同样保守拒绝，避免静默遗漏客户端需要的信息。
// 上游错误在调用本函数之前处理，本判断不会掩盖失败响应。
func compatibleStreamTrailer(response *schemas.BifrostChatResponse) bool {
	// 即使转换器只消费首个 choice，也不能把其他 choice 的新输出当作合法尾块忽略。
	for _, choice := range response.Choices {
		if choice.ChatStreamResponseChoice == nil || choice.Delta == nil {
			continue
		}
		delta := choice.Delta
		// 空字符串不包含新输出；不能仅因供应商显式写了空 content 就报错。
		if delta.Content != nil && *delta.Content != "" || delta.Refusal != nil && *delta.Refusal != "" ||
			delta.Reasoning != nil && *delta.Reasoning != "" || len(delta.ReasoningDetails) > 0 ||
			delta.Audio != nil || len(delta.Annotations) > 0 || len(delta.ToolCalls) > 0 || len(delta.ExtraContent) > 0 {
			return false
		}
	}
	return true
}

// SDK 从 map 生成工具结束事件；Gemini 在此时才输出调用，必须保持原输出索引顺序。
func orderCompatibleToolCompletions(events []*schemas.BifrostResponsesStreamResponse) {
	toolCompletion := func(event *schemas.BifrostResponsesStreamResponse) bool {
		if event.OutputIndex == nil {
			return false
		}
		return event.Type == schemas.ResponsesStreamResponseTypeFunctionCallArgumentsDone ||
			event.Type == schemas.ResponsesStreamResponseTypeOutputItemDone && event.Item != nil &&
				event.Item.Type != nil && *event.Item.Type == schemas.ResponsesMessageTypeFunctionCall
	}
	for start := 0; start < len(events); {
		if !toolCompletion(events[start]) {
			start++
			continue
		}
		end := start + 1
		for end < len(events) && toolCompletion(events[end]) {
			end++
		}
		group := events[start:end]
		sequence := group[0].SequenceNumber
		sort.SliceStable(group, func(i, j int) bool {
			return *group[i].OutputIndex < *group[j].OutputIndex
		})
		for index, event := range group {
			event.SequenceNumber = sequence + index
		}
		start = end
	}
}

func compatibleToolIndexesKnown(response *schemas.BifrostChatResponse, state *schemas.ChatToResponsesStreamState) bool {
	if len(response.Choices) == 0 || response.Choices[0].ChatStreamResponseChoice == nil || response.Choices[0].Delta == nil {
		return true
	}
	var introduced map[uint16]bool
	for _, call := range response.Choices[0].Delta.ToolCalls {
		if call.ID != nil && *call.ID != "" {
			if introduced == nil {
				introduced = make(map[uint16]bool)
			}
			introduced[call.Index] = true
		} else if !introduced[call.Index] && state.ToolCallIndexToID[call.Index] == "" {
			return false
		}
	}
	return true
}

// 仅提取存在性，立刻清除原始文本。SDK 可将多个 JSON 事件以空白连接在同一捕获值中。
func takeCompatibleUsagePresence(raw *any) bool {
	value := *raw
	*raw = nil
	if value == nil {
		return false
	}
	body, ok := rawRequestJSON(value)
	if !ok {
		return false
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	for {
		var event struct {
			Usage json.RawMessage `json:"usage"`
		}
		if err := decoder.Decode(&event); err != nil {
			// 缺失或被截断的捕获不是零用量证据；保留 SDK 原本的流错误判断。
			return false
		}
		if len(event.Usage) > 0 && !bytes.Equal(bytes.TrimSpace(event.Usage), []byte("null")) {
			return true
		}
	}
}

func compatibleStreamError(message string) *schemas.BifrostError {
	return &schemas.BifrostError{
		StatusCode: schemas.Ptr(http.StatusBadGateway),
		Error:      &schemas.ErrorField{Type: schemas.Ptr("invalid_response_error"), Message: message},
	}
}
