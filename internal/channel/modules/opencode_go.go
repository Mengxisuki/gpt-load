package modules

import (
	"strings"

	"gpt-load/internal/channel/spec"
	"gpt-load/internal/execution"
	"gpt-load/internal/protocol"
)

func OpenCodeGo() spec.Module {
	return spec.Module{
		Definition: spec.Definition{
			ID:          spec.OpenCodeGo,
			Name:        "OpenCode Go",
			Mark:        "OG",
			Icon:        "opencode",
			Description: "Native multi-protocol API gateway",
			Connection: spec.Connection{
				Type:            spec.ConnectionAPIKey,
				CredentialInput: "batch_text",
			},
			Params: []spec.Field{{
				Key: "base_url", Label: "Base URL", InputKind: spec.InputURL,
				Normalizer: spec.NormalizeBaseURL,
			}},
			Credentials: []spec.Field{{
				Key: "api_key", Label: "API Key", InputKind: spec.InputSecret,
				Required: true, Sensitive: true, Normalizer: spec.NormalizeNonEmpty,
			}},
			Provider: spec.ProviderBinding{
				ProviderKind:      spec.ProviderMultiProtocolGateway,
				CatalogProviderID: "opencode-go",
				EndpointPolicy:    spec.EndpointFixedWithOverride,
				FixedBaseURL:      "https://opencode.ai/zen/go",
			},
			Routes: []spec.Route{
				spec.NewRoute(protocol.OpenAICompletions, execution.OperationChatCompletion, execution.RouteNative),
				spec.NewRoute(protocol.OpenAICompletions, execution.OperationListModels, execution.RouteNative),
				spec.NewRoute(protocol.OpenAICompletions, execution.OperationProbe, execution.RouteNative),
				// 官方目录将 GLM、Kimi、DeepSeek 声明为 Chat 模型；按最终目标选择转换。
				// GPT 与未知模型保留原生行为，不按源别名推断供应商能力。
				// 默认模式保持原接口声明，只有 resolver 命中的家族走转换。
				// 状态合同仍是 stateless，不因支持 Responses 而宣称可以保存会话。
				{
					ClientProtocol: protocol.OpenAIResponses, Operation: execution.OperationResponsesCreate,
					Mode: execution.RouteNative, ResponsesStoreHandling: spec.ResponsesStoreHandlingStateless,
					Resolver: "opencode_responses", PossibleModes: []execution.RouteMode{execution.RouteNative, execution.RouteConverted},
				},
				// 探测与实际执行使用同一模型判断，不能测试原生接口后实际走转换。
				// PossibleModes 明确允许的两种结果，编译器拒绝越界的 resolver。
				{
					ClientProtocol: protocol.OpenAIResponses, Operation: execution.OperationProbe,
					Mode: execution.RouteNative, Resolver: "opencode_responses",
					PossibleModes: []execution.RouteMode{execution.RouteNative, execution.RouteConverted},
				},
				spec.NewRoute(protocol.Anthropic, execution.OperationChatCompletion, execution.RouteNative),
				spec.NewRoute(protocol.Anthropic, execution.OperationProbe, execution.RouteNative),
			},
		},
		// 两个 OpenCode 入口共用一份模型族规则，防止配置随版本漂移。
		Extensions: spec.Extensions{RouteResolvers: map[spec.RouteResolverID]spec.RouteResolver{
			"opencode_responses": resolveOpenCodeResponsesRoute,
		}},
	}
}

// resolveOpenCodeResponsesRoute 只维护已核实的模型家族，不把显示目录当执行能力表。
// Go 与 Zen 共用官方 Chat 端点合同；来源是 https://opencode.ai/docs/zen/。
// 新增模型家族前应核实端点并补 HTTP 回归，未知模型不自动降级。
// 匹配官方模型 ID，不使用客户端可自定义的别名来猜测能力。
func resolveOpenCodeResponsesRoute(model string, defaultMode execution.RouteMode) execution.RouteMode {
	for _, prefix := range []string{"glm-", "kimi-", "deepseek-"} {
		if strings.HasPrefix(model, prefix) {
			return execution.RouteConverted
		}
	}
	return defaultMode
}
