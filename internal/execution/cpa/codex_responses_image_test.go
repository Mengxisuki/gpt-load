// 用公共 HTTP 入口、真实调度和 CPA 执行器验证 Responses 生图转换。
// 上游由回环服务模拟，测试只使用合成图片、提示词和订阅凭据。
// 夹具与原生 Codex 测试共用，分组同时提供文本和生图模型。
// 请求必须经过正常的 AccessKey 验证，不能直接构造已通过检查的执行尝试。
// 上游只接受回环服务的证书，让 SDK 使用真实的 HTTPS 请求构造。
// 测试成功不代表生产账号有生图权限，断言关注格式、路由和执行记录。
// 测试中的图片由固定样本构造，避免依赖远端随机生成结果。
package cpa

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"

	"gpt-load/internal/execution"
	"gpt-load/internal/protocol"
	"gpt-load/internal/telemetry"
)

// 同一生图型号既能接受原生 Images，也能在外部协议保持 Responses。
// 这里以模型的请求日志与客户端可见事件交叉验证，避免只校验转换函数。
// 普通 JSON 和 SSE 使用同一张最终图，保证两种调用方式下图片都可提取。
// 原生上游不返回文本，成功必须由实际图像数据证明。
// 通过原始用量和归一化日志分别核对计费输入，覆盖两端协议的字段差异。
func TestCodexResponsesImageGateway(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const model = "gpt-image-2.6-sunburst"
	// 长于 CPA 的单次 32 KiB 读块，确保测试覆盖图片事件跨块的实际路径。
	// 用可解码的字节串作为合成部分图片，避免网络断片被错误地当成坏 base64。
	partial := strings.Repeat("YWJj", 12000)
	// 保留明细对象且输入输出 token 不同，捕捉错误形状和计量字段互换。
	const usage = `{"input_tokens":2,"output_tokens":7,"total_tokens":9,"input_tokens_details":{"image_tokens":0,"text_tokens":2}}`
	for _, test := range []struct {
		name, input, wantPrompt string
		stream                  bool
	}{
		// 使用最简字符串输入，覆盖普通 SDK 调用。
		{"普通字符串", `"draw a circle"`, "draw a circle", false},
		// 两段文本不同，断言顺序及分隔符保留，防止只取第一段的实现误通过。
		{"流式单轮消息", `[{"type":"message","role":"user","content":[{"type":"input_text","text":"draw"},{"type":"input_text","text":"a circle"}]}]`, "draw\na circle", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			// 上游 TLS 回调与测试分属不同 goroutine，使用原子计数核对真实派发。
			var calls atomic.Int32
			upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				// 前一次故障的上游地址是 /responses，这里固定为原生生成路径。
				// 路径和方法同时检查，执行器选择错误时即使有可解析响应也会失败。
				if r.URL.Path != "/backend-api/codex/images/generations" || r.Method != http.MethodPost {
					t.Errorf("上游路由错误 %s %s", r.Method, r.URL.Path)
				}
				var imageRequest map[string]any
				// 读取实际 HTTP 正文，验证 SDK 生图请求与输入转换共同生效。
				if err := json.NewDecoder(r.Body).Decode(&imageRequest); err != nil {
					t.Error(err)
				}
				// 确认选中模型、提示词与流开关在实际请求中保真。
				// reasoning、store 和空工具列表留在外部协议，Images 只收到生成选项。
				// SDK 在非流式时省略 stream，只有显式 true 才开启事件流。
				// 额外字段数量断言可识别原 Responses 字段被误传的情况。
				stream, _ := imageRequest["stream"].(bool)
				wantFields := 3
				if test.stream {
					wantFields++
				}
				if imageRequest["model"] != model || imageRequest["prompt"] != test.wantPrompt ||
					stream != test.stream || imageRequest["quality"] != "high" || len(imageRequest) != wantFields {
					t.Errorf("上游生图请求 %+v", imageRequest)
				}
				if !test.stream {
					// 普通返回只有 Images 字段，测试服务不会代替网关生成 Responses 外壳。
					w.Header().Set("Content-Type", "application/json")
					_, _ = fmt.Fprintf(w, `{"created":1,"data":[{"b64_json":%q}],"usage":%s}`, antigravityTestImage, usage)
					return
				}
				// 同时覆盖心跳、CRLF、事件名以及超过单块大小的 base64。
				// 两个上游图片事件分开刷新，使网关真正看到先到的部分图。
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = fmt.Fprintf(w, ": keepalive\r\n\r\nevent: image_generation.partial_image\r\ndata: {\"type\":\"image_generation.partial_image\",\"b64_json\":%q,\"partial_image_index\":0}\r\n\r\n", partial)
				w.(http.Flusher).Flush()
				_, _ = fmt.Fprintf(w, "data: {\"type\":\"image_generation.completed\",\"b64_json\":%q,\"usage\":%s}\n\n", antigravityTestImage, usage)
				// 终态后的数据不应产生第二个完成事件。
				_, _ = io.WriteString(w, "data: [DONE]\n\n")
			}))
			t.Cleanup(upstream.Close)
			// 通过真实分组模型登记触发按模型的转换路由。
			engine, sink := newCodexBootstrapGateway(t, upstream.URL, nil, model)
			// 兼容已有客户端发送的推理档位与空数组，图片质量应作为图片参数保留。
			body := fmt.Sprintf(`{"model":%q,"input":%s,"stream":%t,"reasoning":{"effort":"high"},"tools":[],"quality":"high"}`, model, test.input, test.stream)
			res := sendCodexImageResponses(t, engine, upstream, body)
			// 可用的主分组只需实际派发一次，不能尝试备选订阅。
			if res.Code != http.StatusOK || calls.Load() != 1 {
				t.Fatalf("HTTP %d，派发 %d 次，正文 %.400s", res.Code, calls.Load(), res.Body.String())
			}
			result := gjson.ParseBytes(res.Body.Bytes())
			// 普通调用直接解析响应对象，流式调用则从最后的完成事件取同一结构。
			if test.stream {
				// 逐条解析实际下发事件，校验序号和中间图片能被 Responses 客户端消费。
				// 客户端以 data.type 解码。上游的 image_generation.* 不能直接漏下来。
				var itemID string
				sequence, completions := 0, 0
				for _, line := range strings.Split(res.Body.String(), "\n") {
					// 事件空行是分帧标记，只有 data 行有 Responses JSON。
					data, ok := strings.CutPrefix(line, "data: ")
					if !ok {
						continue
					}
					event := gjson.Parse(data)
					// 要求序号字段存在且连续，缺省整数零不能误通过第一帧校验。
					if field := event.Get("sequence_number"); field.Type != gjson.Number || field.Int() != int64(sequence) {
						t.Fatalf("SSE 缺少序号 %d，实际 %s", sequence, field.Raw)
					}
					sequence++
					switch event.Get("type").String() {
					case "response.output_item.added":
						// 条目前导提供后续事件引用的唯一 ID。
						itemID = event.Get("item.id").String()
					case "response.image_generation_call.partial_image":
						// 部分图是在输出数组外临时呈现的内容，不能作为完成图计数。
						if itemID == "" || event.Get("item_id").String() != itemID || event.Get("partial_image_b64").String() != partial {
							t.Fatal("部分图片或对应的条目 ID 未保留")
						}
					case "response.completed":
						// 最终事件须携带整个响应对象，供 SDK 的 final_response 接口读取。
						// 记录完成次数，拒绝同一生成结果被发布为多个完成事件。
						result = event.Get("response")
						completions++
					}
				}
				// 共有两个前导、一个部分图片、三个完成事件。
				// 最终输出引用同一个条目，便于重复出现的图片结果被正确去重。
				if sequence != 6 || completions != 1 || itemID != result.Get("output.0.id").String() {
					t.Fatalf("SSE 终态无效，事件数 %d，完成数 %d", sequence, completions)
				}
			}
			// 两种传输都以 Responses 图片条目和相同的用量结束。
			// 要求 store=false，避免返回的本地 ID 被客户端当成可续接资源。
			if result.Get("object").String() != "response" || result.Get("id").String() == "" ||
				result.Get("model").String() != model || result.Get("store").Type != gjson.False ||
				result.Get("status").String() != "completed" || len(result.Get("output").Array()) != 1 ||
				result.Get("output.0.type").String() != "image_generation_call" ||
				result.Get("output.0.result").String() != antigravityTestImage || result.Get("usage").Raw != usage {
				t.Fatalf("Responses 生图结果或用量无效 %s", result.Raw)
			}
			// 客户端的逻辑操作用于公开协议，实际上游协议用于排障及重放决策。
			// 日志的归一化 token 断言确保只保留原始 JSON 还不够，采集器也必须读取到它。
			if len(sink.events) != 1 {
				t.Fatalf("请求日志数 %d", len(sink.events))
			}
			log := sink.events[0]
			// 客户端看到成功且日志必须同时观察到成功，避免事件未被网关识别为终态。
			if log.Status != telemetry.RequestStatusSuccess || log.Operation != execution.OperationResponsesCreate ||
				len(log.Attempts) != 1 || log.Attempts[0].RouteMode != execution.RouteConverted ||
				log.Attempts[0].UpstreamProtocol != protocol.OpenAIImages ||
				log.Usage.Result.Tokens.UncachedInput != 2 || log.Usage.Result.Tokens.Output != 7 {
				t.Fatalf("生图路由或用量日志 %+v", log)
			}
		})
	}
}

// 这些载荷无法由提示词保真表达。两个配置候选都不能收到无效或被截断的请求。
// 检查 HTTP 错误码和派发次数，避免拒绝结果来自上游的通用模型错误。
// 输入校验属于请求级错误，不能触发另一个账号继续派发。
// 不依赖上游懂 Responses 输入，非法内容应在构造图片载荷之前被拦住。
func TestCodexResponsesImageRejectsUnsupportedInputBeforeDispatch(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const model = "gpt-image-2.6-sunburst"
	var calls atomic.Int32
	// 若实现意外派发，这个服务明确拒绝并留下独立的网络证据。
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		http.Error(w, "unexpected request", http.StatusBadRequest)
	}))
	t.Cleanup(upstream.Close)
	// 同一夹具可连续验证多种请求，失败不会破坏分组运行状态。
	engine, _ := newCodexBootstrapGateway(t, upstream.URL, nil, model)
	for _, body := range []string{
		// 真正的工具列表不能当成单独的图片生成指令。
		`"input":"draw","tools":[{"type":"image_generation"}]`,
		// 编辑必须使用专用入口，避免参考图悄悄被忽略。
		`"input":[{"role":"user","content":[{"type":"input_image","image_url":"https://example.com/image.png"}]}]`,
		// 不截取最后一条信息来假装完整表达多轮意图。
		`"input":[{"role":"user","content":"draw"},{"role":"assistant","content":"ok"}]`,
		// 缺少绘制指令时不允许默认使用空提示请求上游。
		`"input":"   "`,
	} {
		res := sendCodexImageResponses(t, engine, upstream, fmt.Sprintf(`{"model":%q,%s}`, model, body))
		// 对客户端保留可识别的转换错误码，便于改用受支持的输入。
		if res.Code != http.StatusBadRequest || !strings.Contains(res.Body.String(), execution.ErrorCodeCodexImageConversionInput) {
			t.Fatalf("未明确拒绝输入，HTTP %d %s", res.Code, res.Body.String())
		}
	}
	// 检查整批无效请求之后仍然没有网络尝试。
	if calls.Load() != 0 {
		t.Fatalf("无效输入被派发 %d 次", calls.Load())
	}
}

// 未拿到最终图片时不得返回成功，也不能使用另一订阅账号再次尝试生成。
// 夹具包含两个账号，故派发次数可验证真实网关重放约束。
// HTTP 和 SSE 出错都经同一个生产调度器，验证改写协议没有放宽安全策略。
// 这里的错误缺少上游未处理的明确证据，不能以返回 503 或空图片推断无副作用。
// 两种传输的状态和错误体形态可以不同，均不得出现 Responses 成功完成。
func TestCodexResponsesImageDoesNotReplayUnknownOutcome(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const model = "gpt-image-2.6-sunburst"
	for _, test := range []struct {
		name, mediaType, payload string
		status                   int
		stream                   bool
	}{
		// HTTP 错误可能来自处理中断，保护未完成前的重复派发。
		{"上游状态未知", "application/json", `{"error":{"type":"server_error","message":"backend error"}}`, 503, false},
		// 通用过载提示曾用于文本流启动恢复，不能作为生图未执行的证明。
		{"流式握手的通用过载提示", "application/json", `{"error":{"type":"server_error","message":"Our servers are currently overloaded. Please try again later."}}`, 503, true},
		// 普通 200 也必须有图片，防止兼容层误报一个空输出。
		{"普通响应缺图", "application/json", `{"data":[]}`, 200, false},
		// 裸结束标记不能替代图片完成帧。
		{"流式没有终态", "text/event-stream", "data: [DONE]\n\n", 200, true},
		// 供应商 SSE 错误不能被转成成功完成。
		{"流式返回错误", "text/event-stream", "data: {\"type\":\"image_generation.failed\",\"error\":{\"message\":\"failed\"}}\n\n", 200, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			var calls atomic.Int32
			upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				// 两个账号都使用这一响应，任何额外尝试都会增加计数。
				calls.Add(1)
				w.Header().Set("Content-Type", test.mediaType)
				w.WriteHeader(test.status)
				_, _ = io.WriteString(w, test.payload)
			}))
			t.Cleanup(upstream.Close)
			engine, sink := newCodexBootstrapGateway(t, upstream.URL, nil, model)
			// 每个用例独立建账号和网关，避免首个失败改变后续用例的候选顺序。
			res := sendCodexImageResponses(t, engine, upstream, fmt.Sprintf(`{"model":%q,"input":"draw","stream":%t}`, model, test.stream))
			// 无图或缺终态时只有可见错误，不出现伪造的 completed。
			// 状态、出站次数和终态一起验证，保护成功和失败的可观察边界。
			if res.Code < 400 || strings.Contains(res.Body.String(), "response.completed") || calls.Load() != 1 {
				t.Fatalf("意外结果，HTTP %d，派发 %d 次，正文 %s", res.Code, calls.Load(), res.Body.String())
			}
			// 失败日志也要按实际尝试记录，供排查拒绝、取消与重放问题。
			if len(sink.events) != 1 || len(sink.events[0].Attempts) != 1 ||
				sink.events[0].Attempts[0].WillRetry || sink.events[0].Status == telemetry.RequestStatusSuccess {
				t.Fatalf("生图失败被重试或记为成功 %+v", sink.events)
			}
		})
	}
}

// sendCodexImageResponses 只替换网络传输以信任回环证书。
// 使用真实 AccessKey 入口、已选模型和渠道执行契约。
// 请求总是发往 Responses 路由，避免测试意外直接调用原生 Images。
// 响应记录器捕获普通 JSON 和事件流，调用者按实际模式验证。
func sendCodexImageResponses(t *testing.T, engine *gin.Engine, upstream *httptest.Server, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	// 保留请求上下文及测试取消能力，只指定测试上游的可信 transport。
	req = req.WithContext(context.WithValue(t.Context(), "cliproxy.roundtripper", upstream.Client().Transport))
	req.Header.Set("Authorization", "Bearer gl-client")
	req.Header.Set("Content-Type", "application/json")
	// 测试密钥与夹具中配置一致，不需要读取任何现场凭据。
	res := httptest.NewRecorder()
	engine.ServeHTTP(res, req)
	return res
}
