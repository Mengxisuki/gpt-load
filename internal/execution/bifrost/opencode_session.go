package bifrost

import (
	"fmt"
	"net/http"
	"strings"

	"gpt-load/internal/channel"
	"gpt-load/internal/execution"
)

// withOpenCodeSession 在实际出站前补齐 OpenCode 的会话路由头。
// 仅处理 Go/Zen 的生成和探测请求，其他渠道及模型列表不受影响。
// 优先保留显式配置，其次复用客户端原生会话；不读取提示词猜测对话。
// 没有会话线索时复用网关生成的请求 UUID，重试保持不变。
// 该 UUID 不信任客户端请求头，不与共享渠道凭据或 AccessKey 绑定。
// 不保存聊天状态，不将随机请求标识宣称为跨请求的真实会话。
// 此头用于供应商路由和缓存，不是用户认证令牌或访问控制边界。
// 两名调用者主动传相同会话值时仍沿用原值，不额外改写客户端协议。
// 系统及分组覆盖规则已先执行，非空固定配置同样视为显式值。
// 因此管理员不能使用占位符字面量或统一常量代替真实会话标识。
func withOpenCodeSession(spec execution.AttemptSpec) (execution.AttemptSpec, error) {
	if spec.ChannelID != string(channel.OpenCodeGo) && spec.ChannelID != string(channel.OpenCodeZen) {
		return spec, nil
	}
	if spec.Operation != execution.OperationChatCompletion && spec.Operation != execution.OperationResponsesCreate && spec.Operation != execution.OperationProbe {
		return spec, nil
	}
	if strings.TrimSpace(spec.Header.Get("X-Opencode-Session")) != "" {
		return spec, nil
	}
	// 原生头的固定优先级与 Codex 现有会话拼写兼容。
	// 只复制标识，不修改原头；已提供的对话仍可复用供应商缓存。
	session := ""
	for _, name := range []string{"Session-Id", "Session_id", "Thread-Id", "X-Claude-Code-Session-Id"} {
		session = strings.TrimSpace(spec.Header.Get(name))
		if session != "" {
			break
		}
	}
	// 不使用 AttemptID，否则重试会改变会话；也不能使用全局默认值。
	// 遥测 UUID 生成失败时网关传入 untracked，不能把它发给所有请求。
	// 这种情况明确失败，避免通过固定兜底值掩盖请求隔离失效。
	if session == "" {
		session = strings.TrimSpace(spec.RequestID)
		if session == "" || session == "untracked" {
			return spec, fmt.Errorf("cannot generate upstream session without a unique request id")
		}
	}
	// AttemptSpec 的头可能被其他候选共用，修改前复制，避免重试污染。
	// nil 头同样可补齐；新字段只进入上游请求，不作为响应头暴露。
	spec.Header = spec.Header.Clone()
	if spec.Header == nil {
		spec.Header = make(http.Header)
	}
	spec.Header.Set("X-Opencode-Session", session)
	return spec, nil
}
