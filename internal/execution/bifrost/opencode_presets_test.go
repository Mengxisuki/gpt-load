package bifrost

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/tidwall/gjson"

	"gpt-load/internal/channel"
	"gpt-load/internal/execution"
	"gpt-load/internal/protocol"
	"gpt-load/internal/provideradapter"
	"gpt-load/internal/state"
)

func TestOpenCodePresetsPreserveNativeProtocols(t *testing.T) {
	for _, id := range []channel.ID{"opencode_go", "opencode_zen"} {
		for _, test := range []struct {
			protocol protocol.Protocol
			path     string
			body     string
			response string
			stream   string
		}{
			{
				protocol: protocol.OpenAICompletions, path: "/v1/chat/completions",
				body:     `{"model":"client-model","messages":[{"role":"user","content":"hello"}]}`,
				response: `{"id":"chat_1","object":"chat.completion","model":"upstream-model","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`,
				stream:   "data: {\"id\":\"chat_1\",\"object\":\"chat.completion.chunk\",\"model\":\"upstream-model\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1,\"total_tokens\":2}}\n\ndata: [DONE]\n\n",
			},
			{
				protocol: protocol.OpenAIResponses, path: "/v1/responses",
				body:     `{"model":"client-model","input":"hello","store":false}`,
				response: `{"id":"resp_1","object":"response","status":"completed","model":"upstream-model","output":[],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`,
				stream:   openAIResponsesStreamFixture,
			},
			{
				protocol: protocol.Anthropic, path: "/v1/messages",
				body:     `{"model":"client-model","max_tokens":16,"messages":[{"role":"user","content":"hello"}]}`,
				response: `{"id":"msg_1","type":"message","role":"assistant","model":"upstream-model","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`,
				stream:   anthropicResponsesStreamFixture,
			},
		} {
			for _, mode := range []string{"unary", "stream", "probe"} {
				t.Run(string(id)+"/"+string(test.protocol)+"/"+mode, func(t *testing.T) {
					prefix := "/zen"
					if id == "opencode_go" {
						prefix += "/go"
					}
					server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						if r.Method != http.MethodPost || r.URL.Path != prefix+test.path {
							t.Errorf("unexpected target %s %s", r.Method, r.URL.Path)
						}
						wantAuth, wantAPIKey := "Bearer "+testAPIKey, ""
						if test.protocol == protocol.Anthropic {
							wantAuth, wantAPIKey = "", testAPIKey
						}
						if r.Header.Get("Authorization") != wantAuth || r.Header.Get("X-Api-Key") != wantAPIKey {
							t.Error("selected credential did not replace client authentication")
						}
						body, err := io.ReadAll(r.Body)
						if err != nil {
							t.Error(err)
							return
						}
						var payload map[string]json.RawMessage
						if err := json.Unmarshal(body, &payload); err != nil {
							t.Error(err)
							return
						}
						if string(payload["model"]) != `"upstream-model"` || payload["api_key"] != nil {
							t.Errorf("unexpected upstream body %s", body)
						}
						if mode != "probe" {
							if !bytes.Contains(payload["vendor_extension"], []byte(`1.2300`)) {
								t.Errorf("native extension changed: %s", body)
							}
							if r.Header.Get("User-Agent") != "coding-client/1.0" || r.Header.Get("X-Opencode-Session") != "conversation-one" {
								t.Error("coding client identity or conversation header was lost")
							}
						}
						if mode == "stream" {
							w.Header().Set("Content-Type", "text/event-stream")
							_, _ = io.WriteString(w, test.stream)
							w.(http.Flusher).Flush()
						} else {
							w.Header().Set("Content-Type", "application/json")
							_, _ = io.WriteString(w, test.response)
						}
					}))
					defer server.Close()
					manager, target := newOpenCodeManagerForTest(t, id, server.URL+prefix)
					operation := execution.OperationChatCompletion
					if test.protocol == protocol.OpenAIResponses {
						operation = execution.OperationResponsesCreate
					}
					body := strings.TrimSuffix(test.body, "}") + `,"vendor_extension":{"value":1.2300},"api_key":"client-secret"}`
					if mode == "stream" {
						body = strings.TrimSuffix(body, "}") + `,"stream":true}`
					}
					if mode == "probe" {
						operation = execution.OperationProbe
					}
					spec := utilitySpec(id, test.protocol, operation, http.MethodPost, test.path, []byte(body))
					spec.ClientModel, spec.UpstreamModel = "client-model", "upstream-model"
					spec.TargetConfig = target.TargetConfig
					spec.Header.Set("Content-Type", "application/json")
					spec.Header.Set("Authorization", "Bearer client-secret")
					spec.Header.Set("X-Api-Key", "client-secret")
					spec.Header.Set("User-Agent", "coding-client/1.0")
					spec.Header.Set("X-Opencode-Session", "conversation-one")
					if mode == "stream" {
						spec.ClientModel = spec.UpstreamModel
					}
					if mode == "probe" {
						spec.Method, spec.Path = "", ""
						spec.Body = nil
					}
					spec = freezeTestAttempt(spec)
					if mode == "stream" {
						var data bytes.Buffer
						result := manager.ExecuteStream(t.Context(), spec, func(event execution.StreamEvent) error {
							if event.Kind == execution.StreamEventData {
								data.Write(event.Data)
							}
							return nil
						})
						if data.String() != test.stream {
							t.Errorf("native SSE changed: %q", data.String())
						}
						if err := result.Validate(); err != nil || result.Error != nil || result.StatusCode != http.StatusOK || result.UpstreamProtocol != test.protocol || result.Usage == nil {
							t.Fatalf("stream result=%+v error=%+v contract=%v", result, result.Error, err)
						}
						return
					}
					result := manager.Execute(t.Context(), spec)
					if mode != "probe" {
						var got, want map[string]any
						if err := json.Unmarshal(result.Body, &got); err != nil {
							t.Fatal(err)
						}
						if err := json.Unmarshal([]byte(test.response), &want); err != nil {
							t.Fatal(err)
						}
						want["model"] = "client-model"
						if !reflect.DeepEqual(got, want) {
							t.Errorf("native response or model alias changed: %s", result.Body)
						}
					}
					if err := result.Validate(); err != nil || result.Error != nil || result.StatusCode != http.StatusOK || result.UpstreamProtocol != test.protocol {
						t.Fatalf("result=%+v error=%+v contract=%v", result, result.Error, err)
					}
					if mode != "probe" && result.Usage == nil {
						t.Error("native usage was lost")
					}
				})
			}
		}
	}
}

func TestOpenCodePresetsDiscoverModelsAtGatewayRoot(t *testing.T) {
	for _, id := range []channel.ID{"opencode_go", "opencode_zen"} {
		t.Run(string(id), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet || r.URL.Path != "/custom-root/v1/models" || r.Header.Get("Authorization") != "Bearer "+testAPIKey {
					t.Errorf("unexpected model discovery target %s %s", r.Method, r.URL.Path)
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"object":"list","data":[{"id":"model-one"}]}`)
			}))
			defer server.Close()
			manager, target := newOpenCodeManagerForTest(t, id, server.URL+"/custom-root")
			spec := utilitySpec(id, protocol.OpenAICompletions, execution.OperationListModels, http.MethodGet, "/v1/models", nil)
			spec.TargetConfig = target.TargetConfig
			result := manager.Execute(t.Context(), freezeTestAttempt(spec))
			if err := result.Validate(); err != nil || result.Error != nil || !bytes.Contains(result.Body, []byte(`"id":"model-one"`)) {
				t.Fatalf("models result=%+v contract=%v", result, err)
			}
		})
	}
}

func newOpenCodeManagerForTest(t *testing.T, id channel.ID, baseURL string) (*RuntimeManager, channel.ResolvedTarget) {
	t.Helper()
	registry := channel.NewRegistry()
	target, err := registry.Resolve(id, json.RawMessage(`{"base_url":"`+baseURL+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	manager, err := newRuntimeManager(runtimeOptions{allowPrivateNetwork: true}, registry)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(manager.Shutdown)
	if err := manager.Reconcile([]provideradapter.RuntimeTarget{{Target: target}}); err != nil {
		t.Fatal(err)
	}
	return manager, target
}

// 用真实执行器和 HTTP 边界复现新任务的参数拒绝，不 mock 协议转换函数。
// 源名仍是 GPT；目标由冻结路由决定，覆盖 New API 两种映射进入后的共同形状。
// 三类目标共用 Chat 协议，返回仍须符合客户端的 Responses 合同。
// 断言覆盖请求及响应两端，避免只验证请求不报错。
// 流和非流分别走不同 SDK 路径，因此同一模型都需要执行两次。
// 错误响应刻意采用现场报错，但业务正文完全是合成数据。
func TestOpenCodeMappedResponsesUsesChat(t *testing.T) {
	// 两个入口的 base_url 前缀不同，但应具有相同的模型族策略。
	// 每例独立创建运行时，避免前一个模型留下连接或路由缓存影响结果。
	// GLM/Kimi 来自现场报错，DeepSeek 覆盖用户提出的另一类目标。
	// 不向真实厂商发请求，也不依赖生产组或已登录账号。
	for _, id := range []channel.ID{channel.OpenCodeGo, channel.OpenCodeZen} {
		for _, model := range []string{"glm-5.3", "kimi-k3", "deepseek-v4.1-flash"} {
			for _, stream := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/stream=%t", id, model, stream), func(t *testing.T) {
					// 只捕获模拟服务器实际收到的正文，不能从请求草稿推导通过结果。
					var received []byte
					// 上游仅接受 Chat；旧实现在 /responses 上稳定返回现场同类错误。
					// 同时保留真实 HTTP 序列化，捕获模型、指令与工具 schema 的丢失。
					server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						w.Header().Set("Content-Type", "application/json")
						if r.URL.Path != "/custom-root/v1/chat/completions" {
							w.WriteHeader(400)
							io.WriteString(w, `{"error":{"message":"reasoning.context: this reasoning option requires a native Responses target"}}`)
							return
						}
						// 模拟服务使用内存正文；后续字段断言同时检查成功读取。
						// 认证仍由原执行器写入，转换不能沿用客户端凭据。
						received, _ = io.ReadAll(r.Body)
						if r.Header.Get("Authorization") != "Bearer "+testAPIKey {
							t.Error("upstream credential missing")
						}
						// 非流返回函数调用，流式返回文本，分别覆盖两种响应重建。
						if stream {
							w.Header().Set("Content-Type", "text/event-stream")
							io.WriteString(w, openAIChatFinalStream)
						} else {
							io.WriteString(w, `{"id":"chat_1","object":"chat.completion","created":1,"model":"served","choices":[{"index":0,"message":{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"read_file","arguments":"{}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":12,"completion_tokens":3,"total_tokens":15}}`)
						}
					}))
					defer server.Close()
					// 自定义前缀保护 /v1 拼接，避免只在默认地址上碰巧通过。
					// manager 运行真实 Bifrost Core，只有网络另一端是测试替身。
					// 凭据使用仓库公共 fixture，测试输出不包含任何线上信息。
					manager, target := newOpenCodeManagerForTest(t, id, server.URL+"/custom-root")
					// 同时携带 context、effort 与 encrypted_content 请求选项，
					// 防止修复只删一个字段却把下一项不兼容参数继续发给目标。
					// include 是输出偏好，与已经存在的加密输入历史不是同一语义。
					// instructions 与 input 并存，转换不能让其中一个覆盖另一个。
					body := []byte(fmt.Sprintf(`{"model":"gpt-6-luna","stream":%t,"store":false,"instructions":"Keep instructions","input":[{"role":"user","content":"Read file"}],"reasoning":{"effort":"high","context":"all_turns","summary":"auto"},"include":["reasoning.encrypted_content"],"tools":[{"type":"function","name":"read_file","parameters":{"type":"object","properties":{}}}]}`, stream))
					spec := utilitySpec(id, protocol.OpenAIResponses, execution.OperationResponsesCreate, http.MethodPost, "/v1/responses", body)
					spec.TargetConfig, spec.ClientModel, spec.UpstreamModel = target.TargetConfig, "gpt-6-luna", model
					// 使用注册表冻结真实目标的 RouteMode，不在测试中强行指定转换模式。
					spec = freezeTestAttempt(spec)
					// 执行前后的输入必须相同，避免重试或并行请求共享可变草稿。
					original := spec.Clone()
					if stream {
						// StreamEventData 是客户端真正收到的事件，不读取 SDK 内部状态。
						// 后续要求完整终态，避免仅建立流连接就被测试算作成功。
						var output bytes.Buffer
						result := manager.ExecuteStream(t.Context(), spec, func(event execution.StreamEvent) error {
							if event.Kind == execution.StreamEventData {
								output.Write(event.Data)
							}
							return nil
						})
						if result.Error != nil || result.StatusCode != http.StatusOK {
							t.Fatalf("execution failed: %+v", result.Error)
						}
						// 日志依赖这个公开字段解释转换后的实际协议。
						if result.UpstreamProtocol != protocol.OpenAICompletions {
							t.Errorf("unexpected upstream protocol: %s", result.UpstreamProtocol)
						}
						if !strings.Contains(output.String(), "response.completed") || !strings.Contains(output.String(), "done") {
							t.Errorf("missing completed text: %s", output.String())
						}
						// 上游有用量时必须保留证据，不能因格式转换变成未知或免费。
						if result.Usage == nil {
							t.Error("usage missing")
						}
					} else {
						result := manager.Execute(t.Context(), spec)
						if result.Error != nil || result.StatusCode != http.StatusOK {
							t.Fatalf("execution failed: %+v", result.Error)
						}
						// 日志依赖这个公开字段解释转换后的实际协议。
						if result.UpstreamProtocol != protocol.OpenAICompletions {
							t.Errorf("unexpected upstream protocol: %s", result.UpstreamProtocol)
						}
						// 下游仍使用 Responses，直接透传 Chat 的 choices 会破坏客户端。
						// 函数名称和 call_id 是下一回合回传结果的关联键。
						// 不能只保留输出文字而丢失需要客户端执行的工具调用。
						if gjson.GetBytes(result.Body, "object").String() != "response" || gjson.GetBytes(result.Body, "output.0.call_id").String() != "call_1" || gjson.GetBytes(result.Body, "output.0.name").String() != "read_file" {
							t.Errorf("tool call not preserved: %s", result.Body)
						}
						// 上游有用量时必须保留证据，不能因格式转换变成未知或免费。
						if result.Usage == nil {
							t.Error("usage missing")
						}
					}
					if !bytes.Equal(original.Body, spec.Body) {
						t.Error("caller body mutated")
					}
					// 对真实线上的四个关键字段逐项核对，不复制转换算法计算期望。
					// 目标模型决定路由，源别名不应出现在上游 model 中。
					// 用户消息与全局指令分别保留，函数 schema 走原 SDK 编码。
					for path, want := range map[string]string{"model": model, "messages.0.content": "Keep instructions", "messages.1.content": "Read file", "tools.0.function.name": "read_file"} {
						if got := gjson.GetBytes(received, path).String(); got != want {
							t.Errorf("%s=%q, want %q", path, got, want)
						}
					}
					// 复用 Compatible 的推理语义：不给其他厂商伪造 GPT 专属推理档位。
					for _, path := range []string{"reasoning", "reasoning_effort", "include"} {
						if gjson.GetBytes(received, path).Exists() {
							t.Errorf("GPT-only parameter retained: %s", path)
						}
					}
				})
			}
		}
	}
}

// 新任务转换只接受可移植历史；在真正发送 HTTP 前验证拒绝与保留边界。
// 请求主体使用合成数据，不复制用户线上任务或凭据。
// 这些失败必须在派发前明确，不能消耗供应商额度后再提示不支持。
// 成功用例保护新任务生成工具调用后的正常第二轮。
// 不承诺模型切换时能把旧厂商的私有状态迁移到新厂商。
func TestOpenCodeResponsesStateAndToolContinuation(t *testing.T) {
	// 动态工具声明与普通回合共用历史，确保成功不是靠丢弃调用结果实现。
	const toolTurn = `[{"role":"user","content":"read"},{"type":"function_call","call_id":"call_1","name":"read_file","arguments":"{}"},{"type":"function_call_output","call_id":"call_1","output":[{"type":"input_text","text":"file contents"}]}]`
	// additional_tools 在输入里携带定义，目标 Chat 请求仍须得到完整函数 schema。
	dynamicTurn := `[{"type":"additional_tools","tools":[{"type":"function","name":"read_file","parameters":{"type":"object","properties":{"path":{"type":"string"}}}}]},` + toolTurn[1:]
	// 失败用例只改变影响可移植性的状态，正文其余部分保持最小有效。
	// 不把 previous_response_id 当普通字符串删掉后重新开始任务。
	// 加密推理即使同时存在文字摘要，也不能假装完整迁移。
	// 压缩记录和厂商内置工具都有独立的上游状态。
	// 未适配的自定义语法不是 JSON 函数 schema，不能静默忽略。
	for _, tc := range []struct {
		name, extra, input string
		reject             bool
		dynamic            bool
		historyType        string
	}{
		{name: "hosted tool", extra: `,"tools":[{"type":"web_search"}]`, input: `"hello"`, reject: true},
		{name: "custom tool", extra: `,"tools":[{"type":"custom","name":"patch"}]`, input: `"hello"`, reject: true},
		{name: "hosted tool history", input: `[{"type":"web_search_call","id":"ws_1"}]`, reject: true, historyType: "web_search_call"},
		// 未实现的历史保留拒绝，但日志须能区分类型，不再只返回同一笼统原因。
		// custom 现在进入专用适配校验；缺少受支持声明时仍应明确拒绝。
		{name: "custom tool history", input: `[{"type":"custom_tool_call","name":"patch","input":"private-history-marker"}]`, reject: true},
		{name: "tool search history", input: `[{"type":"tool_search_output","tools":[]}]`, reject: true, historyType: "tool_search_output"},
		// 未知 type 也属于不可信输入，诊断不能把它原样写到日志。
		{name: "unknown history", input: `[{"type":"private-history-marker"}]`, reject: true, historyType: "unknown"},
		{name: "previous response", extra: `,"previous_response_id":"resp_old"`, input: `"continue"`, reject: true},
		{name: "conversation", extra: `,"conversation":"conv_old"`, input: `"continue"`, reject: true},
		{name: "encrypted reasoning", input: `[{"type":"reasoning","encrypted_content":"opaque"},{"role":"user","content":"continue"}]`, reject: true},
		{name: "compaction", input: `[{"type":"compaction","encrypted_content":"opaque"},{"role":"user","content":"continue"}]`, reject: true},
		// 无状态工具回合依赖完整 call_id 与 output，不能误判成跨厂商旧会话。
		{name: "complete tool turn", input: toolTurn},
		// 已有 Compatible 提升逻辑不能被 OpenCode 的历史白名单挡住。
		{name: "dynamic function tools", input: dynamicTurn, dynamic: true},
		// 白名单选择也必须在工具提升之后执行，否则声明存在但解析不到目标。
		{name: "dynamic allowed tools", input: dynamicTurn, extra: `,"tool_choice":{"type":"allowed_tools","mode":"required","tools":[{"type":"function","name":"read_file"}]}`, dynamic: true},
		// 动态声明不是绕过原有工具能力校验的入口。
		{name: "dynamic custom tool", input: `[{"type":"additional_tools","tools":[{"type":"custom","name":"patch"}]}]`, reject: true},
		{name: "dynamic hosted namespace", input: `[{"type":"additional_tools","tools":[{"type":"namespace","name":"files","tools":[{"type":"web_search"}]}]}]`, reject: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// 零调用是本地拒绝的可观察证据，而非仅检查错误文案。
			// 成功回合则必须恰好调用一次，不能先失败再偷偷回退。
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				// 对应函数输出应成为 tool 消息，顺序保持在 assistant 调用后。
				// 数组形式的文本输出沿用已有 Compatible 展开规则。
				calls++
				wire, _ := io.ReadAll(r.Body)
				// 保留调用身份和结果文本，目标仍是本轮固定的 Kimi。
				if gjson.GetBytes(wire, "messages.1.tool_calls.0.id").String() != "call_1" || gjson.GetBytes(wire, "messages.2.tool_call_id").String() != "call_1" || gjson.GetBytes(wire, "messages.2.content").String() != "file contents" {
					t.Errorf("tool continuation changed: %s", wire)
				}
				// 同时断言定义与历史均到达上游，不能只检查 HTTP 成功。
				if tc.dynamic && (gjson.GetBytes(wire, "tools.0.function.name").String() != "read_file" || gjson.GetBytes(wire, "tools.0.function.parameters.properties.path.type").String() != "string") {
					t.Errorf("dynamic tool definition lost: %s", wire)
				}
				w.Header().Set("Content-Type", "application/json")
				io.WriteString(w, openAIChatFinalResponse)
			}))
			defer server.Close()
			manager, target := newOpenCodeManagerForTest(t, channel.OpenCodeGo, server.URL+"/zen/go")
			// New API 已改名时，gpt-load 收到的客户端名称与目标一致，也必须转换。
			// 同目标的工具结果回传不属于运行中更换供应商，仍是支持范围。
			// 这里特意令两种模型名相同，防止错误地只在名称不同时适配。
			// 回归不依赖 New API 私有请求头，渠道映射也能复用此入口。
			body := []byte(`{"model":"kimi-k3","store":false,"input":` + tc.input + tc.extra + `}`)
			spec := utilitySpec(channel.OpenCodeGo, protocol.OpenAIResponses, execution.OperationResponsesCreate, http.MethodPost, "/v1/responses", body)
			spec.TargetConfig, spec.ClientModel, spec.UpstreamModel = target.TargetConfig, "kimi-k3", "kimi-k3"
			result := manager.Execute(t.Context(), freezeTestAttempt(spec))
			// 检查稳定错误码与派发状态，避免坏历史被当作厂商暂时故障。
			if tc.reject {
				// 禁止先向供应商发送、再把状态丢失解释成可重试错误。
				if result.Error == nil || result.Error.Code != execution.ErrorCodeCriticalSemanticLoss || result.DispatchState != execution.DispatchNotSent || calls != 0 {
					t.Fatalf("state not rejected before sending: %+v, calls=%d", result.Error, calls)
				}
				// 只允许固定类型和数组位置进入诊断，完整正文仍不记录。
				if tc.historyType != "" && (!strings.Contains(result.Error.Summary, "input[0] type "+tc.historyType) || strings.Contains(result.Error.Summary, "private-history-marker")) {
					t.Errorf("unsafe or unhelpful history error: %s", result.Error.Summary)
				}
				if tc.name == "custom tool history" && result.Error.Summary != "custom history requires a supported exec declaration" {
					t.Errorf("unexpected undeclared custom history error: %s", result.Error.Summary)
				}
			} else if result.Error != nil || calls != 1 || result.StatusCode != 200 {
				t.Fatalf("tool continuation failed: %+v, calls=%d", result.Error, calls)
			}
		})
	}
}

// 原生 GPT 必须保留专属参数；转换不能以删除所有模型的 reasoning 作为修复。
// 与转换目标使用同一渠道和执行器，验证规则边界而不是另一套模拟实现。
// 模型选择为 GPT 时，不应改成 Chat 或删除其原生推理控制。
// 成功响应允许空输出，这是 Responses 协议的有效结构。
// 此处不验证模型效果，只验证路由边界与原样转发合同。
// 对未知模型不作能力猜测的约束由同一注册表实现。
func TestOpenCodeGPTKeepsNativeReasoning(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		wire, _ := io.ReadAll(r.Body)
		if r.URL.Path != "/zen/go/v1/responses" || gjson.GetBytes(wire, "reasoning.context").String() != "all_turns" || gjson.GetBytes(wire, "reasoning.effort").String() != "high" {
			t.Errorf("GPT native request changed: %s %s", r.URL.Path, wire)
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"resp_1","object":"response","status":"completed","model":"gpt-6-luna","output":[],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`)
	}))
	defer server.Close()
	manager, target := newOpenCodeManagerForTest(t, channel.OpenCodeGo, server.URL+"/zen/go")
	spec := utilitySpec(channel.OpenCodeGo, protocol.OpenAIResponses, execution.OperationResponsesCreate, http.MethodPost, "/v1/responses", []byte(`{"model":"gpt-6-luna","input":"hello","store":false,"reasoning":{"effort":"high","context":"all_turns"}}`))
	// 源与目标相同是普通调用的常见形状，必须保持既有行为。
	// 客户端能力和实际模型一致时，无需兼容降级。
	spec.TargetConfig, spec.ClientModel, spec.UpstreamModel = target.TargetConfig, "gpt-6-luna", "gpt-6-luna"
	result := manager.Execute(t.Context(), freezeTestAttempt(spec))
	if result.Error != nil || result.StatusCode != 200 || result.UpstreamProtocol != protocol.OpenAIResponses {
		t.Fatalf("native request failed: %+v", result)
	}
}

// 覆盖真实入口的认证、别名解析、路由冻结和日志，防止执行器测试绕过错误的调度。
// 测试日志只断言协议与目标，不依赖上游返回的模型字段推断计费身份。
// 此测试运行完整 Gin 路由，认证、状态快照和执行转发器都是真实实现。
// 只把供应商网络替换成本机 HTTP 服务，不依赖外部余额或可用性。
// 已有 state.Manager.Publish 会重新编译路由，而非测试直接修改执行对象。
// 因此这条用例也验证配置改变后下一次请求采用新模型的行为。
// 同时检查流式工具输出，覆盖执行器与网关写出逻辑之间的连接。
func TestOpenCodeMappingGatewaySelectsConvertedRoute(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%t", stream), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				wire, _ := io.ReadAll(r.Body)
				if r.URL.Path != "/team-a/v1/chat/completions" || gjson.GetBytes(wire, "model").String() != "glm-5.3" || gjson.GetBytes(wire, "reasoning.context").Exists() {
					t.Errorf("wrong mapped target: %s %s", r.URL.Path, wire)
				}
				if stream {
					w.Header().Set("Content-Type", "text/event-stream")
					io.WriteString(w, compatibleToolStream(true, true, false))
				} else {
					w.Header().Set("Content-Type", "application/json")
					io.WriteString(w, openAIChatFinalResponse)
				}
			}))
			defer server.Close()
			engine, manager, input, sink, _ := newAggregationSearchGateway(t, channel.OpenCodeGo, server.URL+"/team-a")
			// 复用隔离的组配置 fixture，再发布本用例自己的模型别名。
			// 模型目录不是装饰数据：必须参与实际候选路由的编译。
			input.Groups[0].Models = []state.ModelConfig{{ID: "glm-5.3", Alias: "gpt-6-luna"}}
			// 不依赖管理员参数覆盖，让测试只观察自动协议转换。
			input.Groups[0].Settings = nil
			if _, err := manager.Publish(input); err != nil {
				t.Fatal(err)
			}
			body := fmt.Sprintf(`{"model":"gpt-6-luna","input":"start task","store":false,"stream":%t,"reasoning":{"context":"all_turns","effort":"high"}}`, stream)
			request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
			// 使用 fixture 的受限测试 Key，不能绕过入口直接调用处理函数。
			request.Header.Set("Authorization", "Bearer gl-search-client")
			request.Header.Set("Content-Type", "application/json")
			response := httptest.NewRecorder()
			engine.ServeHTTP(response, request)
			if response.Code != 200 {
				t.Fatalf("gateway failed: %d %s", response.Code, response.Body.String())
			}
			// 流式必须完整恢复并行函数调用及末尾用量，不能仅返回一个成功 HTTP 头。
			if stream && (!strings.Contains(response.Body.String(), "response.completed") || !strings.Contains(response.Body.String(), "function_call") || !strings.Contains(response.Body.String(), "call_1")) {
				t.Errorf("tool stream lost: %s", response.Body.String())
			}
			// 单次请求只形成一条审计事件，不把协议转换伪造成第二次调用。
			// 实际协议来自执行结果，不能仍按入口路径误标为原生 Responses。
			if len(sink.events) != 1 {
				t.Fatalf("log count=%d", len(sink.events))
			}
			event := sink.events[0]
			if len(event.Attempts) != 1 || event.Attempts[0].RouteMode != channel.RouteConverted || event.Attempts[0].UpstreamProtocol != protocol.OpenAICompletions {
				t.Errorf("conversion not visible in logs: %+v", event.Attempts)
			}
		})
	}
}

// 探测返回也必须符合所选客户端协议，否则管理页会把可用模型判成失败。
// 探测与用户请求复用模型规则，但自身没有用户输入历史。
// 模拟上游返回 Chat，探测校验看到的必须已转换回 Responses。
// 这可捕获“改了请求端点，却忘记改探测结果”的独立回归。
func TestOpenCodeChatModelResponsesProbe(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		wire, _ := io.ReadAll(r.Body)
		// 小预算探测仍请求 Chat，不借用真实用户任务来验证连接。
		if r.URL.Path != "/v1/chat/completions" || !gjson.GetBytes(wire, "messages").IsArray() {
			t.Errorf("invalid probe: %s %s", r.URL.Path, wire)
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, openAIChatFinalResponse)
	}))
	defer server.Close()
	manager, target := newOpenCodeManagerForTest(t, channel.OpenCodeGo, server.URL)
	// 探测不携带 method/path/body；它们由执行器按操作合同生成。
	// client_model 仍用于标识所测模型，不能省略成无模型请求。
	spec := utilitySpec(channel.OpenCodeGo, protocol.OpenAIResponses, execution.OperationProbe, "", "", nil)
	spec.TargetConfig, spec.ClientModel, spec.UpstreamModel = target.TargetConfig, "glm-5.3", "glm-5.3"
	result := manager.Execute(t.Context(), freezeTestAttempt(spec))
	if result.Error != nil || result.StatusCode != 200 || result.UpstreamProtocol != protocol.OpenAICompletions || gjson.GetBytes(result.Body, "object").String() != "response" {
		t.Fatalf("invalid converted probe: %+v", result.Error)
	}
}
