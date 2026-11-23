package codex

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	cpaembedded "github.com/router-for-me/CLIProxyAPI/v8/gptload-embedded/embedded"
)

func TestWSSessionIsExplicitAndHTTPExecutorIsUnchanged(t *testing.T) {
	session, err := NewWSSession(WSSessionOptions{
		CredentialID: "test", Credential: Credential{Type: Provider, AccessToken: "test-access", RefreshToken: "test-refresh", AccountID: "test-account"},
		ProxyURL: "direct",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := session.Close(); err != nil {
			t.Error(err)
		}
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	result, err := session.ExecuteTurn(ctx, json.RawMessage(`{"model":"gpt-5","input":"hello"}`), nil)
	var failure *WSError
	if !errors.Is(err, context.Canceled) || !errors.As(err, &failure) || result.DispatchState != "not_sent" {
		t.Fatalf("cancellation contract lost: %v", err)
	}
	httpExecutor := NewExecutor().(*executor)
	if httpExecutor.bridge == nil {
		t.Fatal("existing HTTP executor changed")
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	_, err = session.ExecuteTurn(context.Background(), json.RawMessage(`{"model":"gpt-5","input":"hello"}`), nil)
	if !errors.As(err, &failure) || failure.Code != "session_closed" {
		t.Fatalf("closed session reused: %v", err)
	}
}

func TestWSErrorFromBridgePreservesSafeRateLimitMetadata(t *testing.T) {
	err := wsErrorFromBridge(&cpaembedded.CodexWSError{
		Code: "upstream_error", UpstreamType: "usage_limit_reached",
		HTTPStatus: 429, RetryAfter: 2 * time.Hour, DispatchState: WSMaybeSent,
	})
	var failure *WSError
	if !errors.As(err, &failure) || failure.UpstreamType != "usage_limit_reached" ||
		failure.HTTPStatus != 429 || failure.RetryAfter != 2*time.Hour {
		t.Fatalf("safe WebSocket rate-limit metadata was lost: %+v", failure)
	}
}

// 桥接层必须保留 SDK 给出的瞬时容量证据。
// 该证据允许现有策略区分容量与额度耗尽。
// 同时保留可能已发送的状态，不自动放宽重放条件。
// 输入仅含安全分类字段，不依赖上游错误正文。
// 用真实转换函数验证边界映射，避免手工复制结果自证。
func TestWSErrorFromBridgePreservesTransientCapacity(t *testing.T) {
	err := wsErrorFromBridge(&cpaembedded.CodexWSError{
		Code: "upstream_error", UpstreamType: "rate_limit_error", HTTPStatus: 429,
		TransientRateLimit: true, DispatchState: WSMaybeSent,
	})
	var failure *WSError
	if !errors.As(err, &failure) || !failure.TransientRateLimit || failure.DispatchState != WSMaybeSent {
		t.Fatalf("WebSocket capacity evidence was lost: %+v", failure)
	}
}
