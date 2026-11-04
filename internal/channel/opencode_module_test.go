package channel

import (
	"encoding/json"
	"testing"

	"gpt-load/internal/execution"
	"gpt-load/internal/protocol"
)

func TestOpenCodePresetsResolveNativeGatewayRoutes(t *testing.T) {
	registry := NewRegistry()
	for _, test := range []struct {
		id      ID
		baseURL string
	}{
		{"opencode_go", "https://opencode.ai/zen/go"},
		{"opencode_zen", "https://opencode.ai/zen"},
	} {
		t.Run(string(test.id), func(t *testing.T) {
			for _, override := range []string{"", "https://relay.example/team"} {
				params := json.RawMessage(`{}`)
				wantURL := test.baseURL
				if override != "" {
					params = json.RawMessage(`{"base_url":"` + override + `/"}`)
					wantURL = override
				}
				target, err := registry.Resolve(test.id, params)
				if err != nil {
					t.Fatal(err)
				}
				if target.ProviderKind != ProviderMultiProtocolGateway || string(target.TargetConfig) != `{"base_url":"`+wantURL+`"}` {
					t.Fatalf("unexpected gateway target: %+v", target)
				}
				for _, p := range []protocol.Protocol{protocol.OpenAICompletions, protocol.OpenAIResponses, protocol.Anthropic} {
					operation := execution.OperationChatCompletion
					if p == protocol.OpenAIResponses {
						operation = execution.OperationResponsesCreate
					}
					if mode, ok := target.Mode(p, operation); !ok || mode != RouteNative {
						t.Errorf("%s must use a native route, got %s / %t", p, mode, ok)
					}
				}
				// 原先的模型前缀不得再改变协议；执行与探测使用同一原生合同。
				// 未知模型也由上游判定能力，不在目录或名称层新增白名单。
				// 同时覆盖默认和自定义根地址，不能仅默认配置返回正确路由。
				// 直接查询已编译目标，不在测试中写入或覆盖最终模式。
				for _, model := range []string{"kimi-k3", "glm-5.3", "deepseek-v4.1-flash", "gpt-6-luna", "future-model"} {
					for _, operation := range []execution.Operation{execution.OperationResponsesCreate, execution.OperationProbe} {
						if mode, ok := target.ModeForModel(protocol.OpenAIResponses, operation, model); !ok || mode != RouteNative {
							t.Errorf("%s/%s must stay native, got %s/%t", model, operation, mode, ok)
						}
					}
				}
				if _, ok := target.Mode(protocol.Gemini, execution.OperationChatCompletion); ok {
					t.Error("Gemini must not be advertised without its upstream path adaptation")
				}
				if _, ok := target.Mode(protocol.OpenAIResponses, execution.OperationResponsesRetrieve); ok {
					t.Error("Responses resource operations must not be advertised")
				}
			}
			descriptor, _ := registry.Get(test.id)
			for _, route := range descriptor.Routes {
				if route.RouteMode != RouteNative {
					t.Errorf("unexpected conversion route: %+v", route)
				}
			}
		})
	}
}
