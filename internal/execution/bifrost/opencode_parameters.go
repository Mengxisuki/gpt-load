package bifrost

import (
	"encoding/json"

	"github.com/sirupsen/logrus"

	"gpt-load/internal/channel"
	"gpt-load/internal/execution"
)

// normalizeOpenCodeContext 处理已验证目标不接受的 GPT 推理上下文选项。
// OpenCode 仍负责 Responses 工具与历史转换，本层不恢复 Chat 桥接。
// 依据最终上游模型判断，因此 Key 映射和渠道别名共享同一条规则。
// 精确列出实测模型：DeepSeek V4 Pro 已接受该字段，不能按非 GPT 泛化。
// 只取消 all_turns 这一不支持的推理选项，不删除 input 中的任何回合。
// effort、summary、加密历史及工具保持原样，不制造跨供应商私有状态兼容。
// 未知模型、字段类型或选项继续交给上游校验，避免掩盖坏请求。
// 返回值区分无改动、已归一化和序列化失败，供原生准备流程统一处理。
func normalizeOpenCodeContext(object map[string]json.RawMessage, spec execution.AttemptSpec) (bool, error) {
	if spec.ChannelID != string(channel.OpenCodeGo) && spec.ChannelID != string(channel.OpenCodeZen) {
		return false, nil
	}
	if spec.UpstreamModel != "kimi-k3" && spec.UpstreamModel != "glm-5.3" {
		return false, nil
	}
	// 只解码需要改动的对象，其他字段保留原始 JSON 精度与结构。
	// 不补造 reasoning；无选项的正常请求保持原有传输内容。
	var options map[string]json.RawMessage
	if json.Unmarshal(object["reasoning"], &options) != nil || !jsonStringEquals(options["context"], "all_turns") {
		return false, nil
	}
	delete(options, "context")
	// RawMessage 均来自已成功解码的合法 JSON，序列化不会引入新数据。
	// 序列化失败直接返回调用方，不吞错或继续发送部分修改后的正文。
	encoded, err := json.Marshal(options)
	if err != nil {
		return false, err
	}
	object["reasoning"] = encoded
	// 固定事件及字段便于定位某次适配，不记录任务正文、凭据或工具参数。
	// 一次尝试只在实际删除选项时记录，不对所有原生请求刷日志。
	logrus.WithFields(logrus.Fields{
		"event": "opencode.reasoning_context.normalized", "request_id": spec.RequestID,
		"channel_id": spec.ChannelID, "upstream_model": spec.UpstreamModel,
		"removed_parameter": "/reasoning/context",
	}).Info("已适配目标模型不支持的推理上下文选项")
	return true, nil
}
