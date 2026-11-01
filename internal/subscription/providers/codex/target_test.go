package codex

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"

	subscriptionruntime "gpt-load/internal/subscription/runtime"
)

type targetRoundTripper func(*http.Request) (*http.Response, error)

func (f targetRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestBusinessTargetPreservesNativeEndpoints(t *testing.T) {
	driver := newCodexDriver()
	credential, err := driver.Parse([]byte(`{"type":"codex","access_token":"test-access","refresh_token":"test-refresh","account_id":"account-one"}`))
	if err != nil {
		t.Fatal(err)
	}
	for _, root := range []string{"", "https://relay.example", "https://relay.example/team-a"} {
		t.Run(root, func(t *testing.T) {
			target := subscriptionruntime.NewTarget([]byte(`{"base_url":"` + root + `"}`))
			for _, op := range []struct {
				name string
				call func(context.Context, subscriptionruntime.Credential, subscriptionruntime.Target) error
				urls []string
			}{
				{"models", func(ctx context.Context, c subscriptionruntime.Credential, target subscriptionruntime.Target) error {
					_, err := driver.DiscoverModels(ctx, c, target)
					return err
				}, []string{"https://chatgpt.com/backend-api/codex/models"}},
				{"observation", func(ctx context.Context, c subscriptionruntime.Credential, target subscriptionruntime.Target) error {
					_, err := driver.Observe(ctx, c, target)
					return err
				}, []string{"https://chatgpt.com/backend-api/wham/usage"}},
				{"refresh official", func(ctx context.Context, c subscriptionruntime.Credential, _ subscriptionruntime.Target) error {
					_, err := driver.Refresh(ctx, c)
					return err
				}, []string{"https://auth.openai.com/oauth/token"}},
				{"reset details", func(ctx context.Context, c subscriptionruntime.Credential, target subscriptionruntime.Target) error {
					credential, err := ParseCredentialJSON(c.Canonical())
					if err != nil {
						return err
					}
					root, err := target.BaseURL()
					if err != nil {
						return err
					}
					_, err = ObserveResetCredits(ctx, credential, root)
					return err
				}, []string{"https://chatgpt.com/backend-api/wham/rate-limit-reset-credits"}},
				{"reset consume", func(ctx context.Context, c subscriptionruntime.Credential, target subscriptionruntime.Target) error {
					_, err := driver.Consume(ctx, c, target, subscriptionruntime.ResetCreditRequest{ID: "test-request-id"})
					return err
				}, []string{"https://chatgpt.com/backend-api/wham/rate-limit-reset-credits/consume"}},
			} {
				t.Run(op.name, func(t *testing.T) {
					var mu sync.Mutex
					var got []string
					previous := http.DefaultTransport
					t.Cleanup(func() { http.DefaultTransport = previous })
					http.DefaultTransport = targetRoundTripper(func(r *http.Request) (*http.Response, error) {
						targetURL := *r.URL
						// 只忽略随版本变化的客户端版本参数，业务查询参数必须保留。
						query := targetURL.Query()
						query.Del("client_version")
						query.Del("entrypoint")
						targetURL.RawQuery = query.Encode()
						mu.Lock()
						got = append(got, r.Method+" "+targetURL.String())
						mu.Unlock()
						if op.name != "refresh official" && r.Header.Get("Authorization") != "Bearer test-access" {
							t.Error("business request lost its OAuth access token")
						}
						return &http.Response{StatusCode: http.StatusTeapot, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{}`)), Request: r}, nil
					})
					if err := op.call(t.Context(), credential, target); err == nil {
						t.Fatal("unexpected success from failed upstream")
					}
					var want []string
					for _, raw := range op.urls {
						u, err := url.Parse(raw)
						if err != nil {
							t.Fatal(err)
						}
						method := "GET"
						if strings.Contains(u.Path, "v1internal:") || strings.HasSuffix(u.Path, "/token") || strings.HasSuffix(u.Path, "/consume") {
							method = "POST"
						}
						if root != "" && op.name != "refresh official" {
							raw = root + u.RequestURI()
						}
						want = append(want, method+" "+raw)
					}
					slices.Sort(got)
					slices.Sort(want)
					if !reflect.DeepEqual(got, want) {
						t.Fatalf("requests = %v; want %v", got, want)
					}
				})
			}
		})
	}
}

// 主额度与卡详情独立观测：详情失败不得把未知卡数变成零。
// 测试只替换 HTTP 传输层，实际执行驱动解析与标准化。
// 三个用例分别覆盖已知零、完全未知和已知数量但详情失败。
// 空数组成功不能错误地显示成接口异常。
// 服务返回错误不能错误地显示成没有卡。
// partial 只说明计数存在，不能让用户误认到期日期也已刷新。
// HTTP 传输桩按真正业务路径区分主额度和卡片接口。
// 厂商错误正文使用空对象，不依赖某个具体错误字符串。
// 每个子测试恢复默认传输层，避免污染其他官方目标测试。
// 这里不并行改动全局 transport，确保桩只影响当前用例。
// 观测成功输出仍经过 JSON 反序列化，覆盖对外字段实际命名。
// 新鲜时间必须是独立字段，不能由 UI 在收到响应时自行补造。
// 这种局部故障不会触发自动重置，也不会消耗机会。
// 测试只证明解析与状态契约，不能推断真实账号持有多少张卡。
func TestObserveResetCreditAvailability(t *testing.T) {
	driver := newCodexDriver()
	credential, err := driver.Parse([]byte(`{"type":"codex","access_token":"test-access","refresh_token":"test-refresh","account_id":"account-one"}`))
	if err != nil {
		t.Fatal(err)
	}
	// 空数组是成功观测到零张卡，错误响应才是不可用。
	// 主响应带计数但详情失败时只承诺数量，不承诺每张到期时间。
	for _, test := range []struct {
		name, usage, details, state string
		status                      int
	}{
		{"empty known", `{"rate_limit":{}}`, `[]`, "fresh", 200},
		{"unknown", `{"rate_limit":{}}`, `{}`, "unavailable", 503},
		{"partial", `{"rate_limit":{},"rate_limit_reset_credits":{"available_count":2}}`, `{}`, "partial", 503},
	} {
		t.Run(test.name, func(t *testing.T) {
			previous := http.DefaultTransport
			t.Cleanup(func() { http.DefaultTransport = previous })
			http.DefaultTransport = targetRoundTripper(func(r *http.Request) (*http.Response, error) {
				body, status := test.usage, 200
				if strings.Contains(r.URL.Path, "rate-limit-reset-credits") {
					body, status = test.details, test.status
				}
				return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
			})
			// 不调用消费接口，任何副作用都不属于额度读取契约。
			// 读取真实归一化输出，而非直接构造对外 DTO 自证正确。
			observed, err := driver.Observe(t.Context(), credential, subscriptionruntime.NewTarget([]byte(`{}`)))
			if err != nil {
				t.Fatal(err)
			}
			var snapshot quotaSnapshot
			if err := json.Unmarshal(observed.Payload, &snapshot); err != nil {
				t.Fatal(err)
			}
			if snapshot.ResetCreditObservation == nil || snapshot.ResetCreditObservation.State != test.state {
				t.Fatalf("card state=%#v", snapshot.ResetCreditObservation)
			}
			// 新鲜卡详情必须带独立时间，避免混用主额度更新时间。
			// 失败时仍保留成功的主额度内容，允许用户识别局部故障。
			if test.state == "fresh" && snapshot.ResetCreditObservation.ObservedAtMS == nil {
				t.Fatal("missing detail observation time")
			}
		})
	}
}
