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
)

// 合成历史保持现场的两个并行调用、图片结果及插入的 developer 消息。
// 图片仅供结构测试；真实上游联调使用下方独立的有效 PNG。
// 未知字段与大整数用来保护 RawMessage 精度，不能经 float64 重建。
const historyCallA = `{"type":"function_call","call_id":"a","name":"read_a","arguments":"{}","status":"completed"}`

// 工具名称不同但调用 ID 才是归属依据，不能将两个结果合并成一份。
const historyCallB = `{"type":"function_call","call_id":"b","name":"read_b","arguments":"{}","status":"completed"}`
const historyOutputA = `{"type":"function_call_output","call_id":"a","output":[{"type":"input_image","image_url":"data:image/png;base64,c3ludGhldGlj"}],"extension":9007199254740993}`
const historyOutputB = `{"type":"function_call_output","call_id":"b","output":"B"}`

// developer 带扩展字段，防止适配将原项目降级为单纯的 content 字符串。
const historyDeveloper = `{"type":"message","role":"developer","content":[{"type":"input_text","text":"Continue with both results."}],"extension":"keep"}`

// TestOpenCodeToolHistoryBoundary 检查真实正文准备入口的完整历史输出。
// 期望值由案例显式声明，不复写实现的扫描算法。
// 对所有不适用或不完整历史要求逐字不变，错误仍交给上游。
// 这组测试只验证局部排序合同，供应商行为由显式联调另行确认。
// 结果归属依据 call_id，而不是工具名称或数组索引。
// 因此返回次序与调用次序不一致时仍应保留真实返回次序。
// 协议允许 message 省略 type，显式与简写形式共用同一角色约束。
// 指令的正文和扩展字段都是合同的一部分，不允许只留下可见文字。
// 真实上游在实验中要求连续结果，本地不再复写其内部实现。
// 坏数据案例要求保持原样并继续失败，不能用删除项目把错误变成成功。
// 将来扩大渠道或型号范围时，必须先更新实测证据再调整隔离对照。
func TestOpenCodeToolHistoryBoundary(t *testing.T) {
	for _, test := range []struct {
		name, channel, model, input, want string
		chat, compact                     bool
	}{
		// 成功案例保留结果原顺序，只延后夹在其中的 developer 消息。
		// 两条消息也必须按原顺序保留，不能覆盖或拼成一条指令。
		{name: "image", input: historyCallA + "," + historyCallB + "," + historyOutputA + "," + historyDeveloper + "," + historyOutputB, want: historyCallA + "," + historyCallB + "," + historyOutputA + "," + historyOutputB + "," + historyDeveloper},
		{name: "reversed_results", input: historyCallA + "," + historyCallB + "," + historyOutputB + "," + historyDeveloper + "," + historyDeveloper + "," + historyOutputA, want: historyCallA + "," + historyCallB + "," + historyOutputB + "," + historyOutputA + "," + historyDeveloper + "," + historyDeveloper},
		// 省略 message 类型的写法来自 Responses 简写合同。
		// 角色判断仍必须精确，不能将普通用户内容当开发者指令处理。
		{name: "implicit_message", input: historyCallA + "," + historyCallB + "," + historyOutputA + `,{"role":"developer","content":"Continue"},` + historyOutputB, want: historyCallA + "," + historyCallB + "," + historyOutputA + "," + historyOutputB + `,{"role":"developer","content":"Continue"}`},
		// 已连续的结果和尾随消息不需要重新序列化。
		// 单调用不属于本次已验证的并行故障范围。
		{name: "already_contiguous", input: historyCallA + "," + historyCallB + "," + historyOutputA + "," + historyOutputB + "," + historyDeveloper},
		{name: "single_call", input: historyCallA + "," + historyDeveloper + "," + historyOutputA},
		// 调用后、首个结果前的指令尚未做过语义对照，不移动它。
		// 这也避免把取消或约束工具执行的中途说明挪到执行结果后方。
		{name: "before_first_result", input: historyCallA + "," + historyCallB + "," + historyDeveloper + "," + historyOutputA + "," + historyOutputB},
		// 结果缺失或重复时不能捏造配对，否则会掩盖真实坏数据。
		// 重复调用 ID 即使带了两个结果也不能按顺序猜测其归属。
		{name: "missing_result", input: historyCallA + "," + historyCallB + "," + historyOutputA + "," + historyDeveloper},
		{name: "duplicate_result", input: historyCallA + "," + historyCallB + "," + historyOutputA + "," + historyDeveloper + "," + historyOutputB + "," + historyOutputB},
		// 一旦发现重复 ID，不能因为前两份结果看似齐全就整理部分历史。
		{name: "duplicate_call", input: historyCallA + "," + historyCallA + "," + historyOutputA + "," + historyDeveloper + "," + historyOutputA},
		{name: "unmatched_result", input: historyCallA + "," + historyCallB + "," + historyOutputA + "," + historyDeveloper + `,{"type":"function_call_output","call_id":"unknown","output":"C"}`},
		// 缺失 output 与显式 null 都不能伪装成空字符串结果。
		// 坏值保留可让上游给出真实错误，客户端不会看到假成功。
		{name: "missing_output", input: historyCallA + "," + historyCallB + "," + historyOutputA + "," + historyDeveloper + `,{"type":"function_call_output","call_id":"b"}`},
		{name: "null_output", input: historyCallA + "," + historyCallB + "," + historyOutputA + "," + historyDeveloper + `,{"type":"function_call_output","call_id":"b","output":null}`},
		// 用户与助手消息是语义边界，不能越过它们收集后面的结果。
		// custom 和动态工具历史也不属于普通 function 的配对规则。
		{name: "user_boundary", input: historyCallA + "," + historyCallB + "," + historyOutputA + "," + historyDeveloper + `,{"role":"user","content":"Stop"},` + historyOutputB},
		{name: "assistant_boundary", input: historyCallA + "," + historyCallB + "," + historyOutputA + "," + historyDeveloper + `,{"type":"message","role":"assistant","content":[]},` + historyOutputB},
		{name: "custom_boundary", input: historyCallA + "," + historyCallB + "," + historyOutputA + "," + historyDeveloper + `,{"type":"custom_tool_call","call_id":"custom","input":"text(1)"},` + historyOutputB},
		{name: "dynamic_boundary", input: historyCallA + "," + historyCallB + "," + historyOutputA + "," + historyDeveloper + `,{"type":"additional_tools","tools":[]},` + historyOutputB},
		// system 角色不能沿用 developer 特例，避免扩大指令重排范围。
		// 即使其内容看似相同，也保留供应商原始处理与拒绝行为。
		{name: "system_boundary", input: historyCallA + "," + historyCallB + "," + historyOutputA + `,{"role":"system","content":"Continue"},` + historyOutputB},
		// 坏字段类型不能被局部修复抹去，仍让原有协议校验拒绝。
		// 只有 role=developer 且 type 为 message/省略时可以移动。
		{name: "bad_call_id", input: `{"type":"function_call","call_id":42},` + historyCallB + "," + historyOutputA + "," + historyDeveloper + "," + historyOutputB},
		{name: "unknown_developer_type", input: historyCallA + "," + historyCallB + "," + historyOutputA + `,{"type":"future","role":"developer"},` + historyOutputB},
		// 精确按最终目标生效；用户别名不参与模型能力判断。
		// Zen、其他模型、其他协议都没有本轮实测依据。
		{name: "zen", channel: "opencode_zen"},
		{name: "other_channel", channel: "openai"},
		{name: "gpt", model: "gpt-6-luna"},
		{name: "other_deepseek", model: "deepseek-v4.1-flash"},
		{name: "chat", chat: true},
		// compact 用来压缩历史，不能把创建响应的兼容策略套入压缩输入。
		{name: "compact", compact: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			// 默认参数指向已复现的唯一渠道/型号组合。
			// 排除案例仍使用同一失败形状，避免空输入造成假通过。
			if test.channel == "" {
				test.channel = "opencode_go"
			}
			if test.model == "" {
				test.model = "deepseek-v4-pro"
			}
			if test.input == "" {
				test.input = historyCallA + "," + historyCallB + "," + historyOutputA + "," + historyDeveloper + "," + historyOutputB
			}
			body := []byte(`{"model":"` + test.model + `","stream":false,"input":[` + test.input + `],"reasoning":{"context":"all_turns"},"extension":9007199254740993}`)
			// 客户端别名与最终目标故意不同，防止误用 ClientModel 判断。
			// context 字段是 DeepSeek 已接受的参数，排序不能顺手删除它。
			spec := execution.AttemptSpec{ChannelID: test.channel, UpstreamModel: test.model, ClientModel: "public", Body: body, ClientProtocol: protocol.OpenAIResponses, Operation: execution.OperationResponsesCreate}
			if test.chat {
				spec.ClientProtocol, spec.Operation = protocol.OpenAICompletions, execution.OperationChatCompletion
			}
			if test.compact {
				// compact 原本会去除 stream，夹具先遵守该已存在的合同。
				// 否则逐字比较会因无关处理失败，无法检验历史隔离。
				spec.Operation = execution.OperationResponsesCompact
				body = bytes.ReplaceAll(body, []byte(`,"stream":false`), nil)
				spec.Body = body
			}
			original := bytes.Clone(body)
			// 保存独立字节快照，检测对底层共享切片的写入。
			// 仅比较 spec 指针不会发现就地改动，因而不能替代此快照。
			got, err := sanitizeNativeRequestBody(spec, false)
			if err != nil {
				t.Fatal(err)
			}
			// 比较完整正文，保护模型、推理及所有扩展字段。
			// 输出项目仍为原始 JSON，不允许精度或图片内容变化。
			want := original
			if test.want != "" {
				want = bytes.Replace(original, []byte(test.input), []byte(test.want), 1)
			}
			var gotFields, wantFields map[string]json.RawMessage
			// 顶层 map 序列化可能改变键顺序，字段内部数组顺序必须保留。
			// 对照使用 RawMessage，比 any 解码更严格地保护大整数。
			// 两个字段集合也要完全一致，防止只比较已知路径漏掉删除。
			if err := json.Unmarshal(got, &gotFields); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(want, &wantFields); err != nil {
				t.Fatal(err)
			}
			for key := range gotFields {
				gotFields[key] = compactHistoryJSON(t, gotFields[key])
			}
			for key := range wantFields {
				wantFields[key] = compactHistoryJSON(t, wantFields[key])
			}
			if !reflect.DeepEqual(gotFields, wantFields) {
				t.Fatalf("got %s, want %s", got, want)
			}
			if test.want == "" && !bytes.Equal(got, original) {
				t.Fatal("untargeted history was reserialized")
			}
			if !bytes.Equal(spec.Body, original) {
				t.Fatal("attempt mutated shared body")
			}
			// 已归一化的请求再次进入执行器时必须完全稳定。
			// 避免重试或下一轮历史造成 developer 消息持续移动。
			spec.Body = got
			// 重入使用第一次真实输出，而不是重新构造的已排序夹具。
			// 这能发现重序列化后第二次才暴露的判断差异。
			again, err := sanitizeNativeRequestBody(spec, false)
			if err != nil || !bytes.Equal(got, again) {
				t.Fatalf("not idempotent: %v", err)
			}
		})
	}
}

// compactHistoryJSON 只去掉空白，不经过 any/float64 解码。
// 对象键顺序保持已知 fixture 顺序；用于完整历史项目的无损比较。
// 输入必须是有效 JSON，错误会让测试失败而不是返回空的期望值。
// 此函数只用于断言，不会参与生产排序或重建任何业务字段。
func compactHistoryJSON(t *testing.T, raw []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// TestOpenCodeToolHistoryOnWire 通过真实执行器捕获普通与流式 HTTP 出站。
// 上游只接受已验证的结果连续形式，修复前会产生原故障的 400。
// 多批次历史和后续消息完整保留，模型别名仍由执行器处理。
// 两种模式都保留原 Responses 路由，不用 Chat 转换掩盖兼容问题。
// 合成上游对固定期望进行比较，不再实现一套工具配对算法。
// 因而实现排序错误或仅改了辅助函数时，实际 HTTP 回归会失败。
// 该测试不评估图片识别质量，也不执行返回的工具或代码。
func TestOpenCodeToolHistoryOnWire(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%t", stream), func(t *testing.T) {
			input := "[" + historyCallA + "," + historyCallB + "," + historyOutputA + "," + historyDeveloper + "," + historyOutputB + "]"
			want := "[" + historyCallA + "," + historyCallB + "," + historyOutputA + "," + historyOutputB + "," + historyDeveloper + "]"
			// 第二批使用不同 ID；两批间的用户消息不能跟着第一批移动。
			// 每批自己的 developer 消息必须留在该批的结果之后。
			separator := `,{"role":"user","content":"next synthetic turn"},`
			nextInput := strings.NewReplacer(`"a"`, `"c"`, `"b"`, `"d"`).Replace(input)
			nextWant := strings.NewReplacer(`"a"`, `"c"`, `"b"`, `"d"`).Replace(want)
			input = strings.TrimSuffix(input, "]") + separator + strings.TrimPrefix(nextInput, "[")
			// 此处只组合静态期望，不依据运行输出推导期望次序。
			// 原始输入与预期内容分别构造，避免同一个改写函数自证正确。
			want = strings.TrimSuffix(want, "]") + separator + strings.TrimPrefix(nextWant, "[")
			seen := make(chan []byte, 1)
			// 单请求单回包，用缓冲通道交接 HTTP 处理协程捕获的正文。
			// 不共享可变请求切片，从而适合 race 检查实际执行链路。
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, err := io.ReadAll(r.Body)
				if err != nil {
					t.Error(err)
					w.WriteHeader(500)
					return
				}
				seen <- body
				// 捕获真实网络正文，不以 helper 返回值代替执行器验收。
				// 精确序列比较同时证明未知字段、大整数与图片输出未丢失。
				if !bytes.Equal(compactHistoryJSON(t, []byte(gjson.GetBytes(body, "input").Raw)), compactHistoryJSON(t, []byte(want))) {
					w.WriteHeader(400)
					io.WriteString(w, `{"error":{"message":"No tool output found for tool call b."}}`)
					return
				}
				response := `{"id":"resp_diag","object":"response","status":"completed","model":"deepseek-v4-pro","output":[],"usage":{"input_tokens":10,"output_tokens":2,"total_tokens":12}}`
				// 两种模式引用同一响应快照，用量均由合成上游提供。
				// 终态只发一次，不能靠重复完成事件让消费者误以为成功。
				if stream {
					w.Header().Set("Content-Type", "text/event-stream")
					fmt.Fprintf(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":%s}\n\n", response)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				io.WriteString(w, response)
			}))
			defer server.Close()
			manager, target := newOpenCodeManagerForTest(t, channel.OpenCodeGo, server.URL)
			// 复用生产 runtime、目标解析、凭据快照及冻结路由合同。
			// 只有远程供应商被回环服务器替代，出站准备过程是真实实现。
			spec := utilitySpec(channel.OpenCodeGo, protocol.OpenAIResponses, execution.OperationResponsesCreate, http.MethodPost, "/v1/responses", []byte(fmt.Sprintf(`{"model":"public","stream":%t,"input":%s}`, stream, input)))
			spec.TargetConfig, spec.ClientModel, spec.UpstreamModel = target.TargetConfig, "public", "deepseek-v4-pro"
			spec = freezeTestAttempt(spec)
			// 冻结后不再变更目标或协议，用来覆盖真实执行合同的校验。
			original := bytes.Clone(spec.Body)
			// 普通请求必须返回成功；流式必须有且只有一个完成事件。
			// 下游终态保留上游用量，不能通过假空流绕开失败。
			if stream {
				var output strings.Builder
				result := manager.ExecuteStream(t.Context(), spec, func(e execution.StreamEvent) error { output.Write(e.Data); return nil })
				if result.Error != nil || strings.Count(output.String(), `"type":"response.completed"`) != 1 {
					t.Fatalf("stream failed: %+v %s", result.Error, output.String())
				}
				// 用量应来自原生快照，不因历史整理被重建为零。
				if !strings.Contains(output.String(), `"total_tokens":12`) {
					t.Fatal("stream usage lost")
				}
			} else {
				result := manager.Execute(t.Context(), spec)
				if result.Error != nil || result.StatusCode != 200 || gjson.GetBytes(result.Body, "status").String() != "completed" {
					t.Fatalf("request failed: %+v", result.Error)
				}
				if gjson.GetBytes(result.Body, "usage.total_tokens").Int() != 12 {
					t.Fatal("response usage lost")
				}
			}
			if got := gjson.GetBytes(<-seen, "model").String(); got != "deepseek-v4-pro" {
				t.Fatalf("wrong upstream model: %s", got)
			}
			if !bytes.Equal(spec.Body, original) {
				t.Fatal("shared history changed")
			}
		})
	}
}

// TestOpenCodeCompatibilityLive 仅显式设置环境时访问真实上游。
// 默认跳过，凭据只通过进程环境注入，不能出现在日志或测试失败详情。
// 固定合成正文不包含用户任务，响应中的工具调用从不执行。
// 可经受控隧道调用当前生产 GPT-Load，验证本地候选修复的实际效果。
// 这不是默认 CI 的外网依赖，环境变量缺失时必须清晰标为跳过。
// 当前测试只接受合成任务，不能用用户会话或生产请求替换夹具。
// 每次运行标识由调用者生成，普通、流式与不同案例各自独立。
func TestOpenCodeCompatibilityLive(t *testing.T) {
	endpoint, key, runID := os.Getenv("OAK_HISTORY_LIVE_URL"), os.Getenv("OAK_HISTORY_LIVE_KEY"), os.Getenv("OAK_HISTORY_LIVE_REQUEST_ID")
	if endpoint == "" || key == "" {
		t.Skip("requires explicit live endpoint and credential")
	}
	if runID == "" {
		t.Fatal("live request identity is required")
	}
	manager, target := newOpenCodeManagerForTest(t, channel.OpenCodeGo, endpoint)
	// 真实凭据仅进入生产快照封装，不通过错误消息或测试名称回显。
	// 合成会话不复用用户的 conversation/thread 标识。
	credential, err := json.Marshal(map[string]string{"api_key": key})
	if err != nil {
		t.Fatal(err)
	}
	// 纯红色 PNG 由标准 PNG 编码构造，确保图片不是请求校验的干扰项。
	// 图片只用于检验工具历史可接受性，不声称模型理解了画面内容。
	const png = "iVBORw0KGgoAAAANSUhEUgAAABAAAAAQCAIAAACQkWg2AAAAF0lEQVR4nGP4z8BAEiJN9aiGUQ1DSgMAkPn/Afnh+ngAAAAASUVORK5CYII="
	for _, variant := range []string{"text", "image", "flash"} {
		// 分别验证两个根因：Pro 的正文历史、Flash 的访问身份。
		// 相同用例再测流式，是为了排除只有普通路径接入修复的情况。
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", variant, stream), func(t *testing.T) {
				// 保持实际触发顺序：两次调用、第一份结果、developer、第二份结果。
				// 补齐人工合成的 reasoning_text，排除思考历史缺失造成的独立 400。
				input := `[{"role":"user","content":"Synthetic compatibility test. All results are complete. Reply only OK."},{"type":"reasoning","content":[{"type":"reasoning_text","text":"I will read both provided results and acknowledge completion."}],"summary":[]},` + historyCallA + "," + historyCallB + `,{"type":"function_call_output","call_id":"a","output":"A"},` + historyDeveloper + "," + historyOutputB + "]"
				body := []byte(fmt.Sprintf(`{"model":"deepseek-v4-pro","stream":%t,"input":%s,"tools":[{"type":"function","name":"read_a","description":"Synthetic result","parameters":{"type":"object","properties":{}}},{"type":"function","name":"read_b","description":"Synthetic result","parameters":{"type":"object","properties":{}}}]}`, stream, input))
				// 图片案例保持 developer 的原始失败位置，只替换第一份结果内容。
				// 测试不会预先重排，必须由本地候选执行器完成适配。
				if variant == "image" {
					body = bytes.Replace(body, []byte(`"output":"A"`), []byte(`"output":[{"type":"input_image","image_url":"data:image/png;base64,`+png+`"}]`), 1)
				}
				spec := utilitySpec(channel.OpenCodeGo, protocol.OpenAIResponses, execution.OperationResponsesCreate, http.MethodPost, "/v1/responses", body)
				spec.TargetConfig, spec.ClientModel, spec.UpstreamModel = target.TargetConfig, "deepseek-v4-pro", "deepseek-v4-pro"
				// Flash 沿用原 Chat 路径，只有 UA 修复参与，避免混淆两个根因。
				// 限制合成回答长度，测试仅消耗短问候所需的供应商额度。
				if variant == "flash" {
					spec.ClientModel, spec.UpstreamModel = "deepseek-v4.1-flash", "deepseek-v4.1-flash"
					spec.ClientProtocol, spec.Operation, spec.Path = protocol.OpenAICompletions, execution.OperationChatCompletion, "/v1/chat/completions"
					spec.Body = []byte(fmt.Sprintf(`{"model":"deepseek-v4.1-flash","stream":%t,"max_tokens":64,"messages":[{"role":"user","content":"Reply only OK."}]}`, stream))
				}
				spec.RequestID = fmt.Sprintf("%s-%s-%t", runID, variant, stream)
				spec.Header.Set("User-Agent", "Python-urllib/3.11")
				spec.Credential = execution.NewCredentialSnapshot(2, 1, 1, credential)
				// 使用同一供应商凭据验证，不自动切换账户规避失败。
				// 生产组的路由日志可独立对齐型号和协议，作为辅助证据。
				spec = freezeTestAttempt(spec)
				// 这里使用真实供应商回包；不打印推理或完整响应，只验收终态。
				// Python 默认 UA 同时验证明确的上游身份替换是否实际生效。
				if stream {
					var output strings.Builder
					result := manager.ExecuteStream(t.Context(), spec, func(e execution.StreamEvent) error { output.Write(e.Data); return nil })
					if result.Error != nil {
						t.Fatalf("live stream failed: %+v", result.Error)
					}
					terminal := `"type":"response.completed"`
					// Chat 与 Responses 的完成协议不同，不能共用假终态。
					if variant == "flash" {
						terminal = "[DONE]"
					}
					if result.StatusCode != 200 || !strings.Contains(output.String(), terminal) {
						t.Fatal("live stream did not complete")
					}
				} else {
					result := manager.Execute(t.Context(), spec)
					if result.Error != nil {
						t.Fatalf("live request failed: %+v", result.Error)
					}
					completed := gjson.GetBytes(result.Body, "status").String() == "completed"
					// 上游 HTTP 200 之外还要求应用层完成，避免空响应假通过。
					if variant == "flash" {
						completed = gjson.GetBytes(result.Body, "choices.0.finish_reason").String() == "stop"
					}
					if result.StatusCode != 200 || !completed {
						t.Fatal("live response did not complete")
					}
				}
			})
		}
	}
}
