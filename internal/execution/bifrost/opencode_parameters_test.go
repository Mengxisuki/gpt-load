package bifrost

import (
	"bytes"
	"testing"

	"github.com/tidwall/gjson"

	"gpt-load/internal/execution"
	"gpt-load/internal/protocol"
)

// TestOpenCodeContextBoundary 验证最终目标、渠道与协议三个生效边界。
// 调用真实透传准备入口，防止 helper 测试通过但执行器未接入。
// 对未命中的情况逐字比较，避免空对象重建或意外丢失未知字段。
// 兼容不能改变 effort、summary、工具或用户提供的完整历史。
// 多个模型共用 public 别名，证明规则不能依据客户端别名判断。
// 用户显式提供未知 context 时保持原样，让上游给出真实校验错误。
// 普通 Chat 与其他供应商不受 OpenCode Responses 规则影响。
// Kimi 与 GLM 分别覆盖两个 OpenCode 渠道，防止规则只接入 Go。
// DeepSeek 是实测支持该选项的反例，不能用家族黑名单代替能力证据。
// GPT 路径仍需保留原生推理策略，不能被映射修复连带降级。
// 未知 Kimi 后续型号不继承当前限制，避免误判未来供应商能力。
// 空 reasoning 与缺少 context 都是合法输入，不应新增默认对象。
// 错误类型本来应由供应商报错，适配不能将其当作成功移除的选项。
// 仅包含 context 的对象在归一化后为空，依旧必须是合法 JSON 对象。
// 规则读取已冻结的最终模型，不读取可在下一请求变更的 Key 配置。
// 已接受的普通函数与 custom 历史往返由完整网关回归另外覆盖。
// 本用例专门保护参数适配的边界，不复制真实调用链的所有样本。
func TestOpenCodeContextBoundary(t *testing.T) {
	for _, test := range []struct {
		name, channel, model, reasoning string
		chat, normalize                 bool
	}{
		{"kimi", "opencode_go", "kimi-k3", `{"context":"all_turns","effort":"high","summary":"detailed"}`, false, true},
		{"glm", "opencode_zen", "glm-5.3", `{"context":"all_turns"}`, false, true},
		{"deepseek", "opencode_go", "deepseek-v4-pro", `{"context":"all_turns"}`, false, false},
		{"gpt", "opencode_go", "gpt-6.1-terra", `{"context":"all_turns"}`, false, false},
		{"unknown", "opencode_go", "kimi-future", `{"context":"all_turns"}`, false, false},
		{"other_channel", "openai", "kimi-k3", `{"context":"all_turns"}`, false, false},
		{"chat", "opencode_go", "kimi-k3", `{"context":"all_turns"}`, true, false},
		{"null", "opencode_go", "kimi-k3", `null`, false, false},
		{"bad_type", "opencode_go", "kimi-k3", `"all_turns"`, false, false},
		{"bad_context", "opencode_go", "kimi-k3", `{"context":123}`, false, false},
		{"unknown_context", "opencode_go", "kimi-k3", `{"context":"future"}`, false, false},
		{"no_context", "opencode_go", "kimi-k3", `{"effort":"high"}`, false, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			// 非空工具与历史作为哨兵，不能用最小空请求掩盖误删。
			// 使用相同模型避免模型别名替换影响逐字比较。
			// 流式与普通调用使用相同的正文准备函数。
			body := []byte(`{"model":"` + test.model + `","stream":false,"reasoning":` + test.reasoning + `,"input":[{"role":"user","content":"synthetic"}],"tools":[{"type":"web_search"}]}`)
			spec := execution.AttemptSpec{ChannelID: test.channel, UpstreamModel: test.model, ClientModel: "public", Body: body, ClientProtocol: protocol.OpenAIResponses, Operation: execution.OperationResponsesCreate}
			if test.chat {
				spec.ClientProtocol, spec.Operation = protocol.OpenAICompletions, execution.OperationChatCompletion
			}
			got, err := sanitizeNativeRequestBody(spec, false)
			if err != nil {
				t.Fatal(err)
			}
			// 非命中请求必须无额外变化，含坏类型也不能悄悄修复。
			// 本层只处理已验证选项，保持上游对其他值的校验职责。
			if !test.normalize {
				if !bytes.Equal(got, body) {
					t.Fatalf("unrelated request changed: %s", got)
				}
				return
			}
			if gjson.GetBytes(got, "reasoning.context").Exists() {
				t.Fatal("unsupported context still forwarded")
			}
			// 按具体路径检查原始语义，缺失选项也不能变成默认值。
			// 保留工具声明不代表供应商搜索已可用，真实搜索另行验收。
			for _, path := range []string{"input", "tools", "reasoning.effort", "reasoning.summary"} {
				if gjson.GetBytes(got, path).Raw != gjson.GetBytes(body, path).Raw {
					t.Fatalf("%s changed", path)
				}
			}
			// 共享原始请求不能被就地修改，否则后续候选会继承本次适配。
			if !gjson.GetBytes(spec.Body, "reasoning.context").Exists() {
				t.Fatal("attempt mutated its input")
			}
		})
	}
}
