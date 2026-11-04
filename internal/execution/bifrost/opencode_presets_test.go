package bifrost

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
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

// TestOpenCodeSessionHeader 验证实际 HTTP 出站头，覆盖普通和流式转发。
// 缺失标识的请求必须能发送，已有标识与原请求对象必须保持不变。
// 使用两个固定的请求 UUID 表达独立请求，不依赖随机碰撞概率。
// 同一请求不同尝试共享标识，不能把 AttemptID 当成会话身份。
// 这些值属于合成 fixture，与真实用户 UUID 或凭据无关。
// 会话不承担鉴权，测试仍使用执行器原有凭据快照。
// 每次网络回包的内容固定，差异只来自被测的请求头规则。
// 不模拟供应商的私有缓存，也不声称缓存命中率得到验证。
func TestOpenCodeSessionHeader(t *testing.T) {
	const firstID = "11111111-1111-4111-8111-111111111111"
	const nextID = "22222222-2222-4222-8222-222222222222"
	for _, id := range []channel.ID{channel.OpenCodeGo, channel.OpenCodeZen} {
		t.Run(string(id), func(t *testing.T) {
			// 用通道捕获服务器实际收到的值，避免读取异步共享变量。
			// 请求正文包含 stream，但不需要更改正文来补齐会话头。
			// 响应始终提供终态，用来识别仅连通却没有完成流的回归。
			// 服务端在响应前捕获头，串行执行保证每次只消费自己的结果。
			// 不读取源 spec 的生成结果作为期望，避免实现自证正确。
			received := make(chan string, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				received <- r.Header.Get("X-Opencode-Session")
				body, err := io.ReadAll(r.Body)
				if err != nil {
					t.Error(err)
				}
				if gjson.GetBytes(body, "stream").Bool() {
					w.Header().Set("Content-Type", "text/event-stream")
					io.WriteString(w, "data: {\"id\":\"chat_1\",\"object\":\"chat.completion.chunk\",\"model\":\"gemini-4-argon\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
					return
				}
				w.Header().Set("Content-Type", "application/json")
				io.WriteString(w, `{"id":"chat_1","object":"chat.completion","model":"gemini-4-argon","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)
			}))
			defer server.Close()
			manager, target := newOpenCodeManagerForTest(t, id, server.URL)
			// 正常网关在入口生成 RequestID；这里仅替换网络供应商。
			// 相同 fixture 同时覆盖已有头、原生别名、空值和重试。
			// 执行后再次比较源头，确保候选切换不会继承前次注入。
			// 两个渠道复用同一合同，但分别解析自己的默认提供商配置。
			// 供应商 URL 被替换为回环地址，整个矩阵不会访问外网。
			for _, test := range []struct {
				name, header, value, request, attempt, want string
			}{
				// 缺头复现 Gemini 原始失败；重试只改变尝试编号。
				// 独立请求使用另一个网关 UUID，不能沿用前一会话。
				{"missing", "", "", firstID, "attempt-1", firstID},
				{"retry", "", "", firstID, "attempt-2", firstID},
				{"independent", "", "", nextID, "attempt-1", nextID},
				// 非空显式值保留，空白值视为缺失而自动补齐。
				// 这也覆盖管理员把目标头留空的出站配置。
				{"explicit", "X-Opencode-Session", "original-session", firstID, "attempt-1", "original-session"},
				{"blank", "X-Opencode-Session", "  ", firstID, "attempt-1", firstID},
				// Codex 两种拼写都见于现有客户端合同。
				// 兼容原生线程头时不能改变其他头或原来的会话值。
				{"codex", "Session-Id", "codex-session", firstID, "attempt-1", "codex-session"},
				{"codex-alias", "Session_id", "codex-session", firstID, "attempt-1", "codex-session"},
				{"thread", "Thread-Id", "thread-session", firstID, "attempt-1", "thread-session"},
				{"claude", "X-Claude-Code-Session-Id", "claude-session", firstID, "attempt-1", "claude-session"},
			} {
				for _, stream := range []bool{false, true} {
					t.Run(fmt.Sprintf("%s/stream=%t", test.name, stream), func(t *testing.T) {
						spec := utilitySpec(id, protocol.OpenAICompletions, execution.OperationChatCompletion, http.MethodPost, "/v1/chat/completions", []byte(`{"model":"gemini-4-argon","messages":[{"role":"user","content":"hello"}]}`))
						spec.TargetConfig, spec.ClientModel, spec.UpstreamModel = target.TargetConfig, "gemini-4-argon", "gemini-4-argon"
						spec.RequestID, spec.AttemptID = test.request, test.attempt
						if test.header != "" {
							spec.Header.Set(test.header, test.value)
						}
						// Header.Set 使用真实 HTTP 的规范化规则，覆盖大小写无关的取值。
						// 对 nil/空白的边界在以下用例和独立边界测试中分别验证。
						original := spec.Header.Clone()
						// Chat 模式故意不在输入手工补 session，补齐必须来自真实执行器。
						// 模型使用实际报错型号，避免只验证 GPT 特例。
						// 按真实路由冻结合同，不能跳过执行器校验来证明 helper 正确。
						// 流式除了状态码还要求读到终态，避免缺头失败被当作空流成功。
						spec = freezeTestAttempt(spec)
						if stream {
							// 数据事件透传到消费者后，才能检查完整 SSE 终态。
							// 上游错误不得因为 HTTP 已建立就误算为成功。
							var output strings.Builder
							result := manager.ExecuteStream(t.Context(), spec, func(e execution.StreamEvent) error { output.Write(e.Data); return nil })
							if result.Error != nil {
								t.Fatalf("upstream failed: %+v", result.Error)
							}
							if result.StatusCode != http.StatusOK {
								t.Fatalf("HTTP status = %d", result.StatusCode)
							}
							if !strings.Contains(output.String(), "[DONE]") {
								t.Fatal("stream terminal missing")
							}
						} else {
							// 普通请求走与生产 Gemini 相同的原生 Chat 发送路径。
							// 这里只隔离供应商响应，不替换请求头处理代码。
							result := manager.Execute(t.Context(), spec)
							if result.Error != nil {
								t.Fatalf("upstream failed: %+v", result.Error)
							}
							if result.StatusCode != http.StatusOK {
								t.Fatalf("HTTP status = %d", result.StatusCode)
							}
						}
						if got := <-received; got != test.want {
							t.Errorf("session = %q, want %q", got, test.want)
						}
						// 同一个冻结 spec 还可能用于其他候选，不能原地写入生成值。
						// 源头不变才能保证非 OpenCode 候选不继承这个专用头。
						if !reflect.DeepEqual(original, spec.Header) {
							t.Error("source request headers changed")
						}
					})
				}
			}
		})
	}
}

// TestOpenCodeSessionBoundary 验证非目标渠道和失败输入的边界。
// 固定缺失请求 ID 不能生成所有用户共用的会话。
// 已有会话不依赖遥测 ID，因此仍按原有规则透传。
// 此处只检测不该发请求的情况，成功发送的行为由 HTTP 矩阵验证。
// 不启动额外服务器来重复验证相同的失败合同。
func TestOpenCodeSessionBoundary(t *testing.T) {
	spec := execution.AttemptSpec{ChannelID: string(channel.OpenCodeGo), Operation: execution.OperationChatCompletion, RequestID: "untracked"}
	_, err := withOpenCodeSession(spec)
	// 网关熵源异常时会用 untracked 继续遥测，此值绝不能充当会话。
	// 失败路径不发送供应商请求，也不尝试悄悄改为共享身份。
	if err == nil {
		t.Fatal("missing request identity must fail")
	}
	// 显式值的优先级高于原生头，不改变管理员或客户端选择。
	// 无请求 UUID 时仍可使用它，但不能把空值当成已设置。
	spec.Header = http.Header{"X-Opencode-Session": {"explicit"}, "Session-Id": {"native"}}
	got, err := withOpenCodeSession(spec)
	// 已有会话足以满足上游要求，无需再依赖遥测生成成功。
	// 原生别名存在也不能覆盖显式值。
	if err != nil {
		t.Fatal(err)
	}
	if got.Header.Get("X-Opencode-Session") != "explicit" {
		t.Fatal("explicit session changed")
	}
	// 模型发现不是生成请求，不能因遥测缺失影响管理查询。
	// 其他供应商同样不受 OpenCode 的特殊要求影响。
	spec.Header = nil
	// nil 头应保持 nil，说明排除路径确实没有分配并注入专用字段。
	// 不能用空 map 与 nil 的等价判断掩盖多余的出站处理。
	spec.Operation = execution.OperationListModels
	got, err = withOpenCodeSession(spec)
	if err != nil {
		t.Fatal(err)
	}
	if got.Header != nil {
		t.Fatal("unrelated operation received session header")
	}
	spec.Operation, spec.ChannelID = execution.OperationChatCompletion, string(channel.OpenAICompatible)
	// 排除普通兼容渠道，避免把供应商专用头扩散到任意上游。
	// 该对照使用相同无身份输入，能识别判断顺序错误。
	got, err = withOpenCodeSession(spec)
	if err != nil {
		t.Fatal(err)
	}
	if got.Header != nil {
		t.Fatal("unrelated operation received session header")
	}
}

// TestOpenCodeSessionLive 显式启用时用真实上游验证自动补头。
// 默认跳过，不在日常测试中消耗模型额度或读取任何生产凭据。
// 密钥只由调用进程环境提供，不落盘、不打印请求头或完整回复。
// URL 可以是受控 SSH 回环隧道，测试正文固定为无业务信息的问候。
// 联调与日常回归分开启用，真实执行结果不能由模拟响应替代。
// 这里只验证 Gemini Chat；不把它外推为所有模型或工具链均兼容。
func TestOpenCodeSessionLive(t *testing.T) {
	endpoint, key := os.Getenv("OAK_SESSION_LIVE_URL"), os.Getenv("OAK_SESSION_LIVE_KEY")
	if endpoint == "" || key == "" {
		t.Skip("requires explicit live endpoint and credential")
	}
	// 运行标识须显式提供且每次更换，不能复用固定测试会话。
	// 有端点却漏配标识时直接失败，不发送字面量默认值。
	runID := os.Getenv("OAK_SESSION_LIVE_REQUEST_ID")
	if strings.TrimSpace(runID) == "" {
		t.Fatal("live request identity is required")
	}
	manager, target := newOpenCodeManagerForTest(t, channel.OpenCodeGo, endpoint)
	// 使用现有凭据快照接口，认证仍由正常发送器接入。
	// 此 JSON 不包含用户正文，也不被测试断言打印。
	credential, err := json.Marshal(map[string]string{"api_key": key})
	if err != nil {
		t.Fatal(err)
	}
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%t", stream), func(t *testing.T) {
			// 不传 x-opencode-session 或任何原生会话别名。
			// RequestID 使用本次随机 UUID 加模式后缀；两个请求分别独立。
			// 运行标识由测试进程提供，防止不同联调复用固定 UUID。
			spec := utilitySpec(channel.OpenCodeGo, protocol.OpenAICompletions, execution.OperationChatCompletion, http.MethodPost, "/v1/chat/completions", []byte(`{"model":"gemini-4-argon","messages":[{"role":"user","content":"Reply only OK."}]}`))
			spec.RequestID = runID + fmt.Sprintf("-%t", stream)
			spec.TargetConfig, spec.ClientModel, spec.UpstreamModel = target.TargetConfig, "gemini-4-argon", "gemini-4-argon"
			spec.Credential = execution.NewCredentialSnapshot(10, 1, 1, credential)
			spec = freezeTestAttempt(spec)
			// 只检查协议完成与非空回答，不验证模型语义或输出原文。
			// 普通与流式必须各自成功，不能用前者替代后者验收。
			// 不把模型回复直接写进测试日志，失败只显示状态和脱敏错误。
			// 即使返回 200，缺少完成标记或正文仍判定为联调失败。
			if stream {
				var output strings.Builder
				result := manager.ExecuteStream(t.Context(), spec, func(e execution.StreamEvent) error { output.Write(e.Data); return nil })
				if result.Error != nil || result.StatusCode != 200 || !strings.Contains(output.String(), "[DONE]") || !strings.Contains(output.String(), "\"content\"") {
					t.Fatalf("live stream failed: status=%d error=%+v", result.StatusCode, result.Error)
				}
				return
			}
			result := manager.Execute(t.Context(), spec)
			if result.Error != nil || result.StatusCode != 200 || gjson.GetBytes(result.Body, "choices.0.finish_reason").String() != "stop" || gjson.GetBytes(result.Body, "choices.0.message.content").String() == "" {
				t.Fatalf("live completion failed: status=%d error=%+v", result.StatusCode, result.Error)
			}
		})
	}
}

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
						// 探测不依赖管理员手动填写会话头，仍要经过自动补齐。
						if mode == "probe" && r.Header.Get("X-Opencode-Session") != "utility-request" {
							t.Error("probe session header missing")
						}
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
						spec.Header.Del("X-Opencode-Session") // 探测没有客户端会话，复现缺头入口。
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

// 验证完整网关而非转换 helper：客户端别名最终必须命中 Responses。
// 合成上游只检查协议及字段保真，不声称真实厂商已实现这些工具。
// 搜索、动态声明、未知 grammar 和完整历史都由上游判断能力。
// 普通响应与流式响应共享样本，避免只验证 HTTP 成功状态。
// 两个渠道及原先命中的模型家族均覆盖，GPT 作为隔离对照。
// 测试不依赖用户账号或外网，所有任务文本与引用都是合成数据。
// 不校验模型回答质量；这里的验收对象是 Oak 的传输合同。
// 请求仅允许模型别名及已验证的 context 兼容，其他变化均需暴露。
// 原生工具无需在本地登记新类型，未知声明也必须送达供应商。
func TestOpenCodeMappedResponsesNativeTools(t *testing.T) {
	// 搜索调用及来源必须保留，不能将其压成普通回答文本。
	// custom 的输入按自由文本透传，不能包装为 function 的 JSON 参数。
	// 同一输出将用作下一轮历史，检查请求和响应双向保真。
	const output = `[{"type":"web_search_call","id":"ws_1","status":"completed","action":{"type":"search","query":"example","sources":[{"type":"url","url":"https://example.com"}]}},{"type":"custom_tool_call","id":"ct_1","call_id":"call_1","name":"exec","namespace":"functions","input":"text(1)"},{"type":"message","id":"msg_1","role":"assistant","status":"completed","content":[{"type":"output_text","text":"example","annotations":[{"type":"url_citation","start_index":0,"end_index":7,"url":"https://example.com","title":"Example"}]}]}]`
	// 中间搜索状态与自由文本增量属于原生事件，不能只保留最终快照。
	// 逐字比较该段合成 SSE，可检测事件名、输入文本或输出索引被改写。
	const toolEvents = "event: response.web_search_call.searching\ndata: {\"type\":\"response.web_search_call.searching\",\"item_id\":\"ws_1\",\"output_index\":0}\n\nevent: response.custom_tool_call_input.delta\ndata: {\"type\":\"response.custom_tool_call_input.delta\",\"item_id\":\"ct_1\",\"output_index\":1,\"delta\":\"text(1)\"}\n\n"
	for _, id := range []channel.ID{channel.OpenCodeGo, channel.OpenCodeZen} {
		for _, model := range []string{"kimi-k3", "glm-5.3", "deepseek-v4.1-flash", "deepseek-v4-pro", "gpt-6-luna"} {
			for _, stream := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/stream=%t", id, model, stream), func(t *testing.T) {
					// 记录真正发出的请求，不能拿本地请求草稿替代网络边界验证。
					// 请求串行执行，捕获值在每轮完成后读取，不共享跨用例状态。
					var received []byte
					var session string
					calls := 0
					// 返回的模型身份也需要按既有别名机制恢复。
					// usage 使用非零整数，能识别被本地重建成零用量的回归。
					responseBody := fmt.Sprintf(`{"id":"resp_1","object":"response","status":"completed","model":%q,"output":%s,"usage":{"input_tokens":12,"output_tokens":3,"total_tokens":15}}`, model, output)
					server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						calls++
						// 若仍走 Chat，则直接失败；禁止以假 Chat 回包掩盖选路错误。
						if r.URL.Path != "/team-a/v1/responses" {
							t.Errorf("unexpected protocol path: %s", r.URL.Path)
							w.WriteHeader(http.StatusBadRequest)
							return
						}
						// 读取失败必须使测试失败，不能用空正文继续比较。
						var readErr error
						received, readErr = io.ReadAll(r.Body)
						if readErr != nil {
							t.Error(readErr)
							return
						}
						// 模拟现场的拒绝条件，防止只比较草稿而漏掉实际发送内容。
						// DeepSeek 和 GPT 不套用 Kimi/GLM 的参数能力。
						if (model == "kimi-k3" || model == "glm-5.3") && gjson.GetBytes(received, "reasoning.context").Exists() {
							w.WriteHeader(http.StatusBadRequest)
							io.WriteString(w, `{"error":{"message":"reasoning.context: this reasoning option requires a native Responses target"}}`)
							return
						}
						session = r.Header.Get("Session-Id")
						// 客户端凭据不能覆盖渠道凭据，会话头则应完整保留。
						if r.Header.Get("Authorization") != "Bearer "+testAPIKey {
							t.Error("upstream credential not applied")
						}
						if stream {
							// 原始完成事件与最终快照都要到达客户端，不能重建 custom 事件。
							// 故意只发两个工具 done，快照中的文本不应触发额外工具完成事件。
							// 不使用 Chat 的 [DONE]，避免测试误把 Chat 结束规则当作 Responses 合同。
							w.Header().Set("Content-Type", "text/event-stream")
							io.WriteString(w, toolEvents)
							fmt.Fprintf(w, "event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":%s}\n\n", gjson.Get(output, "0").Raw)
							fmt.Fprintf(w, "event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"output_index\":1,\"item\":%s}\n\n", gjson.Get(output, "1").Raw)
							fmt.Fprintf(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":%s}\n\n", responseBody)
						} else {
							w.Header().Set("Content-Type", "application/json")
							io.WriteString(w, responseBody)
						}
					}))
					defer server.Close()
					// 使用真实鉴权、模型映射、冻结路由及执行日志链路。
					// 本机 HTTP 上游由真实执行器访问，模型路由没有被 mock。
					engine, manager, input, sink, _ := newAggregationSearchGateway(t, id, server.URL+"/team-a")
					input.Groups[0].Models = []state.ModelConfig{{ID: model, Alias: "public"}}
					// 不添加管理员参数覆盖，兼容规则必须随最终目标自动生效。
					input.Groups[0].Settings = nil
					if _, err := manager.Publish(input); err != nil {
						t.Fatal(err)
					}
					// 第一轮声明能力，第二轮携带原生搜索/custom 历史继续任务。
					// grammar 使用合成的未知文法，证明不再存在本地 exec 文法白名单。
					// additional_tools 应留在 input，不能被 Chat 准备阶段提升或删掉。
					// 搜索输出和 custom 输出并存，可同时覆盖两种原先拒绝的历史项。
					// call_id 必须关联上轮工具调用，不能替换成响应项的 id。
					for turn := 0; turn < 2; turn++ {
						history := `[{"type":"additional_tools","tools":[{"type":"function","name":"lookup","parameters":{"type":"object"}}]},{"role":"user","content":"synthetic task"}]`
						if turn == 1 {
							history = strings.TrimSuffix(output, "]") + `,{"type":"custom_tool_call_output","call_id":"call_1","output":"synthetic result"}]`
						}
						// 仅已验证不支持的目标去掉 context；完整输入历史仍必须保留。
						// include 是输出偏好，不能据此删掉工具或把请求改成其他协议。
						// store=false 保留现有无状态合同，不靠改变存储选项触发上游回退。
						body := fmt.Sprintf(`{"model":"public","stream":%t,"store":false,"input":%s,"reasoning":{"context":"all_turns","effort":"high"},"include":["reasoning.encrypted_content"],"tools":[{"type":"web_search","external_web_access":true},{"type":"namespace","name":"functions","tools":[{"type":"custom","name":"exec","format":{"type":"grammar","syntax":"lark","definition":"start: /.+/"}}]},{"type":"tool_search","execution":"client","parameters":{"type":"object"}}],"tool_choice":"auto"}`, stream, history)
						request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
						request.Header.Set("Authorization", "Bearer gl-search-client")
						request.Header.Set("Content-Type", "application/json")
						request.Header.Set("Session-Id", "synthetic-conversation")
						response := httptest.NewRecorder()
						engine.ServeHTTP(response, request)
						if response.Code != http.StatusOK || calls != turn+1 || session != "synthetic-conversation" {
							t.Fatalf("native request failed: status=%d calls=%d body=%s", response.Code, calls, response.Body.String())
						}
						// 整体比较请求，防止只检查 tool 名而忽略参数或完整历史被丢弃。
						// JSON 对象键顺序不属于合同，因此比较解码后的值而非序列化顺序。
						// 数组顺序属于合同，DeepEqual 会检查工具及历史项的次序。
						var want, got map[string]any
						if err := json.Unmarshal([]byte(body), &want); err != nil {
							t.Fatal(err)
						}
						if err := json.Unmarshal(received, &got); err != nil {
							t.Fatal(err)
						}
						want["model"] = model
						// 这是预期的供应商参数兼容，不改变请求工具或响应协议。
						// 未知目标继续透传，不能把个别模型的限制推广到所有模型。
						if model == "kimi-k3" || model == "glm-5.3" {
							delete(want["reasoning"].(map[string]any), "context")
						}
						if !reflect.DeepEqual(got, want) {
							t.Fatalf("native payload changed: %s", received)
						}
						// 流式检查最终快照及每个完成事件，避免重复发布工具导致重复执行。
						// 没有最终事件时保留空值，后续解析会失败而非借用模拟上游快照。
						final := response.Body.String()
						if stream {
							if !strings.Contains(response.Body.String(), toolEvents) {
								t.Fatal("native tool stream events changed")
							}
							final = ""
							for _, line := range strings.Split(response.Body.String(), "\n") {
								if strings.HasPrefix(line, "data: ") && gjson.Get(strings.TrimPrefix(line, "data: "), "type").String() == "response.completed" {
									final = gjson.Get(strings.TrimPrefix(line, "data: "), "response").Raw
								}
							}
							if strings.Count(response.Body.String(), "event: response.output_item.done\n") != 2 {
								t.Fatal("tool done events changed")
							}
						}
						// 返回模型仍为用户别名，Token 用量与引用则沿用上游证据。
						// annotations 的 URL、标题和文本范围必须一起保留，不能只保留正文。
						// 比较整个 output 也检查 namespace、自由文本及搜索 action 的结构。
						if gjson.Get(final, "model").String() != "public" || gjson.Get(final, "usage.total_tokens").Int() != 15 {
							t.Fatalf("response identity or usage lost: %s", final)
						}
						var wantOutput, gotOutput any
						if err := json.Unmarshal([]byte(output), &wantOutput); err != nil {
							t.Fatal(err)
						}
						if err := json.Unmarshal([]byte(gjson.Get(final, "output").Raw), &gotOutput); err != nil {
							t.Fatal(err)
						}
						if !reflect.DeepEqual(wantOutput, gotOutput) {
							t.Fatalf("native output changed: %s", final)
						}
						// 审计必须反映真实上游协议，不能继续标注为 Chat 转换。
						// 两轮应形成两个独立请求事件，每轮只对应一次供应商执行。
						if len(sink.events) != turn+1 || len(sink.events[turn].Attempts) != 1 {
							t.Fatal("unexpected attempt count")
						}
						attempt := sink.events[turn].Attempts[0]
						if attempt.RouteMode != channel.RouteNative || attempt.UpstreamProtocol != protocol.OpenAIResponses {
							t.Fatalf("unexpected route: %+v", attempt)
						}
					}
				})
			}
		}
	}
}

// 原生路径必须把真实上游错误留给客户端，不能删工具后重试成成功。
// 此测试仅模拟供应商拒绝，不把本地转发通过当成供应商能力承诺。
// 模拟服务返回固定错误，不包含用户输入、账号标识或真实供应商诊断。
// 普通和流式请求都可能在响应头阶段失败，分别检查派发次数。
// 探测单独验证 Responses 路径，避免保留旧的 Chat 探测捷径。
// 自定义入口遵循聚合网关的根地址合同，由执行器追加 /v1/responses。
// 失败样本是参数错误，不依赖任何真实账号或供应商错误内容。
// 这个用例不承诺厂商接受所有工具，只要求失败真实可见。
func TestOpenCodeNativeErrorsAndProbe(t *testing.T) {
	for _, id := range []channel.ID{channel.OpenCodeGo, channel.OpenCodeZen} {
		for _, mode := range []string{"unary", "stream", "probe"} {
			t.Run(string(id)+"/"+mode, func(t *testing.T) {
				calls := 0
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls++
					// 失败后不得转向 Chat；自定义路径前缀也必须保留。
					// 这里的 /openai 代表协议入口前缀，不改变原有 URL 拼接规则。
					if r.URL.Path != "/openai/v1/responses" {
						t.Errorf("unexpected path: %s", r.URL.Path)
					}
					// 网络读取异常应直接暴露，不以缺字段错误掩盖实际失败。
					body, err := io.ReadAll(r.Body)
					if err != nil {
						t.Error(err)
						return
					}
					// 探测仍指定真实目标模型，不借用 GPT 型号替目标宣称成功。
					if gjson.GetBytes(body, "model").String() != "kimi-k3" {
						t.Error("probe/model identity changed")
					}
					w.Header().Set("Content-Type", "application/json")
					if mode == "probe" {
						// 探测输出沿用供应商 Responses 协议，不经过 Chat 反向转换。
						// completed 和 usage 是探测有效性的证据，不能仅凭 HTTP 200 判定。
						io.WriteString(w, `{"id":"probe","object":"response","status":"completed","output":[],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`)
						return
					}
					// 断言搜索声明确实到达上游，失败不是本地工具白名单触发的。
					// 返回标准参数错误，验证通用错误链仍然能解析供应商拒绝。
					if gjson.GetBytes(body, "tools.0.type").String() != "web_search" {
						t.Error("search declaration lost")
					}
					w.WriteHeader(http.StatusBadRequest)
					io.WriteString(w, `{"error":{"type":"invalid_request_error","message":"synthetic unsupported web_search"}}`)
				}))
				defer server.Close()
				manager, target := newOpenCodeManagerForTest(t, id, server.URL+"/openai")
				// 凭据、调度规格和协议模式来自真实运行时，只有供应商网络被隔离。
				// 模型使用旧转换规则命中的 Kimi，避免未知模型本来就原生造成假通过。
				spec := utilitySpec(id, protocol.OpenAIResponses, execution.OperationResponsesCreate, http.MethodPost, "/v1/responses", []byte(`{"model":"kimi-k3","input":"synthetic","store":false,"tools":[{"type":"web_search"}]}`))
				spec.TargetConfig, spec.ClientModel, spec.UpstreamModel = target.TargetConfig, "kimi-k3", "kimi-k3"
				if mode == "probe" {
					// 管理探测没有客户端正文，按操作合同由执行器生成最小请求。
					// 冻结规格之前切换操作，确保测试与真实调度时序一致。
					spec.Operation, spec.Method, spec.Path, spec.Body = execution.OperationProbe, "", "", nil
				}
				spec = freezeTestAttempt(spec)
				var result execution.StreamResult
				var wire strings.Builder
				if mode == "stream" {
					// 流式的失败不应发布 response.completed，更不能生成假搜索结果。
					result = manager.ExecuteStream(t.Context(), spec, func(e execution.StreamEvent) error { wire.Write(e.Data); return nil })
				} else {
					unary := manager.Execute(t.Context(), spec)
					result = execution.StreamResult{StatusCode: unary.StatusCode, Error: unary.Error, UpstreamProtocol: unary.UpstreamProtocol}
				}
				// 每种操作恰好一次调用；原生失败不触发本地协议降级。
				// 错误检查同时要求非空证据，避免仅改 HTTP 状态而吞掉真实失败。
				// 流与非流只合并共有的结果字段，不伪造或解析供应商正文。
				// 保留普通原生协议测试，另行覆盖 GPT、Chat 与 Anthropic 的已有行为。
				// 即使业务结果失败，上游协议也应记录为 Responses 供日志排障。
				if calls != 1 || result.UpstreamProtocol != protocol.OpenAIResponses {
					t.Fatalf("unexpected execution: calls=%d protocol=%s", calls, result.UpstreamProtocol)
				}
				if mode == "probe" {
					if result.Error != nil || result.StatusCode != 200 {
						t.Fatalf("native probe failed: %+v", result.Error)
					}
				} else if result.Error == nil || result.StatusCode != 400 || strings.Contains(wire.String(), "response.completed") {
					t.Fatalf("upstream rejection hidden: status=%d error=%+v", result.StatusCode, result.Error)
				}
			})
		}
	}
}
