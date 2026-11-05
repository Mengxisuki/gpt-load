package bifrost

import (
	"net/http"
	"strings"

	"gpt-load/internal/channel"
	"gpt-load/internal/execution"
	"gpt-load/internal/platform/version"
)

// withUserAgent 为普通渠道补齐出站身份；订阅渠道由各自适配器处理。
// OpenCode Go 已实测拒绝 Python 默认 UA，使用网关自身身份兼容。
// 不仿冒浏览器，不建立其他客户端的推测性黑名单。
// 系统/分组配置已先应用，其显式非空 UA 优先于此兼容规则。
func withUserAgent(spec execution.AttemptSpec) execution.AttemptSpec {
	ua := strings.TrimSpace(spec.Header.Get("User-Agent"))
	// 只覆盖已验证的生成入口；模型列表及其他供应商继续原样透传。
	// 该限制属于 OpenCode HTTP 接口，不按 DeepSeek 型号扩散模型规则。
	normalize := spec.ChannelID == string(channel.OpenCodeGo) && strings.HasPrefix(ua, "Python-urllib/") &&
		(spec.Operation == execution.OperationChatCompletion || spec.Operation == execution.OperationResponsesCreate || spec.Operation == execution.OperationProbe)
	// 配置名称大小写不敏感，管理员可明确指定供应商所需身份。
	// 已配置但留空仍按既有行为补默认 UA，不新增特殊空值语义。
	for _, name := range spec.ConfiguredHeaders {
		if strings.EqualFold(name, "User-Agent") {
			normalize = false
			break
		}
	}
	if ua != "" && !normalize {
		return spec
	}
	// 每次尝试独立复制头，下一候选不能继承本渠道的适配。
	// 原始请求仍保留真实客户端 UA，诊断时可区分入口与出站身份。
	spec.Header = spec.Header.Clone()
	if spec.Header == nil {
		spec.Header = make(http.Header)
	}
	// 没有有效 UA 时使用项目身份，包括规则明确删除或留空的情况。
	// Python 默认身份同样复用已有默认值，版本号来自当前构建。
	spec.Header.Set("User-Agent", "GPT-Load/"+version.Version)
	return spec
}
