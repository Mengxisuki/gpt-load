package gateway

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gpt-load/internal/execution"
	"gpt-load/internal/health"
	"gpt-load/internal/state"
)

func TestModelCooldownHandlesSSERejectionAndCanceledResponse(t *testing.T) {
	for _, canceled := range []bool{false, true} {
		ctx, cancel := context.WithCancel(context.Background())
		result := UpstreamResult{StatusCode: 200, Header: http.Header{"Retry-After": {"3600"}},
			Body: []byte(`{"error":{"type":"rate_limit_error"}}`), ProviderErrorBeforeCommit: true,
			RequestWritten: true, DispatchState: execution.DispatchMaybeSent, ResponseStarted: true,
			ExecutionError: &execution.ErrorEvidence{Kind: execution.ErrorKindProvider, Hint: execution.FailureHintRateLimited,
				ScopeHint: execution.ErrorScopeModel, StatusCode: 429, ReplaySafety: execution.ReplaySafetyRejectedBeforeProcessing}}
		forwarder := &scriptedForwarder{results: []UpstreamResult{result}}
		if canceled {
			forwarder.onCall = func(int) { cancel() }
		}
		engine, _, _, registry := newRequestLogHandlerTestRuntime(t, forwarder, &recordingAccessKeyRPMLimiter{}, &recordingRequestLogSink{}, "sk-one")
		request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"gpt-4o","messages":[]}`)).WithContext(ctx)
		request.Header.Set("Authorization", "Bearer gl-client")
		response := httptest.NewRecorder()
		engine.ServeHTTP(response, request)
		cancel()
		if !canceled && response.Code != 429 {
			t.Fatalf("SSE limit became %d", response.Code)
		}
		if len(registry.ModelCooldowns(1, time.Now())) != 1 {
			t.Fatalf("canceled=%t: lost decoded quota rejection", canceled)
		}
	}
}

func TestModelCooldownRetriesAndFiltersFutureRequests(t *testing.T) {
	forwarder := &scriptedForwarder{results: []UpstreamResult{
		{StatusCode: 429, Header: http.Header{"Retry-After": {"3600"}}, Body: []byte(`{"error":{"code":"rate_limit_exceeded"}}`),
			RequestWritten: true, DispatchState: execution.DispatchMaybeSent, ExecutionError: &execution.ErrorEvidence{
				Kind: execution.ErrorKindHTTP, Hint: execution.FailureHintRateLimited, StatusCode: 429}},
		successfulAffinityResult(), successfulAffinityResult(),
	}}
	engine, _, _, registry := newRequestLogHandlerTestRuntime(t, forwarder, &recordingAccessKeyRPMLimiter{}, &recordingRequestLogSink{}, "sk-one", "sk-two")
	for range 2 {
		request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"gpt-4o","messages":[]}`))
		request.Header.Set("Authorization", "Bearer gl-client")
		response := httptest.NewRecorder()
		engine.ServeHTTP(response, request)
		if response.Code != 200 {
			t.Fatalf("response=%d %s", response.Code, response.Body.String())
		}
	}
	if len(forwarder.inputs) != 3 || forwarder.inputs[0].Credential.ID == forwarder.inputs[1].Credential.ID || forwarder.inputs[0].Credential.ID == forwarder.inputs[2].Credential.ID {
		t.Fatalf("unexpected candidate retry chain: %#v", forwarder.inputs)
	}
	limited := forwarder.inputs[0].Credential.ID
	if len(registry.ModelCooldowns(limited, time.Now())) != 1 {
		t.Fatal("no model cooldown")
	}
	if until, _ := registry.CredentialCooldownUntil(limited); !until.IsZero() {
		t.Fatal("limited the whole credential")
	}
}

func TestOnlyModelCooledCandidatesReturn429WithoutDispatch(t *testing.T) {
	forwarder := &scriptedForwarder{}
	engine, _, _, registry := newRequestLogHandlerTestRuntime(t, forwarder, &recordingAccessKeyRPMLimiter{}, &recordingRequestLogSink{}, "sk-one")
	now := time.Now()
	ref, _ := registry.CredentialRef(1)
	registry.SetModelCooldown(ref, "gpt-4o", now.Add(time.Hour), now)
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"gpt-4o","messages":[]}`))
	request.Header.Set("Authorization", "Bearer gl-client")
	response := httptest.NewRecorder()
	engine.ServeHTTP(response, request)
	if response.Code != 429 || response.Header().Get("Retry-After") == "" || len(forwarder.inputs) != 0 {
		t.Fatalf("limited candidate response = %d %s; attempts=%d", response.Code, response.Body.String(), len(forwarder.inputs))
	}
	// 调度器未发送请求，调用方应能据可信标记选择后续资源。
	if response.Header().Get("X-GPTLoad-Replay-Safety") != "rejected_before_processing" {
		t.Fatal("missing safe fallback evidence for local cooldown")
	}
}

// 只有所有尝试都明确未处理时，429 才能授权调用方换来源。
// 上游伪造响应头、普通限流与前序未知结果都不能获得该授权。
// 使用真实路由、调度器与响应输出，仅替换外部执行器。
// 同一组两个凭据覆盖最后一次安全但前一次未知的情况。
// 普通 HTTP 拒绝与 SSE 首包拒绝走不同的输出分支，必须同时覆盖。
// SSE 的 HTTP 200 不是生成成功，解析出的 429 才是最终拒绝状态。
// 不把尚未提交输出等同于尚未处理输入，二者由独立证据约束。
// 所有错误正文都声称额度耗尽，验证实现没有按文案猜测。
// 上游注入和客户端注入使用同一标记，防止任一方向越过可信边界。
// 未知结果排在安全拒绝之前，保护整个请求而非最后一次尝试。
// 凭据健康冷却仍由真实实现更新，不用手动跳过第一次失败。
// 用不同凭据值保证第二次调用确实是候选切换。
// 只断言最终可观察的授权标记、状态和实际调用次数。
func TestResourceFallbackReplayEvidence(t *testing.T) {
	for _, test := range []struct {
		name   string
		safe   []bool
		want   bool
		stream bool
	}{
		{name: "quota rejection", safe: []bool{true}, want: true},
		{name: "unknown rejection", safe: []bool{false}},
		{name: "unknown then rejected", safe: []bool{false, true}},
		{name: "SSE bootstrap rejection", safe: []bool{true}, want: true, stream: true},
		{name: "unknown SSE rejection", safe: []bool{false}, stream: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			forwarder := &scriptedForwarder{}
			keys := make([]string, len(test.safe))
			for index, safe := range test.safe {
				// 只有执行器提供的语义证据才可信，错误正文刻意保持相同。
				// 这样不能通过匹配 usage_limit_reached 文案误判安全。
				evidence := &execution.ErrorEvidence{Kind: execution.ErrorKindHTTP, Hint: execution.FailureHintRateLimited, StatusCode: 429}
				if safe {
					evidence.ReplaySafety = execution.ReplaySafetyRejectedBeforeProcessing
				}
				// 伪造头必须被网关剥离，最终头只能由执行证据生成。
				forwarder.results = append(forwarder.results, UpstreamResult{StatusCode: 429,
					Header: http.Header{"X-GPTLoad-Replay-Safety": {"rejected_before_processing"}},
					Body:   []byte(`{"error":{"type":"usage_limit_reached"}}`), RequestWritten: true,
					DispatchState: execution.DispatchMaybeSent, ResponseStarted: true, ExecutionError: evidence})
				keys[index] = strings.Repeat("k", index+1)
			}
			// SSE 在生成首个输出前也可能被原厂拒绝，此时 HTTP 已返回 200。
			// 网关应转成明确的 429，并且沿用相同的请求级安全条件。
			if test.stream {
				forwarder.streamResults = forwarder.results
				forwarder.streamResults[0].StatusCode = 200
				forwarder.streamResults[0].ProviderErrorBeforeCommit = true
			}
			// 两个凭据用于观察前序未知状态是否被后续安全拒绝错误覆盖。
			// 期望调用次数来自输入用例，不依赖网关内部重试计数。
			engine, _, _, _ := newRequestLogHandlerTestRuntime(t, forwarder, &recordingAccessKeyRPMLimiter{}, &recordingRequestLogSink{}, keys...)
			body := `{"model":"gpt-4o","messages":[],"stream":` + fmt.Sprint(test.stream) + `}`
			request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
			request.Header.Set("Authorization", "Bearer gl-client")
			// 入站同名头也不能替代网关证据，真实客户端可任意填写它。
			request.Header.Set("X-GPTLoad-Replay-Safety", "rejected_before_processing")
			response := httptest.NewRecorder()
			engine.ServeHTTP(response, request)
			attempts := len(forwarder.inputs) + len(forwarder.streamInputs)
			// 安全标记不会自动重试自己，只向调用方传递已确认事实。
			if response.Code != 429 || attempts != len(test.safe) {
				t.Fatalf("status=%d attempts=%d", response.Code, attempts)
			}
			// 响应头必须与所有尝试的证据一致，不能照抄上游伪造值。
			if got := response.Header().Get("X-GPTLoad-Replay-Safety") == "rejected_before_processing"; got != test.want {
				t.Fatalf("safe fallback=%t, want %t", got, test.want)
			}
		})
	}
}

func TestQuotaRecoveryAllowsGatewayRequestsAgain(t *testing.T) {
	forwarder := &scriptedForwarder{results: []UpstreamResult{
		{StatusCode: 429, Header: http.Header{"Retry-After": {"172800"}}, Body: []byte(`{"error":{"type":"usage_limit_reached"}}`),
			RequestWritten: true, DispatchState: execution.DispatchMaybeSent, ExecutionError: &execution.ErrorEvidence{
				Kind: execution.ErrorKindHTTP, Hint: execution.FailureHintRateLimited, StatusCode: 429, Type: "usage_limit_reached"}},
		successfulAffinityResult(),
	}}
	engine, _, _, registry := newRequestLogHandlerTestRuntime(t, forwarder, &recordingAccessKeyRPMLimiter{}, &recordingRequestLogSink{}, "sk-one")
	for index, want := range []int{429, 200} {
		if index == 1 {
			registry.RestoreQuotaCooldowns(1)
		}
		request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"gpt-4o","messages":[]}`))
		request.Header.Set("Authorization", "Bearer gl-client")
		response := httptest.NewRecorder()
		engine.ServeHTTP(response, request)
		if response.Code != want {
			t.Fatalf("request %d returned %d, want %d: %s", index, response.Code, want, response.Body.String())
		}
	}
	if len(forwarder.inputs) != 2 {
		t.Fatalf("attempts = %d, want 2", len(forwarder.inputs))
	}
}

func TestModelQuotaCooldownWithoutModelDoesNotCooldownAccount(t *testing.T) {
	handler, _, registry := newHandlerForTest(t, &scriptedForwarder{})
	if err := registry.ReplaceCredentials([]state.CredentialEntry{{ID: 1, GroupID: 1,
		Version: 1, IdentityGeneration: 1, Status: state.CredentialStatusActive,
		Fingerprint: "fixture", EncryptedValue: "fixture"}}); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	ref, _ := registry.CredentialRef(1)
	handler.applyDecisionEffectWithBlacklistPolicy(ref, 0, health.Decision{
		Category: health.FailureCategoryRateLimited, Effect: health.EffectCooldownModel,
		CooldownUntil: now.Add(time.Hour), RuleID: "rate_limit.retry_after", QuotaCooldown: true,
	}, 429, now, 0, "")
	view := registry.Snapshot()[0]
	if !view.CooldownUntil.IsZero() || len(view.ModelCooldowns) != 0 {
		t.Fatalf("missing model created a cooldown: %#v", view)
	}
}

func TestQuotaRecoveryPreservesCustomAndRefreshCooldowns(t *testing.T) {
	for _, test := range []struct {
		name      string
		model     string
		custom    bool
		committed bool
		refresh   bool
	}{
		{name: "builtin account"},
		{name: "builtin committed model", model: "model", committed: true},
		{name: "custom unsafe account", custom: true},
		{name: "custom unsafe model", model: "model", custom: true},
		{name: "custom committed account", custom: true, committed: true},
		{name: "custom committed model", model: "model", custom: true, committed: true},
		{name: "token refresh", refresh: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			handler, _, registry := newHandlerForTest(t, &scriptedForwarder{})
			if err := registry.ReplaceCredentials([]state.CredentialEntry{{ID: 1, GroupID: 1, Version: 1, IdentityGeneration: 1,
				Status: state.CredentialStatusActive, Fingerprint: "fixture", EncryptedValue: "fixture"}}); err != nil {
				t.Fatal(err)
			}
			now := time.Now().UTC().Truncate(time.Millisecond)
			effect, scope := health.EffectCooldownCredential, execution.ErrorScopeCredential
			if test.model != "" {
				effect, scope = health.EffectCooldownModel, execution.ErrorScopeModel
			}
			ctx := health.DecisionContext{Method: http.MethodPost, Operation: execution.OperationChatCompletion}
			if test.custom {
				var err error
				ctx.ErrorRules, err = health.CompileErrorRules([]health.ErrorRule{{StatusCodes: []int{200, 429},
					Retry: health.RetryNextCandidate, Effect: effect, CooldownSeconds: 600}}, "group")
				if err != nil {
					t.Fatal(err)
				}
			}
			attempt := health.ExecutionAttempt{DispatchState: execution.DispatchMaybeSent, StatusCode: 429, Now: now,
				DownstreamCommitted: test.committed, Evidence: &execution.ErrorEvidence{Kind: execution.ErrorKindHTTP,
					StatusCode: 429, Hint: execution.FailureHintRateLimited, OriginHint: execution.ErrorOriginUpstream,
					ScopeHint: scope, ReplaySafety: execution.ReplaySafetyUnknown}}
			if test.committed {
				attempt.StatusCode, attempt.Evidence.Kind = 200, execution.ErrorKindProvider
				attempt.Evidence.ReplaySafety = execution.ReplaySafetyRejectedBeforeProcessing
			}
			if test.refresh {
				attempt.DispatchState, attempt.Evidence.Hint = execution.DispatchNotSent, execution.FailureHintRefreshUnavailable
			}
			decision := health.JudgeExecution(attempt, ctx)
			if decision.Effect != effect {
				t.Fatalf("effect = %s, want %s", decision.Effect, effect)
			}
			ref, _ := registry.CredentialRef(1)
			handler.applyDecisionEffectWithBlacklistPolicy(ref, 0, decision, attempt.StatusCode, now, 0, test.model)
			registry.RestoreObservedQuotaCooldowns(1, now.Add(time.Second).UnixMilli())
			view := registry.Snapshot()[0]
			until := view.CooldownUntil
			if test.model != "" {
				until = view.ModelCooldowns[test.model]
			}
			if wantCleared := !test.custom && !test.refresh; until.IsZero() != wantCleared {
				t.Fatalf("rule %s: cooldown cleared = %t, want %t", decision.RuleID, until.IsZero(), wantCleared)
			}
		})
	}
}
