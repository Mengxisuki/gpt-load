package bifrost

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"gpt-load/internal/channel"
	"gpt-load/internal/execution"
	"gpt-load/internal/platform/version"
	"gpt-load/internal/protocol"
)

// TestOpenCodePythonUserAgent 验证现场 403 的触发 UA 在真实出站处被替换。
// 只替换 OpenCode Go 客户端的 Python 默认身份，保留管理员明确配置。
// 普通及流式共用规则；服务端同时检查调用次数与源头对象隔离。
// 全部请求发送到回环服务器，不消耗供应商额度或依赖账户状态。
// 用例中的 Python 版本取自实际失败请求，不假定全部脚本 UA 都被拒绝。
// 期望默认身份复用项目版本变量，与部署二进制版本保持一致。
// 不在测试中读取或覆盖全局版本，因此并行构建不会产生共享修改。
func TestOpenCodePythonUserAgent(t *testing.T) {
	for _, test := range []struct {
		name, agent, want string
		configured        []string
	}{
		// 默认 Python UA 是已复现的访问拒绝条件。
		// 新身份明确标识 GPT-Load，不伪装成浏览器或官方桌面应用。
		{name: "python", agent: "Python-urllib/3.11", want: "GPT-Load/" + version.Version},
		// 其他非空客户端身份照常透传，不建立更多未验证 UA 黑名单。
		// 显式的组/系统覆盖优先，大小写无关地识别配置头名称。
		{name: "client", agent: "client/1.0", want: "client/1.0"},
		{name: "configured_python", agent: "Python-urllib/3.11", configured: []string{"user-agent"}, want: "Python-urllib/3.11"},
		// 管理员指定同样的 Python UA 时仍保留，这是显式选择的优先级。
		// 保留该值不承诺上游接受它，避免把头策略和服务可用性混为一谈。
	} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", test.name, stream), func(t *testing.T) {
				seen := make(chan string, 1)
				// 每个案例独立启动 runtime 和服务器，不共享上次请求的头缓存。
				// 缓冲只容纳本案例一次出站，便于发现意外的额外请求。
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					seen <- r.Header.Get("User-Agent")
					// 当请求仍带现场的默认 UA 时模拟原始拒绝。
					// 管理员刻意配置相同值的案例仅检查透传，不改变优先级合同。
					if test.name == "python" && r.Header.Get("User-Agent") == "Python-urllib/3.11" {
						// 原始拒绝为非 JSON 文本，模拟时保留这一错误形状。
						// 如执行层错误解析发生变化，测试仍会看到非成功状态。
						w.WriteHeader(403)
						io.WriteString(w, "error code: 1010")
						return
					}
					if stream {
						// 提供实际文本和 Chat 终态，避免只启动流便算成功。
						w.Header().Set("Content-Type", "text/event-stream")
						io.WriteString(w, "data: {\"id\":\"chat_1\",\"object\":\"chat.completion.chunk\",\"model\":\"deepseek-v4.1-flash\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"OK\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
						return
					}
					w.Header().Set("Content-Type", "application/json")
					io.WriteString(w, `{"id":"chat_1","object":"chat.completion","model":"deepseek-v4.1-flash","choices":[{"index":0,"message":{"role":"assistant","content":"OK"},"finish_reason":"stop"}]}`)
				}))
				defer server.Close()
				manager, target := newOpenCodeManagerForTest(t, channel.OpenCodeGo, server.URL)
				// 固定 Flash 型号与现场 Chat 路径，防止意外走其他 provider。
				// 默认会话标识由已有 fixture 提供，本次只验证 UA 差异。
				spec := utilitySpec(channel.OpenCodeGo, protocol.OpenAICompletions, execution.OperationChatCompletion, http.MethodPost, "/v1/chat/completions", []byte(`{"model":"deepseek-v4.1-flash","messages":[{"role":"user","content":"Reply only OK."}]}`))
				spec.TargetConfig, spec.ClientModel, spec.UpstreamModel = target.TargetConfig, "deepseek-v4.1-flash", "deepseek-v4.1-flash"
				spec.Header.Set("User-Agent", test.agent)
				spec.ConfiguredHeaders = test.configured
				// 原始 HTTP Header 无法区分管理员覆盖与客户端输入。
				// ConfiguredHeaders 是已有显式覆盖来源，不新增状态猜测。
				spec = freezeTestAttempt(spec)
				original := spec.Header.Clone()
				// 快照保留全部头；身份适配不能删除认证之外的会话或其他字段。
				// 检查最终状态而非仅执行了 HTTP 请求，错误不能被吞掉。
				// 两种调用都读取实际出站头，避免只测试辅助函数。
				if stream {
					var output strings.Builder
					result := manager.ExecuteStream(t.Context(), spec, func(e execution.StreamEvent) error { output.Write(e.Data); return nil })
					if result.Error != nil || !strings.Contains(output.String(), "[DONE]") {
						t.Fatalf("stream failed: %+v", result.Error)
					}
				} else {
					result := manager.Execute(t.Context(), spec)
					if result.Error != nil || result.StatusCode != 200 {
						t.Fatalf("request failed: %+v", result.Error)
					}
				}
				if got := <-seen; got != test.want {
					// 成功回包并不足以证明正确 UA，最后比较网络实际捕获值。
					t.Fatalf("UA = %q, want %q", got, test.want)
				}
				// 冻结 spec 可能用于下一个候选，源头必须完全不变。
				// Header.Clone 防止专有规则扩散到其他供应商。
				if !reflect.DeepEqual(spec.Header, original) {
					t.Fatal("source headers changed")
				}
			})
		}
	}
}

// TestOpenCodePythonUserAgentBoundary 保护不适用的渠道、操作与管理员设置。
// 只检查规则选择；真正 HTTP 行为由上面的普通/流式案例验证。
// 参数来自冻结尝试，未新增组设置、模型配置或可变全局状态。
// 此边界表故意使用相同 Python UA，仅改变渠道和操作。
// 如果排除条件失效，相同输入就会出现可观察的身份变化。
// 与网络矩阵分开，避免为每个不会发请求的分支重复启动服务器。
func TestOpenCodePythonUserAgentBoundary(t *testing.T) {
	for _, test := range []struct {
		name, channel string
		operation     execution.Operation
		want          string
	}{
		// 同一 Go 渠道的生成路径都使用同一个上游 HTTP 身份。
		// 探测也是实际生成请求，不能继续保留导致误判不可用的 UA。
		{"responses", "opencode_go", execution.OperationResponsesCreate, "GPT-Load/" + version.Version},
		{"probe", "opencode_go", execution.OperationProbe, "GPT-Load/" + version.Version},
		// 管理模型列表不属于本轮已复现的生成接口。
		// Zen 及其他供应商不继承 Go 的访问规则。
		{"models", "opencode_go", execution.OperationListModels, "Python-urllib/3.11"},
		{"zen", "opencode_zen", execution.OperationChatCompletion, "Python-urllib/3.11"},
		{"other", "openai", execution.OperationChatCompletion, "Python-urllib/3.11"},
	} {
		t.Run(test.name, func(t *testing.T) {
			// 显式 Header 与 nil 默认头是不同路径，通用默认头另有既有回归。
			spec := execution.AttemptSpec{ChannelID: test.channel, Operation: test.operation, Header: http.Header{"User-Agent": {"Python-urllib/3.11"}}}
			got := withUserAgent(spec)
			if got.Header.Get("User-Agent") != test.want {
				t.Fatalf("UA = %q", got.Header.Get("User-Agent"))
			}
		})
	}
}
