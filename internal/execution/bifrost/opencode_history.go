package bifrost

import (
	"encoding/json"

	"github.com/sirupsen/logrus"

	"gpt-load/internal/channel"
	"gpt-load/internal/execution"
)

// normalizeOpenCodeToolHistory 适配已复现的 Go/DeepSeek 并行结果配对限制。
// 上游会将 developer 消息隔开的后方工具结果误报为缺失。
// 仅将完整批次内的 developer 消息后置，结果之间的相对顺序保持不变。
// 每个原始项目仍用 RawMessage 保存，图片、扩展字段和大整数不会丢失。
// 不跨越用户/助手/未知项目，不移动调用前或下一轮的指令。
// 无法无歧义配对时保留原请求，由原有上游校验明确报告错误。
// 调用者只在 Responses 创建路径执行，compact 与 Chat 不经过此处。
// 返回值表示是否写回 input；无变化时保留原始 JSON 的编码形态。
// 此规则只改变下一次模型推理所见的历史顺序，不执行任何客户端工具。
// 延后的指令仍在本次推理之前，既有工具结果不因重排重新生成。
func normalizeOpenCodeToolHistory(object map[string]json.RawMessage, spec execution.AttemptSpec) (bool, error) {
	// 能力边界按最终上游模型判断，客户端别名不能扩大适用范围。
	// Zen 和其他 DeepSeek 版本尚未验证，不从相似名称推断兼容。
	if spec.ChannelID != string(channel.OpenCodeGo) || spec.UpstreamModel != "deepseek-v4-pro" {
		return false, nil
	}
	var items []json.RawMessage
	// input 也可以是纯文本或 null，这些输入无需工具历史适配。
	// 解析不符合数组合同只代表规则不适用，不替上游宣称输入合法。
	if json.Unmarshal(object["input"], &items) != nil {
		return false, nil
	}
	// 轻量索引只读取识别和配对字段，正文项目始终从原始字节取得。
	// 全历史重复 ID 会使归属不明确，不能只在某一小段中假装配对成功。
	// 错误类型不在这里修复，避免把坏值变成空字符串后误匹配。
	type itemInfo struct {
		Type   string          `json:"type"`
		Role   string          `json:"role"`
		CallID string          `json:"call_id"`
		Output json.RawMessage `json:"output"`
	}
	info := make([]itemInfo, len(items))
	// 调用与结果分别去重：同一 ID 在两类项目中各出现一次是正确配对。
	// output 只检查外部形状，不对图片解码、转码或改写内容。
	calls, outputs := make(map[string]bool), make(map[string]bool)
	for i, raw := range items {
		if json.Unmarshal(raw, &info[i]) != nil {
			return false, nil
		}
		item := info[i]
		switch item.Type {
		case "function_call":
			if item.CallID == "" || calls[item.CallID] {
				return false, nil
			}
			calls[item.CallID] = true
		case "function_call_output":
			// 结果只接受协议原有的字符串或多模态数组，不补造缺失正文。
			// 内容是否有效仍由上游校验，本层只保护配对所需的外部形状。
			if item.CallID == "" || outputs[item.CallID] || len(item.Output) == 0 || (item.Output[0] != '"' && item.Output[0] != '[') {
				return false, nil
			}
			outputs[item.CallID] = true
		}
	}
	changed := false
	// 扫描游标仅前进，不反复搜索后续历史，整体复杂度为线性。
	// 结果顺序和消息顺序分别稳定保留，不能按 ID 排序替代原先次序。
	for start := 0; start < len(items); {
		if info[start].Type != "function_call" {
			start++
			continue
		}
		// 一批调用必须连续；新调用或任何其他事件都会形成明确边界。
		// 不把分散在不同回合的同名函数合并，也不改变调用次序。
		pending := make(map[string]bool)
		end := start
		for end < len(items) && info[end].Type == "function_call" {
			pending[info[end].CallID] = true
			end++
		}
		resultsStart := end
		// 暂存当前块直至确认全部结果到齐，避免边扫描边修改失败请求。
		// 单调用和未匹配结果不触发整理，但仍会原样交给上游处理。
		var results, messages []json.RawMessage
		valid, move := len(pending) > 1, false
		for end < len(items) {
			item := info[end]
			if item.Type == "function_call_output" {
				if !pending[item.CallID] {
					valid = false
				}
				delete(pending, item.CallID)
				results = append(results, items[end])
				move = move || len(messages) > 0
			} else if (item.Type == "message" || item.Type == "") && item.Role == "developer" {
				// 本轮仅验证结果之间的消息；第一份结果之前出现指令时不猜测。
				// 完整结果后已有的消息按原顺序保留，单独不会触发改写。
				valid = valid && len(results) > 0
				messages = append(messages, items[end])
			} else {
				// 用户消息可能撤销或改变任务，不能向后越过该消息取结果。
				// 未知工具事件也保留其边界，未来协议扩展不会被静默合并。
				break
			}
			end++
		}
		// 只有整批齐全才写回，不能输出部分排序后的失败历史。
		// 空 pending 也不足以单独证明合法，陌生或重复结果会使 valid 失败。
		// 每个结果和消息只拷贝一次，长度保持不变，后续批次索引仍有效。
		if valid && len(pending) == 0 && move {
			copy(items[resultsStart:], results)
			copy(items[resultsStart+len(results):], messages)
			changed = true
		}
		start = end
	}
	// 如果只是尾随 developer 消息而没有结果在它之后，排序没有收益。
	// 此时直接返回，保持幂等并减少正常请求的分配与日志。
	if !changed {
		return false, nil
	}
	// 仅序列化 input；其他顶层字段沿用原生准备流程，不污染共享 Body。
	// 失败直接返回执行器，不能继续发送不完整修改或隐藏编码错误。
	encoded, err := json.Marshal(items)
	if err != nil {
		return false, err
	}
	object["input"] = encoded
	// 序列化成功后才替换对象字段；编码错误不会留下半成品。
	// 记录发生适配的事实与请求关联，不记录任务、图片、工具参数或凭据。
	// 不在未命中请求上刷日志，便于后续核对具体请求是否完成了排序。
	logrus.WithFields(logrus.Fields{
		"event": "opencode.tool_history.normalized", "request_id": spec.RequestID,
		"channel_id": spec.ChannelID, "upstream_model": spec.UpstreamModel,
	}).Info("已保持并行工具结果连续并保留 developer 消息")
	return true, nil
}
