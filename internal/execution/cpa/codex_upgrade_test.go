// 本文件验证 CPA 升级后仍由 GPT-Load 决定重试和健康状态。
// 用例移植自上游 gpt-load 的 CPA v8.0.49 适配提交。
// 测试只替换远端传输，使用实际 CPA 转换器和网关。
// 不读取用户凭据，不访问模型供应商。
package cpa

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"gpt-load/internal/execution"
	"gpt-load/internal/health"
	"gpt-load/internal/telemetry"
)

// 容量不足与套餐额度耗尽都可能返回 429，但调度含义不同。
// 本测试以真实 SDK 错误分类结果驱动现有健康策略。
// 容量不足允许改选候选，不应写入套餐冷却。
// 明确的额度耗尽必须优先于同一响应中的容量错误码。
// 普通限流也不能仅凭相似的英文消息变成安全重放。
// 本例只验证 HTTP 的拒绝证据，WebSocket 重放条件另有测试。
// 不调用 CPA Manager，防止它绕过 GPT-Load 的候选选择。
// 凭据有效期在未来，排除自动刷新对网络次数的干扰。
// 出错时同时报告执行证据与决策，便于定位边界丢失。
func TestCodexTransientCapacityUsesExistingCandidatePolicy(t *testing.T) {
	for _, test := range []struct {
		name     string
		body     string
		capacity bool
	}{
		// 原厂明确的轮次限流码属于瞬时容量限制。
		{"turn limit", `{"error":{"type":"rate_limit_error","code":"turn_rate_limit_exceeded","message":"Codex is at capacity"}}`, true},
		// 缺少错误码时，由 SDK 的精确分类规则提供证据。
		{"capacity without code", `{"error":{"type":"rate_limit_error","message":"Codex is at capacity"}}`, true},
		// 冲突字段场景固定额度类型的优先级。
		{"quota takes precedence", `{"error":{"type":"usage_limit_reached","code":"turn_rate_limit_exceeded","message":"usage limit reached"}}`, false},
		// 近似措辞不应扩大 SDK 认可的容量范围。
		{"generic rate limit", `{"error":{"type":"rate_limit_error","code":"rate_limit_exceeded","message":"The model is at capacity"}}`, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			// 复用真实凭据准备流程，避免直接构造健康证据自证正确。
			adapter, _, _, keys, row := newAdapterFixture(t, credentialJSON("access", "refresh", time.Now().Add(time.Hour)))
			// 仅隔离网络边界，响应仍交给 CPA 解析。
			// 保留 Retry-After，覆盖携带等待提示的限流响应。
			ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", http.RoundTripper(roundTripperFunc(func(r *http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: http.StatusTooManyRequests,
					Header: http.Header{"Content-Type": {"application/json"}, "Retry-After": {"256"}},
					Body:   io.NopCloser(strings.NewReader(test.body)), Request: r}, nil
			})))
			result := adapter.Execute(ctx, validSpec(t, row, keys))
			if result.Error == nil || result.Error.StatusCode != http.StatusTooManyRequests {
				t.Fatalf("result = %+v", result)
			}
			// 使用公开决策入口，验证分类能够穿过执行器边界。
			decision := health.JudgeExecution(health.ExecutionAttempt{
				DispatchState: result.DispatchState, StatusCode: result.StatusCode, Evidence: result.Error, Now: time.Now(),
			}, health.DecisionContext{Method: http.MethodPost, Operation: execution.OperationResponsesCreate})
			// 只有明确拒绝处理的 HTTP 容量响应才能允许改选。
			// 不给账号施加额外健康惩罚，也不消耗额度冷却。
			if test.capacity {
				if result.Error.Hint != execution.FailureHintCandidateUnavailable ||
					result.Error.ReplaySafety != execution.ReplaySafetyRejectedBeforeProcessing ||
					decision.Retry != health.RetryNextCandidate || decision.Effect != health.EffectNone || decision.QuotaCooldown {
					t.Fatalf("capacity evidence=%+v decision=%+v", result.Error, decision)
				}
			} else if result.Error.Hint != execution.FailureHintRateLimited || !decision.QuotaCooldown {
				t.Fatalf("rate limit lost quota policy: evidence=%+v decision=%+v", result.Error, decision)
			}
		})
	}
}

// 同一个 Codex 终态会被转换成四种下游流协议。
// 转换器可能把未完成状态编码为普通终止帧。
// 因此网关日志还必须使用 SDK 的独立 Incomplete 证据。
// 同时测 completed，防止把所有正常终态误记成未完成。
// 两种结果都保留实际生成文本和用量，不能再次发起推理。
// Responses 保留原生事件，Chat 与 Anthropic 需要协议转换。
// Gemini 路径同时检查转换后的用量进入统一统计。
// 上游使用相同模型和负载，避免把路由差异误当成终态差异。
// 服务器只输出合成文本，不执行模型返回的工具。
func TestCodexGatewayPreservesIncompleteAcrossProtocols(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, terminal := range []string{"completed", "incomplete"} {
		for _, test := range []struct {
			name string
			path string
			body string
		}{
			{"Responses", "/v1/responses", `{"model":"gpt-5","stream":true,"input":"hello"}`},
			{"Chat", "/v1/chat/completions", `{"model":"gpt-5","stream":true,"messages":[{"role":"user","content":"hello"}]}`},
			{"Anthropic", "/v1/messages", `{"model":"gpt-5","stream":true,"max_tokens":100,"messages":[{"role":"user","content":"hello"}]}`},
			{"Gemini", "/v1beta/models/gpt-5:streamGenerateContent", `{"contents":[{"role":"user","parts":[{"text":"hello"}]}]}`},
		} {
			t.Run(terminal+"/"+test.name, func(t *testing.T) {
				// 请求计数检查整个网关链路是否发生隐藏重放。
				var calls atomic.Int32
				// 使用回环 TLS 上游，经过实际 HTTP 执行与流式转换。
				upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls.Add(1)
					w.Header().Set("Content-Type", "text/event-stream")
					// 先声明响应，再给出可观察的输出，避免被当作启动失败。
					_, _ = io.WriteString(w, "data: "+`{"type":"response.created","response":{"id":"resp_1","object":"response","status":"in_progress","created_at":1,"model":"gpt-5","output":[]}}`+"\n\n")
					_, _ = io.WriteString(w, "data: "+`{"type":"response.output_text.delta","response_id":"resp_1","output_index":0,"item_id":"msg_1","content_index":0,"delta":"partial answer"}`+"\n\n")
					// 输出预算耗尽属于已处理请求的未完成终态。
					// 它不是可换账号重试的鉴权或容量拒绝。
					details := ""
					if terminal == "incomplete" {
						details = `,"incomplete_details":{"reason":"max_output_tokens"}`
					}
					// 终态携带真实用量结构，验证转换后计费证据未丢失。
					_, _ = fmt.Fprintf(w, "data: "+`{"type":"response.%s","response":{"id":"resp_1","object":"response","status":"%s","model":"gpt-5"%s,"output":[],"usage":{"input_tokens":2,"output_tokens":1,"total_tokens":3}}}`+"\n\n", terminal, terminal, details)
				}))
				defer upstream.Close()
				// 复用真实路由、鉴权、执行器和日志收集入口。
				engine, sink := newCodexBootstrapGateway(t, upstream.URL, nil)
				request := httptest.NewRequest(http.MethodPost, test.path, strings.NewReader(test.body))
				// 仅信任测试服务器自己的证书，不修改全局 TLS 策略。
				request = request.WithContext(context.WithValue(t.Context(), "cliproxy.roundtripper", upstream.Client().Transport))
				request.Header.Set("Authorization", "Bearer gl-client")
				request.Header.Set("Content-Type", "application/json")
				response := httptest.NewRecorder()
				engine.ServeHTTP(response, request)
				// 已输出内容必须完整返回，不能追加伪造错误帧。
				// 一次请求对应一次尝试和一条汇总日志。
				if calls.Load() != 1 || len(sink.events) != 1 || response.Code != http.StatusOK ||
					!strings.Contains(response.Body.String(), "partial answer") || strings.Contains(response.Body.String(), "event: error") {
					t.Fatalf("calls=%d logs=%d response=%d %s", calls.Load(), len(sink.events), response.Code, response.Body)
				}
				// 对用户仍返回流式 200，日志负责区分完成与未完成。
				event := sink.events[0]
				wantStatus, wantCode := telemetry.RequestStatusSuccess, ""
				if terminal == "incomplete" {
					wantStatus, wantCode = telemetry.RequestStatusIncomplete, "upstream_response_incomplete"
				}
				// 未完成属于生成结果，不应对凭据施加健康惩罚。
				// 不因生成中断丢弃已经产生的输入、输出用量。
				if event.Status != wantStatus || event.ErrorCode != wantCode || len(event.Attempts) != 1 ||
					event.Attempts[0].Effect != telemetry.EffectNone || event.Attempts[0].WillRetry ||
					event.Usage.Result.Tokens.UncachedInput != 2 || event.Usage.Result.Tokens.Output != 1 {
					t.Fatalf("status=%s code=%s attempts=%+v usage=%+v", event.Status, event.ErrorCode, event.Attempts, event.Usage.Result)
				}
			})
		}
	}
}
