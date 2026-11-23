// 验证 Codex 订阅的 Image 2.6 请求从网关入口进入原生生图执行器。
// 包含回环 TLS 上游及真实 HTTP 路由、凭据选择、SDK 执行和请求日志。
// 合成账号和图片仅用于验证协议，测试结果不代表真实账号获得生图权限。
// 原生路径回归由上游实际收到的 URL 判定，避免仅凭两种接口都能返回图片而误判。
// 复用订阅网关装配，可覆盖配置中登记模型但执行器仍选择旧接口的故障。
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

	"gpt-load/internal/execution"
	"gpt-load/internal/protocol"
	"gpt-load/internal/telemetry"
)

// 三个模型名均应原样交给原生 Images 接口，由上游决定模型和账号权限。
// 生成与编辑分别验证独立操作，流式用例覆盖客户端可见的图片终态。
// 协议入口、上游路径和日志操作都参与验收，避免 SDK 升级后任一层误归为文本。
// 二进制图片使用已有的 PNG 样本，便于通过网关的格式校验并比较完整内容。
func TestCodexGatewayImage26UsesNativeImagesEndpoints(t *testing.T) {
	// 使用 SDK 对外发现的完整 Image 2.6 系列，避免只覆盖本次故障中的 Sunburst。
	for _, model := range []string{"gpt-image-2.6", "gpt-image-2.6-flare", "gpt-image-2.6-sunburst"} {
		for _, test := range []struct {
			endpoint  string
			operation execution.Operation
			// 参考图为空时使用生成请求协议，编辑时保留在 images 数组中。
			reference string
			terminal  string
		}{
			// 生成只发送文本提示，编辑通过独立的终态事件报告图片完成。
			{"/images/generations", execution.OperationImagesGenerate, "", "image_generation.completed"},
			// JSON 编辑用例携带一张参考图，验证请求转换时保留图片输入。
			{"/images/edits", execution.OperationImagesEdit, "data:image/png;base64," + antigravityTestImage, "image_edit.completed"},
		} {
			// CPA 分别实现普通和流式执行，请求都应使用同一个原生端点。
			for _, stream := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s%s/stream=%t", model, test.endpoint, stream), func(t *testing.T) {
					// 回环服务只模拟远端响应，同时记录实际请求次数和目标路径。
					// HTTP 回调在独立 goroutine 中运行，用原子计数与测试主体同步。
					var calls atomic.Int32
					upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						calls.Add(1)
						// 旧执行器会转到 Responses 图片工具，用路径断言区分并明确报错。
						// 拒绝错误路径，避免测试上游返回的成功内容掩盖退回 Responses 的行为。
						if want := "/backend-api/codex" + test.endpoint; r.Method != http.MethodPost || r.URL.Path != want {
							// 同时打印实际方法与路径，便于区分请求构造与模型路由错误。
							t.Errorf("上游请求 %s %s，预期 POST %s", r.Method, r.URL.Path, want)
							http.Error(w, "unexpected upstream route", http.StatusBadRequest)
							return
						}
						// 编辑参数和流式开关都必须实际到达 SDK 的原生 HTTP 端点。
						// 只解析关键字段，允许 SDK 补充其他合法的生图选项。
						var got struct {
							Model  string `json:"model"`
							Prompt string `json:"prompt"`
							Stream bool   `json:"stream"`
							Images []struct {
								URL string `json:"image_url"`
							} `json:"images"`
						}
						if err := json.NewDecoder(r.Body).Decode(&got); err != nil || got.Model != model || got.Prompt != "draw a circle" || got.Stream != stream {
							t.Errorf("上游生图字段 %+v，解析错误 %v", got, err)
						}
						// 完整比较参考图的 data URL，避免仅确认数组存在就放过图片丢失。
						if test.reference != "" && (len(got.Images) != 1 || got.Images[0].URL != test.reference) {
							t.Errorf("上游编辑请求丢失参考图 %+v", got.Images)
						}
						// 使用真实格式的合成图像，覆盖网关的普通响应和 SSE 提取。
						if stream {
							// 声明事件流媒体类型，使 SDK 走正常 SSE 分帧处理。
							w.Header().Set("Content-Type", "text/event-stream")
							// 图片已经生成时发送完整 SSE 事件，复用协议层的终态识别。
							_, _ = fmt.Fprintf(w, "data: {\"type\":%q,\"b64_json\":%q}\n\n", test.terminal, antigravityTestImage)
							return
						}
						// 普通响应使用 Images 的 data 数组封装，覆盖单次响应的网关处理。
						w.Header().Set("Content-Type", "application/json")
						_, _ = io.WriteString(w, `{"created":1,"data":[{"b64_json":"`+antigravityTestImage+`"}]}`)
					}))
					t.Cleanup(upstream.Close)

					// 使用与线上一致的公共 Images 路由和分组选模流程。
					// 测试分组同时具备文本路由能力，以复现生产中混用的订阅分组。
					engine, sink := newCodexBootstrapGateway(t, upstream.URL, nil, model)
					body := fmt.Sprintf(`{"model":%q,"prompt":"draw a circle","stream":%t`, model, stream)
					// 生成请求省略参考图，保留两个操作各自的公开请求协议。
					if test.reference != "" {
						body += fmt.Sprintf(`,"images":[{"image_url":%q}]`, test.reference)
					}
					req := httptest.NewRequest(http.MethodPost, "/v1"+test.endpoint, strings.NewReader(body+"}"))
					// 仅替换传输以信任回环服务的证书，保留 SDK 的请求构造。
					req = req.WithContext(context.WithValue(t.Context(), "cliproxy.roundtripper", upstream.Client().Transport))
					// 用测试 AccessKey 走完整鉴权流程，生成侧仍使用分组订阅凭据。
					req.Header.Set("Authorization", "Bearer gl-client")
					req.Header.Set("Content-Type", "application/json")
					// 用 HTTP 记录器捕获最终返回的状态和响应体，穿过网关的转发处理。
					res := httptest.NewRecorder()
					engine.ServeHTTP(res, req)

					// 已生成的图像需要完整返回，且一次客户端请求只派发一次。
					// 夹具保留备用分组，额外派发会暴露隐藏的重试或候选顺延。
					if calls.Load() != 1 || res.Code != http.StatusOK || !strings.Contains(res.Body.String(), antigravityTestImage) {
						t.Fatalf("派发 %d 次，HTTP %d，响应 %s", calls.Load(), res.Code, res.Body)
					}
					// 流式客户端依赖事件类型判断结束，单有图片数据不足以完成调用。
					if stream && !strings.Contains(res.Body.String(), test.terminal) {
						t.Fatalf("缺少流式终态 %q，响应 %s", test.terminal, res.Body)
					}
					// 验证日志认定调用成功，并将上游尝试识别为原生 Images。
					// 只有尝试日志能证明上游走的协议，汇总日志只记录客户端入口。
					if len(sink.events) != 1 {
						t.Fatalf("应写入一条请求日志，实际 %d 条", len(sink.events))
					}
					// 核对同一条汇总和尝试日志，确保没有把中间结果记为最终成功。
					event := sink.events[0]
					// 请求操作来自入口，上游协议来自实际执行，两者分别校验。
					if event.Status != telemetry.RequestStatusSuccess || event.Operation != test.operation ||
						event.UpstreamModel != model || len(event.Attempts) != 1 || event.Attempts[0].UpstreamProtocol != protocol.OpenAIImages {
						t.Fatalf("生图路由日志 %+v", event)
					}
				})
			}
		}
	}
}
