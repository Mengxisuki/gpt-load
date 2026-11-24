package modules

import (
	"gpt-load/internal/channel/spec"
	"gpt-load/internal/execution"
	"gpt-load/internal/protocol"
)

const (
	CodexSubscriptionDriver     spec.SubscriptionDriverID = "codex"
	CodexModelDiscovery         spec.UtilityID            = "codex_models"
	CodexQuotaObservation       spec.UtilityID            = "codex_quota"
	CodexResetCreditAction      spec.ActionID             = "codex_reset_credit"
	CodexDefaultBaseURL                                   = "https://chatgpt.com"
	codexResponsesRouteResolver spec.RouteResolverID      = "codex_responses_model"
)

// Codex declares the subscription-backed Codex channel and its supported routes.
func Codex() spec.Module {
	// 在选定实际模型后区分原生文本接口和需要改走 Images 的纯生图模型。
	// 保留无状态语义，转换产生的 Responses ID 不能在订阅上游续接。
	responses := spec.NewResponsesCreateRoute(execution.RouteNative, spec.ResponsesStoreHandlingStateless)
	responses.Resolver = codexResponsesRouteResolver
	responses.PossibleModes = []execution.RouteMode{execution.RouteNative, execution.RouteConverted}
	return spec.Module{
		Definition: spec.Definition{
			ResponsesWebsocket: execution.WebsocketCapabilities{Native: true, Continuation: true, Prewarm: true, StoredResponses: false, Multiplex: false},

			ID:          spec.Codex,
			Name:        "Codex",
			Mark:        "CX",
			Icon:        "codex",
			SearchTerms: []string{"subscription", "oauth", "chatgpt"},
			Description: "OpenAI Codex subscription",
			Connection: spec.Connection{
				Type:            spec.ConnectionSubscription,
				CredentialInput: "authorization",
				AuthorizationMethods: []spec.AuthorizationMethod{
					spec.AuthorizationBrowserOAuth,
					spec.AuthorizationOAuthFile,
				},
			},
			Params: []spec.Field{{
				Key: "base_url", Label: "Base URL", InputKind: spec.InputURL,
				Normalizer: spec.NormalizeOptionalHTTPSBaseURL,
			}},
			Credentials: []spec.Field{},
			Provider: spec.ProviderBinding{
				ProviderKind:    spec.ProviderCodex,
				EndpointPolicy:  spec.EndpointSDKDefault,
				DefaultBaseURLs: []string{CodexDefaultBaseURL},
			},
			Routes: []spec.Route{
				spec.NewRoute(protocol.CodexLive, execution.OperationLiveCall, execution.RouteNative),
				spec.NewRoute(protocol.OpenAICompletions, execution.OperationChatCompletion, execution.RouteConverted),
				spec.NewRoute(protocol.OpenAIImages, execution.OperationImagesGenerate, execution.RouteNative),
				spec.NewRoute(protocol.OpenAIImages, execution.OperationImagesEdit, execution.RouteNative),
				responses,
				spec.NewRoute(protocol.OpenAIResponses, execution.OperationResponsesInputTokens, execution.RouteNative),
				spec.NewRoute(protocol.OpenAIResponses, execution.OperationWebSearch, execution.RouteNative),
				spec.NewRoute(protocol.Anthropic, execution.OperationChatCompletion, execution.RouteConverted),
				spec.NewRoute(protocol.Anthropic, execution.OperationCountTokens, execution.RouteConverted),
				spec.NewRoute(protocol.Gemini, execution.OperationChatCompletion, execution.RouteConverted),
				spec.NewRoute(protocol.Gemini, execution.OperationCountTokens, execution.RouteConverted),
			},
			Capabilities: spec.CapabilityBindings{
				SubscriptionDriver: CodexSubscriptionDriver,
				ModelDiscovery:     CodexModelDiscovery,
				QuotaObservation:   CodexQuotaObservation,
				ResetCreditAction:  CodexResetCreditAction,
			},
		},
		Extensions: spec.Extensions{RouteResolvers: map[spec.RouteResolverID]spec.RouteResolver{
			codexResponsesRouteResolver: resolveCodexResponsesRoute,
		}},
	}
}

// 仅已验证的 Sunburst 模型使用本次 Responses 生图适配。
// 使用上游模型 ID，确保配置了公开别名的同一模型也走正确的执行路径。
// 其他模型继续使用该渠道声明的默认路由。
func resolveCodexResponsesRoute(model string, defaultMode execution.RouteMode) execution.RouteMode {
	if model == "gpt-image-2.6-sunburst" {
		return execution.RouteConverted
	}
	return defaultMode
}
