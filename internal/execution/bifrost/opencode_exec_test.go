// 这些回归覆盖协议适配，不衡量模型选择工具的准确率。
// 所有工具输入均为合成字符串，测试不会执行其中的代码。
// 每个 HTTP 上游独立创建，避免调用计数与流状态跨用例共享。
// 真实 Runtime、SDK 和网关承担转换，只有供应商网络被替换。
// 因此可以复现字段丢失、流事件错误和用量遗漏，无需真实额度。
// 客户端文法夹具独立于实现常量，防止实现和测试同时放宽约束。
// 通过只表示本地协议合同成立，不代表厂商线上验收。
package bifrost

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
	"gpt-load/internal/channel"
	"gpt-load/internal/execution"
	"gpt-load/internal/protocol"
	"gpt-load/internal/state"
)

// 夹具只保留现场工具的公开语法结构，不包含真实任务、凭据或执行代码。
// SOURCE 的任意非空文本分支是本适配的边界，不能拿一般 Lark 文法替换。
const execGrammarFixture = `
start: pragma_source | plain_source
pragma_source: PRAGMA_LINE NEWLINE SOURCE
plain_source: SOURCE

PRAGMA_LINE: /[ \t]*\/\/ @exec:[^\r\n]*/
NEWLINE: /\r?\n/
SOURCE: /[\s\S]+/
`

// execRequestFixture 生成客户端动态声明；所有回合都经过真实 SDK 和 HTTP。
// 保留命名空间，以覆盖线上 input[0].tools.functions 的具体形状。
// additional_tools 是输入中的声明项，发送 Chat 前应提升为顶层 tools。
// 原请求同时保留普通用户消息，核对提升时没有误删会话内容。
// history 直接追加真实首轮返回，避免重建历史掩盖返回结构问题。
// choice 分别取自动、强制和白名单形式，覆盖不同的选择语义。
// store=false 表明不依赖远端会话，无需查询或迁移私有状态。
func execRequestFixture(t *testing.T, stream bool, history []any, choice any) []byte {
	t.Helper()
	// 工具说明也属于声明，转换时不能只留下函数名而丢弃用途。
	// 夹具保留 format 的三个字段，以区分 grammar 和普通 text 自定义工具。
	tool := map[string]any{"type": "custom", "name": "exec", "description": "Execute the supplied source text.", "format": map[string]any{"type": "grammar", "syntax": "lark", "definition": execGrammarFixture}}
	// 默认 namespace 对客户端可见，返回时需要恢复这个归属信息。
	// Chat 本身不理解这个容器，只能接收其中已转换的普通函数。
	tools := []any{map[string]any{"type": "namespace", "name": "functions", "tools": []any{tool}}}
	input := append([]any{map[string]any{"type": "additional_tools", "tools": tools}, map[string]any{"role": "user", "content": "Use exec"}}, history...)
	body, err := json.Marshal(map[string]any{"model": "kimi-k3", "store": false, "stream": stream, "input": input, "tool_choice": choice})
	if err != nil {
		t.Fatal(err)
	}
	return body
}

// 同一工具先由模型调用、再由客户端提交结果，不能只测声明被放行。
// 首轮上游返回 JSON 参数，客户端必须看到原始自由文本及 custom 类型。
// 次轮复用首轮真实输出，能捕获输出看似正确、历史却无法回放的问题。
// 四个组合覆盖普通/流式与默认命名空间/顶层声明。
// 顶层组合使用内容数组结果，命名空间组合使用字符串结果。
// 两种结果都是合法客户端形状，不能只接受其中一种。
// 每组实际发送两次请求，第二次必须保留首轮调用身份。
// 上游包装与客户端自由文本分别断言，不能只验证状态码。
// 源码含 pragma，但适配器不解释或改变客户端的执行选项。
// 用量检查保护计费证据，反向转换不能使实际调用变成免费。
func TestOpenCodeExecRoundTrip(t *testing.T) {
	for _, tc := range []struct{ stream, bare bool }{{false, false}, {true, false}, {false, true}, {true, true}} {
		stream := tc.stream
		t.Run(fmt.Sprintf("stream=%t/bare=%t", stream, tc.bare), func(t *testing.T) {
			// 引号、反斜杠、换行、Unicode 和可选 pragma 必须逐字保留。
			source := "// @exec: {\"max_output_tokens\": 100}\ntext(\"中文\\路径 🐈\");\n"
			// 期望参数由标准编码器生成，不复制适配器逻辑计算结果。
			// 源码中的引号需要包装转义，返回时应完全解开。
			args, _ := json.Marshal(map[string]string{"input": source})
			// HTTP 次数也是验收条件，转换不应额外派发或重试。
			// 每轮结束后才开始续轮，处理器应看到完整前一轮输出。
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				wire, _ := io.ReadAll(r.Body)
				// 真实目标只接受 Chat，工具约束随声明一起到达上游。
				if r.URL.Path != "/v1/chat/completions" || gjson.GetBytes(wire, "tools.0.function.name").String() != "exec" || gjson.GetBytes(wire, "tools.0.function.parameters.properties.input.minLength").Int() != 1 {
					t.Errorf("invalid converted request: %s", wire)
				}
				// 只在续轮检查历史，不能给首轮人为补出工具结果。
				// 消息索引对应用户消息、助手调用和工具结果的实际顺序。
				if calls == 2 {
					// 历史输入封装回 JSON，但 tool_call_id 和结果文字不能改变。
					if gjson.GetBytes(wire, "messages.1.tool_calls.0.function.arguments").String() != string(args) || gjson.GetBytes(wire, "messages.2.tool_call_id").String() != "call_exec" || gjson.GetBytes(wire, "messages.2.content").String() != "execution result" {
						t.Errorf("invalid continuation: %s", wire)
					}
				}
				// 上游发送标准 Chat SSE，不提前伪装成 Responses 事件。
				// 事件类型和终态必须由真实适配链路生成。
				if stream {
					w.Header().Set("Content-Type", "text/event-stream")
					// 故意分片参数；客户端不能收到 JSON 包装壳或未校验的半条源码。
					for i, part := range []string{string(args[:len(args)/2]), string(args[len(args)/2:])} {
						call := map[string]any{"index": 0, "function": map[string]any{"arguments": part}}
						// 首片有名称和 ID，续片只有索引与参数，模拟常见上游。
						// 恢复不能假设每个分片都重复工具身份。
						if i == 0 {
							call["id"] = "call_exec"
							call["type"] = "function"
							call["function"].(map[string]any)["name"] = "exec"
						}
						// 先编码 chunk 再写 SSE，分片中的特殊字符仍由 JSON 转义。
						// 两个参数片段拼接后才是完整对象，单片都不能直接执行。
						event, _ := json.Marshal(map[string]any{"id": "chat_exec", "model": "kimi-k3", "choices": []any{map[string]any{"index": 0, "delta": map[string]any{"tool_calls": []any{call}}}}})
						fmt.Fprintf(w, "data: %s\n\n", event)
					}
					// finish_reason 表示工具回合结束，usage 是真实消费证据。
					// 正常的 [DONE] 关闭流，不应被编码成新的业务输出项。
					io.WriteString(w, "data: {\"id\":\"chat_exec\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}],\"usage\":{\"prompt_tokens\":9,\"completion_tokens\":3,\"total_tokens\":12}}\n\ndata: [DONE]\n\n")
				} else {
					w.Header().Set("Content-Type", "application/json")
					// 只有工具、没有自然语言回答，仍是合法完成结果。
					// 只读取 message.content 会误丢这里的 tool_calls。
					response, _ := json.Marshal(map[string]any{"id": "chat_exec", "object": "chat.completion", "model": "kimi-k3", "choices": []any{map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{"id": "call_exec", "type": "function", "function": map[string]any{"name": "exec", "arguments": string(args)}}}}, "finish_reason": "tool_calls"}}, "usage": map[string]int{"prompt_tokens": 9, "completion_tokens": 3, "total_tokens": 12}})
					w.Write(response)
				}
			}))
			defer server.Close()
			manager, target := newOpenCodeManagerForTest(t, channel.OpenCodeGo, server.URL)
			// 首轮无远端状态，续轮只采用客户端实际收到的对象。
			// 若返回缺字段，第二轮应暴露问题，不能靠预制历史修补。
			var history []any
			for round := range 2 {
				// 第一轮声明、第二轮完整历史，均采用现场的 auto 工具选择。
				body := execRequestFixture(t, stream, history, "auto")
				// 默认命名空间可以省略，但不应因此额外返回一个虚构容器。
				// 此分支只改变声明位置，工具名与 grammar 仍与现场一致。
				if tc.bare {
					// 无命名空间的声明同样有效；结果数组复用既有内容展开规则。
					body, _ = sjson.SetRawBytes(body, "input.0.tools", []byte(gjson.GetBytes(body, "input.0.tools.0.tools").Raw))
				}
				// 冻结正文在回退间共享，转换只能修改本次解码 DTO。
				// 使用独立副本，防止比较同一底层切片导致假通过。
				original := bytes.Clone(body)
				spec := utilitySpec(channel.OpenCodeGo, protocol.OpenAIResponses, execution.OperationResponsesCreate, http.MethodPost, "/v1/responses", body)
				spec.TargetConfig, spec.ClientModel, spec.UpstreamModel = target.TargetConfig, "kimi-k3", "kimi-k3"
				// 两种传输最终都取客户端可见 output，执行相同合同断言。
				// 不检查桥接器内部状态，以覆盖编码器与执行器的集成。
				var output []byte
				if stream {
					// 流完成之前不把任何中间快照当作最终输出。
					// 若缺少 completed，下面的最终输出检查会失败，而不会拿最后一条 delta 冒充成功。
					var final gjson.Result
					// 累计全部 custom delta，既能发现遗漏也能发现重复。
					// 只检查 done 无法证明流式客户端拼出的源码正确。
					var delta strings.Builder
					result := manager.ExecuteStream(t.Context(), freezeTestAttempt(spec), func(event execution.StreamEvent) error {
						if event.Kind != execution.StreamEventData {
							return nil
						}
						for _, line := range strings.Split(string(event.Data), "\n") {
							if !strings.HasPrefix(line, "data: ") {
								continue
							}
							value := gjson.Parse(strings.TrimPrefix(line, "data: "))
							// 参数事件与终态快照分别验证，终态成功不能掩盖中间泄漏。
							// 包装层的 function arguments 不属于客户端 custom 合同。
							switch value.Get("type").String() {
							case "response.custom_tool_call_input.delta":
								delta.WriteString(value.Get("delta").String())
							case "response.custom_tool_call_input.done":
								if value.Get("input").String() != source {
									t.Error("done input changed")
								}
							case "response.function_call_arguments.delta", "response.function_call_arguments.done":
								t.Error("function wrapper leaked to client")
							case "response.completed":
								final = value.Get("response")
							}
						}
						return nil
					})
					if result.Error != nil {
						t.Fatalf("round %d: %+v", round, result.Error)
					}
					// 用量与文本独立断言，内容正确不代表消费证据仍在。
					// 最终输出必须来自真实结束事件，不能使用测试预置结果。
					if result.Usage == nil || delta.String() != source {
						t.Fatalf("missing usage or custom delta: %q", delta.String())
					}
					output = []byte(final.Get("output.0").Raw)
				} else {
					result := manager.Execute(t.Context(), freezeTestAttempt(spec))
					if result.Error != nil {
						t.Fatalf("round %d: %+v", round, result.Error)
					}
					// 上游明确给出了非零用量，普通响应不能将它转为空证据。
					// 缺失用量与合法零值不同，测试不能以默认零填充后通过。
					if result.Usage == nil {
						t.Fatal("usage lost")
					}
					output = []byte(gjson.GetBytes(result.Body, "output.0").Raw)
				}
				// 使用客户端最终收到的对象验证类型、命名空间、调用身份和源码。
				// namespace 取值来自原始声明，不从上游函数名字反推。
				// 这可发现默认空间解包后遗漏反向恢复的问题。
				namespace := "functions"
				if tc.bare {
					namespace = ""
				}
				if !bytes.Equal(original, body) {
					t.Fatal("frozen caller body changed")
				}
				for key, want := range map[string]string{"type": "custom_tool_call", "name": "exec", "namespace": namespace, "call_id": "call_exec", "input": source} {
					if gjson.GetBytes(output, key).String() != want {
						t.Fatalf("%s mismatch: %s", key, output)
					}
				}
				// custom 同时暴露 arguments 会令客户端选择错误分派路径。
				// 即使 input 正确，也必须清除内部包装字段。
				if gjson.GetBytes(output, "arguments").Exists() {
					t.Fatal("wrapped arguments leaked")
				}
				// 标准解码器把真实响应送回续轮，不补 name 或 namespace。
				// 验证范围因此包含客户端实际可以保存的字段集合。
				var item any
				if err := json.Unmarshal(output, &item); err != nil {
					t.Fatal(err)
				}
				var outputValue any = "execution result"
				// 数组形式结果通过既有内容展开器进入 Chat，适配器不另建结果格式。
				// 文本内容应保持原样，不把整段数组序列化成用户可见的 JSON。
				if tc.bare {
					outputValue = []any{map[string]any{"type": "input_text", "text": "execution result"}}
				}
				history = []any{item, map[string]any{"type": "custom_tool_call_output", "call_id": "call_exec", "output": outputValue}}
			}
			// 每轮一次派发，避免静默重复执行模型工具请求。
			// 同时排除第二轮被本地预置结果替代的假成功。
			if calls != 2 {
				t.Fatalf("expected two actual requests, got %d", calls)
			}
		})
	}
}

// 非法声明与不完整历史必须在发给厂商前失败，不得用删除字段伪造兼容。
// 每项只改一个协议条件，零 HTTP 调用是拒绝边界的实际证据。
// 相似文法不等于语义相同，限制性 grammar 不能放宽为任意字符串。
// 工具名和命名空间共同界定适配对象，不猜测其他 custom 的契约。
// 历史必须具备调用 ID、非空源码及已有调用关联。
// 默认同名普通函数与 custom 冲突需在发送上游前发现。
// 500 上游哨兵用于证明错误确实发生在本地准备阶段。
// 不支持的输入要求明确拒绝且无消费，不要求被强行改写成功。
func TestOpenCodeExecRejectsUnsupportedRequests(t *testing.T) {
	// 从独立夹具提取合法声明，仅修改和拒绝条件相关的字段。
	// 顶层冲突保留合法文法，保证失败来自重名而不是语法差异。
	var custom any
	if err := json.Unmarshal([]byte(gjson.GetBytes(execRequestFixture(t, false, nil, "auto"), "input.0.tools.0.tools.0").Raw), &custom); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, path string
		value      any
	}{
		// 同名 exec 可能声明另一门语言，不能单凭名称放行。
		// syntax 和 definition 分别变化，保护两个独立识别条件。
		{"different grammar", "input.0.tools.0.tools.0.format.definition", `start: "only-this"`},
		{"different syntax", "input.0.tools.0.tools.0.format.syntax", "regex"},
		{"different custom tool", "input.0.tools.0.tools.0.name", "apply_patch"},
		{"foreign namespace", "input.0.tools.0.name", "foreign"},
		// custom 结果必须关联此前的 exec，不能引用不存在或普通函数的 call_id。
		{"orphan result", "input.2", map[string]any{"type": "custom_tool_call_output", "call_id": "orphan", "output": "ok"}},
		{"empty historical source", "input.2", map[string]any{"type": "custom_tool_call", "name": "exec", "call_id": "c", "input": ""}},
		{"missing historical id", "input.2", map[string]any{"type": "custom_tool_call", "name": "exec", "input": "text(1)"}},
		{"unknown forced tool", "tool_choice", map[string]any{"type": "custom", "name": "unknown"}},
		// 同一默认名字不能同时代表普通函数和 custom，返回时无法无歧义恢复。
		// 没有 namespace 容器时也不能跳过冲突检查。
		{"duplicate bare name", "input.0.tools", []any{custom, map[string]any{"type": "function", "name": "exec", "parameters": map[string]any{"type": "object"}}}},
		{"duplicate default name", "tools", []any{map[string]any{"type": "function", "name": "exec", "parameters": map[string]any{"type": "object"}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++; w.WriteHeader(500) }))
			defer server.Close()
			// 每例只变一个协议条件，避免多个错误相互遮蔽。
			// 修改后的原始 JSON 仍经过真实 DTO 解码和执行准备。
			body, err := sjson.SetBytes(execRequestFixture(t, false, nil, "auto"), tc.path, tc.value)
			if err != nil {
				t.Fatal(err)
			}
			manager, target := newOpenCodeManagerForTest(t, channel.OpenCodeGo, server.URL)
			spec := utilitySpec(channel.OpenCodeGo, protocol.OpenAIResponses, execution.OperationResponsesCreate, http.MethodPost, "/v1/responses", body)
			spec.TargetConfig, spec.ClientModel, spec.UpstreamModel = target.TargetConfig, "kimi-k3", "kimi-k3"
			result := manager.Execute(t.Context(), freezeTestAttempt(spec))
			// 诊断、未派发、零 HTTP 三项同时成立才是正确拒绝。
			// 只查错误对象会将上游报错误认成本地保护生效。
			if result.Error == nil || result.DispatchState != execution.DispatchNotSent || calls != 0 {
				t.Fatalf("bad request reached upstream: %+v calls=%d", result.Error, calls)
			}
		})
	}
}

// 上游并不保证遵守 JSON Schema；格式错误不能变成一次可执行的 custom 调用。
// 普通返回和流式结束分开验证，后者尤其不能在错误后继续发送 completed。
// HTTP 成功不等于生成了可执行工具输入。
// 包装缺失、类型不符、空源码分别违背字段合同。
// 额外与重复字段不能经过 map 解码被静默接受。
// 尾随 JSON 与不完整对象不能按有效前缀执行。
// 每种错误都覆盖普通和流式，保证两条路径校验一致。
// 消费后发生的转换错误仍应携带派发和用量证据。
// 哨兵文本检查事件与诊断不会泄漏未校验的源码。
func TestOpenCodeExecRejectsMalformedOutput(t *testing.T) {
	for _, arguments := range []string{`{}`, `{"input":""}`, `{"input":null}`, `{"input":1}`, `{"input":"secret-output","other":true}`, `{"input":"first","input":"second"}`, `{"input":"secret-output"} {}`, `{"input":"unfinished`} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/%t", arguments, stream), func(t *testing.T) {
				calls := 0
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls++
					// 只破坏 arguments，调用 ID 和名字仍然有效。
					// 避免被普通身份检查先行拦截而未触及解包逻辑。
					call := map[string]any{"index": 0, "id": "bad_call", "type": "function", "function": map[string]any{"name": "exec", "arguments": arguments}}
					// 使用正常 finish，失败应来自内容而非缺少终止标记。
					// message 与 delta 分开构造，分别进入两种 SDK 转换路径。
					choice := map[string]any{"index": 0, "finish_reason": "tool_calls"}
					if stream {
						choice["delta"] = map[string]any{"tool_calls": []any{call}}
					} else {
						choice["message"] = map[string]any{"role": "assistant", "tool_calls": []any{call}}
					}
					wire, _ := json.Marshal(map[string]any{"id": "bad_chat", "object": "chat.completion", "model": "kimi-k3", "choices": []any{choice}, "usage": map[string]int{"prompt_tokens": 8, "completion_tokens": 2, "total_tokens": 10}})
					if stream {
						w.Header().Set("Content-Type", "text/event-stream")
						// 模型可能在工具结束后的独立 chunk 才给出用量。
						// 即使参数已校验失败，也必须读到该尾块后再报告转换错误。
						wire, _ = sjson.DeleteBytes(wire, "usage")
						fmt.Fprintf(w, "data: %s\n\ndata: {\"id\":\"bad_chat\",\"choices\":[],\"usage\":{\"prompt_tokens\":8,\"completion_tokens\":2,\"total_tokens\":10}}\n\ndata: [DONE]\n\n", wire)
					} else {
						w.Header().Set("Content-Type", "application/json")
						w.Write(wire)
					}
				}))
				defer server.Close()
				manager, target := newOpenCodeManagerForTest(t, channel.OpenCodeGo, server.URL)
				body := execRequestFixture(t, stream, nil, "auto")
				spec := utilitySpec(channel.OpenCodeGo, protocol.OpenAIResponses, execution.OperationResponsesCreate, http.MethodPost, "/v1/responses", body)
				spec.TargetConfig, spec.ClientModel, spec.UpstreamModel = target.TargetConfig, "kimi-k3", "kimi-k3"
				if stream {
					// 收集全部下游事件，错误之前发出的内容也要检查。
					// added 身份通知可保留，无效输入不能进入执行事件。
					var output strings.Builder
					result := manager.ExecuteStream(t.Context(), freezeTestAttempt(spec), func(event execution.StreamEvent) error { output.Write(event.Data); return nil })
					if result.Error == nil || result.DispatchState == execution.DispatchNotSent || result.Usage == nil {
						t.Fatalf("invalid stream was accepted or lost billing evidence: %+v", result)
					}
					// 提前发布源码可能触发客户端执行，最终报错也无法补救。
					// 也不能先发 completed 再补错误，客户端可能已结束处理。
					if strings.Contains(output.String(), "response.completed") || strings.Contains(output.String(), "response.custom_tool_call_input.") || strings.Contains(output.String(), "secret-output") {
						t.Fatal("invalid executable input or completion leaked")
					}
				} else {
					result := manager.Execute(t.Context(), freezeTestAttempt(spec))
					if result.Error == nil || result.DispatchState == execution.DispatchNotSent || result.Usage == nil {
						t.Fatalf("invalid output accepted or lost billing evidence: %+v", result)
					}
					// 错误说明应为固定诊断，不得夹带完整 arguments。
					// 普通响应没有中间帧，但日志摘要仍是独立泄漏出口。
					if strings.Contains(result.Error.Summary, "secret-output") {
						t.Fatal("source leaked into error")
					}
				}
				// 坏输出已被上游实际生成，不属于可以标成未发送的本地校验失败。
				// 不能为了拿到合法 JSON 而自动再次调用模型，派发次数必须保持一。
				if calls != 1 {
					t.Fatalf("unexpected upstream calls: %d", calls)
				}
			})
		}
	}
}

// 工具选择不能随 custom→function 转换丢成 auto；两类 OpenCode 入口共用合同。
// required 白名单只允许 exec，不扩大为全部普通函数。
// 强制 custom 在 Chat 中须变成同名 function 选择。
// required 白名单继续要求工具调用，不能降为自由回答的 auto。
// 原生 GPT 对照使用相同动态文法，应完全绕开桥接器。
// 这里从 Zen 入口验证，避免只接通 Go 渠道。
// 原生响应直接是 custom，不应被强行要求携带 JSON 包装。
func TestOpenCodeExecChoicesAndNativeIsolation(t *testing.T) {
	for _, tc := range []struct {
		name   string
		choice any
		want   string
		native bool
	}{
		{"forced", map[string]any{"type": "custom", "name": "exec"}, "exec", false},
		{"allowlist", map[string]any{"type": "allowed_tools", "mode": "required", "tools": []any{map[string]any{"type": "custom", "name": "exec"}}}, "required", false},
		{"native", "auto", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				wire, _ := io.ReadAll(r.Body)
				w.Header().Set("Content-Type", "application/json")
				if tc.native {
					// 原生 GPT 必须原样保留 custom 声明及语法，不能被适配器提前改写。
					if r.URL.Path != "/v1/responses" || gjson.GetBytes(wire, "input.0.tools.0.tools.0.type").String() != "custom" || gjson.GetBytes(wire, "input.0.tools.0.tools.0.format.definition").String() != execGrammarFixture {
						t.Errorf("native request changed: %s", wire)
					}
					io.WriteString(w, `{"id":"native","object":"response","model":"gpt-6-luna","status":"completed","output":[{"type":"custom_tool_call","call_id":"native_call","name":"exec","input":"text(1)"}]}`)
				} else {
					// named function 是对象，required 是字符串，统一读取预期选择值。
					// 检查实际上游 wire，而非准备阶段内存中的临时结构。
					value := gjson.GetBytes(wire, "tool_choice")
					if value.IsObject() {
						value = value.Get("function.name")
					}
					if r.URL.Path != "/v1/chat/completions" || value.String() != tc.want {
						t.Errorf("choice changed: %s", wire)
					}
					io.WriteString(w, openAIChatFinalResponse)
				}
			}))
			defer server.Close()
			manager, target := newOpenCodeManagerForTest(t, channel.OpenCodeZen, server.URL)
			// 只改变最终目标便应切换路径，不依赖源名与目标名是否不同。
			// 覆盖 New API 已映射、执行层只看到目标名称的实际情况。
			model := "kimi-k3"
			if tc.native {
				model = "gpt-6-luna"
			}
			body := execRequestFixture(t, false, nil, tc.choice)
			spec := utilitySpec(channel.OpenCodeZen, protocol.OpenAIResponses, execution.OperationResponsesCreate, http.MethodPost, "/v1/responses", body)
			spec.TargetConfig, spec.ClientModel, spec.UpstreamModel = target.TargetConfig, model, model
			result := manager.Execute(t.Context(), freezeTestAttempt(spec))
			if result.Error != nil {
				t.Fatalf("choice/native execution failed: %+v", result.Error)
			}
			// 请求不变还不够，原生返回也必须保持 custom 类型。
			// 原生合同不含本适配包装，不能套用二次参数解码。
			if tc.native && gjson.GetBytes(result.Body, "output.0.type").String() != "custom_tool_call" {
				t.Fatal("native custom output changed")
			}
		})
	}
}

// 并行两次 exec 与外部命名空间的同名普通函数混用，恢复必须按 item_id 隔离。
// 流中新增 delta、抑制包装参数后，客户端序号仍应连续，输出索引仍指向原条目。
// 默认 exec 与 other.exec 裸名字相同，但协议类型不同。
// 两次默认调用有不同 call_id，不能用函数名作状态键。
// 同时返回不同索引，检查过滤事件后输出顺序仍不改变。
// 普通函数保留 function delta，仅自定义调用改成 custom delta。
// 快照与增量必须采用一致的工具身份和命名空间。
// 连续序号可发现抑制旧事件后留下的空洞或重复。
func TestOpenCodeExecParallelStream(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		wire, _ := io.ReadAll(r.Body)
		if gjson.GetBytes(wire, "tools.1.function.name").String() != "other__exec" {
			t.Errorf("other namespace lost: %s", wire)
		}
		// 一个 chunk 返回多个工具，模拟上游合并发送的最小并行回合。
		// 并行工具不代表额外 HTTP，本例只执行一次模型请求。
		var calls []any
		for i, name := range []string{"exec", "exec", "other__exec"} {
			// 普通同名函数用空对象，误套 custom 解包器就会失败。
			// 默认两个调用源码各不相同，可发现参数与身份错配。
			arguments := `{}`
			if i < 2 {
				v, _ := json.Marshal(map[string]string{"input": fmt.Sprintf("text(%d)", i)})
				arguments = string(v)
			}
			calls = append(calls, map[string]any{"index": i, "id": fmt.Sprintf("call_%d", i), "type": "function", "function": map[string]any{"name": name, "arguments": arguments}})
		}
		chunk, _ := json.Marshal(map[string]any{"id": "parallel", "model": "kimi-k3", "choices": []any{map[string]any{"index": 0, "delta": map[string]any{"tool_calls": calls}, "finish_reason": "tool_calls"}}})
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", chunk)
	}))
	defer server.Close()
	body, err := sjson.SetBytes(execRequestFixture(t, true, nil, "auto"), "input.0.tools.1", map[string]any{"type": "namespace", "name": "other", "tools": []any{map[string]any{"type": "function", "name": "exec", "parameters": map[string]any{"type": "object", "properties": map[string]any{}}}}})
	if err != nil {
		t.Fatal(err)
	}
	manager, target := newOpenCodeManagerForTest(t, channel.OpenCodeGo, server.URL)
	spec := utilitySpec(channel.OpenCodeGo, protocol.OpenAIResponses, execution.OperationResponsesCreate, http.MethodPost, "/v1/responses", body)
	spec.TargetConfig, spec.ClientModel, spec.UpstreamModel = target.TargetConfig, "kimi-k3", "kimi-k3"
	// 从客户端第一条事件开始累计，不仅比较末尾相邻事件。
	// delta 数单独记录，防止正确终态掩盖中间输入缺失。
	sequence := int64(0)
	var outputs []gjson.Result
	// 每个自定义调用完整解包后各发布一条源码 delta。
	// 这里不要求参数边产生边透传，严格校验优先于半条源码的即时展示。
	customDeltas := 0
	// 普通函数仍沿用 SDK 增量行为，不应被 custom 缓冲策略一起暂停。
	// 至少保留一条普通参数 delta，可发现误把所有 exec 名字归成同一类型。
	functionDeltas := 0
	result := manager.ExecuteStream(t.Context(), freezeTestAttempt(spec), func(event execution.StreamEvent) error {
		if event.Kind != execution.StreamEventData {
			return nil
		}
		for _, line := range strings.Split(string(event.Data), "\n") {
			if !strings.HasPrefix(line, "data: {") {
				continue
			}
			v := gjson.Parse(strings.TrimPrefix(line, "data: "))
			// 只对 SSE 业务 JSON 帧计数，排除其他内部执行事件。
			// 计数独立于 SDK 状态，完全依据客户端收到的顺序。
			if v.Get("sequence_number").Int() != sequence {
				t.Errorf("sequence %d expected %d", v.Get("sequence_number").Int(), sequence)
			}
			sequence++
			switch v.Get("type").String() {
			case "response.custom_tool_call_input.delta":
				customDeltas++
			case "response.function_call_arguments.delta":
				functionDeltas++
			case "response.completed":
				outputs = v.Get("response.output").Array()
			}
		}
		return nil
	})
	// 工具数必须与上游一致，不以丢掉不便转换的工具换取成功。
	// 两条 custom 加一条普通函数共同构成完整并行回合。
	if result.Error != nil || len(outputs) != 3 || customDeltas != 2 || functionDeltas != 1 {
		t.Fatalf("parallel output mismatch: error=%+v outputs=%d custom=%d function=%d", result.Error, len(outputs), customDeltas, functionDeltas)
	}
	// 最后一个普通函数应恢复 other，其余保持 functions。
	// 逐项检查 call_id，防止 map 顺序让并行结果错位。
	for i, item := range outputs {
		wantType, wantNamespace := "custom_tool_call", "functions"
		if i == 2 {
			wantType, wantNamespace = "function_call", "other"
		}
		if item.Get("type").String() != wantType || item.Get("namespace").String() != wantNamespace || item.Get("call_id").String() != fmt.Sprintf("call_%d", i) {
			t.Errorf("parallel identity changed: %s", item.Raw)
		}
		// 身份正确仍可能串参数，两个不同源码必须分别跟随原索引返回。
		if i < 2 && item.Get("input").String() != fmt.Sprintf("text(%d)", i) {
			t.Errorf("parallel input crossed calls: %s", item.Raw)
		}
	}
}

// 网关级回归验证：客户端 GPT 别名最终走 Kimi，协议恢复不止停在执行器内部。
// 状态发布器负责编译模型映射，认证和请求冻结都采用生产入口。
// 供应商替换为本机 HTTP，不读取线上凭据或真实用户任务。
// 日志必须保留最终协议证据，以便把模型路由问题与工具转换问题区分开。
// 执行器测试不能替代入口验证，入口可能提前拒绝 custom 类型。
// 实际 Gin handler 证明动态声明在路由和冻结后仍能进入适配。
// 检查客户端最终 JSON，覆盖网关响应编码而非只看内部结果。
// 别名和目标不同，日志转换模式须来自真正的执行结果。
func TestOpenCodeExecMappedGateway(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		wire, _ := io.ReadAll(r.Body)
		// 外部仍请求 Responses；真正发给目标的路径和 model 才决定执行语义。
		if r.URL.Path != "/v1/chat/completions" || gjson.GetBytes(wire, "model").String() != "kimi-k3" || gjson.GetBytes(wire, "tools.0.function.name").String() != "exec" {
			t.Errorf("invalid gateway conversion: %s %s", r.URL.Path, wire)
		}
		w.Header().Set("Content-Type", "application/json")
		// 返回真实 Chat function 形状，不能让测试上游预先伪装成 custom。
		io.WriteString(w, `{"id":"mapped","object":"chat.completion","model":"kimi-k3","choices":[{"index":0,"message":{"role":"assistant","tool_calls":[{"id":"c","type":"function","function":{"name":"exec","arguments":"{\"input\":\"text(1)\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":8,"completion_tokens":2,"total_tokens":10}}`)
	}))
	defer server.Close()
	engine, manager, input, sink, _ := newAggregationSearchGateway(t, channel.OpenCodeGo, server.URL)
	// 映射以配置快照发布，不直接篡改本次执行对象来绕过路由检查。
	input.Groups[0].Models = []state.ModelConfig{{ID: "kimi-k3", Alias: "gpt-6-luna"}}
	input.Groups[0].Settings = nil
	if _, err := manager.Publish(input); err != nil {
		t.Fatal(err)
	}
	body, err := sjson.SetBytes(execRequestFixture(t, false, nil, "auto"), "model", "gpt-6-luna")
	if err != nil {
		t.Fatal(err)
	}
	// 使用隔离夹具中的受限 Key，完整经过网关认证与目标选择。
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
	request.Header.Set("Authorization", "Bearer gl-search-client")
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	engine.ServeHTTP(response, request)
	// 客户端应看到自由文本和原调用 ID，而不是 JSON 参数包装。
	// 网关可见的 call_id 是续轮关联键，不能替换成 SDK 生成的 item ID。
	// 验证 input 的原文可同时确认对外没有多包一层 JSON。
	output := gjson.Get(response.Body.String(), "output.0")
	if response.Code != 200 || output.Get("type").String() != "custom_tool_call" || output.Get("input").String() != "text(1)" || output.Get("call_id").String() != "c" {
		t.Fatalf("gateway exec restoration failed: %d %s", response.Code, response.Body.String())
	}
	// 协议转换不增加派发次数，避免重复消费或产生两条请求审计。
	if len(sink.events) != 1 || len(sink.events[0].Attempts) != 1 || sink.events[0].Attempts[0].RouteMode != channel.RouteConverted || sink.events[0].Attempts[0].UpstreamProtocol != protocol.OpenAICompletions {
		t.Fatalf("gateway execution evidence missing: %+v", sink.events)
	}
}
