package control

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"gpt-load/internal/channel"
	"gpt-load/internal/execution"
	app_errors "gpt-load/internal/platform/errors"
	"gpt-load/internal/protocol"
)

// HostingTestResponse 仅扩展本人账号测试，普通管理探测仍使用原来的结果结构。
// 提示词与本次实际请求共用来源；回复只含 assistant 消息的 output_text。
// 这些临时诊断内容不写入账号、凭据或审计记录。
// 公共 Probe 字段沿用原兼容契约，恢复证明始终保持空值。
type HostingTestResponse struct {
	CredentialProbeResponse
	Prompt       string `json:"prompt"`
	ResponseText string `json:"response_text"`
}

// TestHostingAccount 仅测试已核验归属的 Codex 私有账号，不进入公共调度池。
// 通用 TestGroupCredential 仍拒绝订阅组，因为 Codex 未声明 OperationProbe。
// 此处使用正常 ResponsesCreate，凭据准备与刷新继续由订阅执行器负责。
// 测试可能消耗少量原厂额度；成功不修改账号启停或凭据健康状态。
// owner 必须来自请求断言的核验结果，不能使用浏览器直接声明的归属。
// 返回值只描述本次模型和协议的结果，不保证其他模型也能使用。
// 执行中的请求沿用既有停用语义：暂停禁止新请求，不强制中断在途调用。
func (s *Service) TestHostingAccount(ctx context.Context, owner, account string, request CredentialProbeRequest) (HostingTestResponse, error) {
	// 与其他挂靠操作相同，归属判断以持久绑定为准，不接受任意组或凭据 ID。
	binding, err := s.lookupHosting(ctx, owner, account)
	if err != nil {
		return HostingTestResponse{}, err
	}
	if binding.Status == "deleted" {
		return HostingTestResponse{}, app_errors.ErrResourceNotFound
	}
	// 首次账号需先保存内部 Key 并激活；暂停账号不能借测试入口发起推理。
	// New API 的待验证状态与此处的执行层 active 不同，前者仍可请求测试。
	// 此检查不调用激活接口，避免一次诊断改变用户原先的启停选择。
	if binding.Status != "active" {
		return HostingTestResponse{}, app_errors.ErrInvalidCredentialState
	}
	// 首期契约只支持显式模型和 Responses，缺失字段不能偷偷选择默认模型。
	// null 与字段缺失分别由 optionalField 保留，均不代表使用账号默认值。
	// 输入模型沿用既有长度和字符约束，避免另造一套验证规则。
	if !request.Model.Set || request.Model.Null || !request.Protocol.Set || request.Protocol.Null || request.Protocol.Value != protocol.OpenAIResponses {
		return HostingTestResponse{}, app_errors.ErrValidation
	}
	model, err := normalizeValidationModel(request.Model.Value)
	if err != nil {
		return HostingTestResponse{}, err
	}
	// 复用现有读取边界，确认凭据仍属于绑定的订阅组。
	// 绑定中已固定唯一凭据，不能从组中重新挑选“更健康”的其他账号。
	// 数据库缺失或读取失败直接返回，不能降级到公共组执行。
	row, credential, err := s.loadSubscriptionCredentialTarget(ctx, binding.GroupID, binding.CredentialID)
	if err != nil {
		return HostingTestResponse{}, err
	}
	snapshot := s.manager.Current()
	if s.executor == nil || snapshot == nil {
		return HostingTestResponse{}, app_errors.ErrInternalServer
	}
	group, exists := snapshot.Groups[binding.GroupID]
	if !exists || group.ChannelID != channel.Codex {
		return HostingTestResponse{}, app_errors.ErrInvalidCredentialState
	}
	// 模型目录限制用户可测试的范围；目录命中本身不算连接测试成功。
	// 私有组的模型来自该账号授权时的原厂发现，不添加公共组模型作为回退。
	// 对外模型名与原厂 ID 分开保存，别名不能直接作为厂商模型提交。
	// 路由能力再校验一次所选原厂模型，避免目录与执行能力不一致。
	upstreamModel := ""
	for _, candidate := range group.Models {
		if candidate.ExternalName() == model {
			upstreamModel = candidate.ID
			break
		}
	}
	mode, supported := group.ResolvedTarget.ModeForModel(protocol.OpenAIResponses, execution.OperationResponsesCreate, upstreamModel)
	if upstreamModel == "" || !supported {
		return HostingTestResponse{}, app_errors.ErrValidation
	}
	// 解密复用订阅凭据规范化，不能将 OAuth 令牌当作普通 api_key 使用。
	// 明文只进入执行规格，不能加入返回结构、业务错误或审计日志。
	// 凭据副本使用后清理，数据库继续保留加密内容。
	canonical, _, err := s.decodeCredential(row, credential)
	if err != nil {
		return HostingTestResponse{}, err
	}
	defer clear(canonical)
	// 使用账号现有代理策略，保持测试和正常推理的网络出口一致。
	// 凭据级代理优先级由既有函数解析，测试不引入独立出口配置。
	// 代理指纹随规格传递，订阅准备能够识别网络上下文的变化。
	network, err := s.credentialNetworkContext(ctx, s.db, row, credential)
	if err != nil {
		return HostingTestResponse{}, err
	}
	requestID, err := s.newExecutionID()
	if err != nil {
		return HostingTestResponse{}, app_errors.ErrInternalServer
	}
	attemptID, err := s.newExecutionID()
	if err != nil {
		return HostingTestResponse{}, app_errors.ErrInternalServer
	}
	// 固定短输入，不接受浏览器提供的任意 prompt、工具或会话标识。
	// 明确关闭服务端存储，测试不创建需要后续管理的会话。
	// Codex 对生成参数存在限制，因此只使用原生 Responses 的必要字段。
	prompt := "Reply only OK."
	body, err := json.Marshal(map[string]any{"model": upstreamModel, "input": prompt, "store": false})
	if err != nil {
		return HostingTestResponse{}, app_errors.ErrInternalServer
	}
	// 保留实际凭据版本和身份代数，执行器会校验刷新期间是否已经重授权。
	// 不设置 ForceCredentialRefresh，测试不能隐式恢复已失效的授权。
	// 请求和尝试分别生成标识，沿用执行器对一次调用与一次尝试的契约。
	// HeaderRules 保留管理员配置，但订阅授权头仍由驱动从 OAuth 凭据建立。
	// 不使用普通 API-Key 的请求头插值，也不向调用方暴露原厂令牌。
	spec := execution.NewAttemptSpec(execution.AttemptSpec{
		RequestID: requestID, AttemptID: attemptID, Sequence: 1,
		ChannelID: string(group.ChannelID), RouteMode: execution.RouteMode(mode),
		ClientProtocol: protocol.OpenAIResponses, Operation: execution.OperationResponsesCreate,
		ClientModel: model, UpstreamModel: upstreamModel, Method: http.MethodPost, Path: "/v1/responses",
		Body: body, Header: applyControlHeaderRules(group.HeaderRules, ""), ConfiguredHeaders: group.HeaderRules.ConfiguredNames(),
		TargetConfig: group.ResolvedTarget.TargetConfig, Timeouts: executionTimeouts(group.Timeouts),
		Credential: execution.NewCredentialSnapshot(credential.ID, groupCollectionCredentialVersion(credential.SecretVersion), groupCollectionCredentialIdentity(credential.IdentityFingerprint, row), canonical),
		Proxy:      network.Proxy, ProxyFingerprint: network.Fingerprint,
	})
	// 规格不完整属于实现错误，不能将未发送请求报告为账号测试成功。
	if err := spec.Validate(); err != nil {
		return HostingTestResponse{}, app_errors.ErrInternalServer
	}
	// 测试只进行一次执行，不换账号、不自动重放超时或限流请求。
	// 超时沿用订阅控制动作上限，并保留正常分组的单次执行超时。
	// 不在网络调用期间持有配置写锁，避免一次慢测试阻塞其他账号管理。
	// 调用方取消需直接返回取消错误，不能用已收到的部分结果伪造成功。
	testCtx, cancel := context.WithTimeout(ctx, defaultSubscriptionControlTimeout)
	defer cancel()
	started := time.Now()
	result := s.executor.Execute(testCtx, spec)
	if err := ctx.Err(); err != nil {
		return HostingTestResponse{}, err
	}
	// 使用稳定的安全分类；不回传厂商原始正文、身份、令牌或内部恢复证明。
	// failed 与 inconclusive 都是业务结果，只有明确通过才返回 passed。
	// CanRestore 和 RestoreProof 保持零值，用户测试不能获得管理恢复权限。
	outcome, reason := classifyCredentialProbeResult(result)
	responseText := ""
	if outcome == CredentialProbeOutcomePassed {
		// HTTP 成功仅代表传输完成，还需确认响应完成且确有可展示的模型文本。
		// 错误或空回复不激活账号；不会为补齐结果自动再次调用原厂。
		responseText = hostingResponseText(result.Body)
		if responseText == "" {
			outcome, reason = credentialProbeOutcomeWithReason(CredentialProbeOutcomeInconclusive, CredentialProbeReasonIncompatible)
		}
	}
	return HostingTestResponse{CredentialProbeResponse: CredentialProbeResponse{
		Outcome: outcome, Model: model, Protocol: protocol.OpenAIResponses,
		LatencyMS: time.Since(started).Milliseconds(), Reason: reason, TestedAtMS: s.now().UTC().UnixMilli(),
	}, Prompt: prompt, ResponseText: responseText}, nil
}

// hostingResponseText 从执行器聚合的 Responses JSON 中提取用户可见文本。
// 这里只接受完成态，不将中途断流、工具输出或推理内容冒充模型回复。
// JSON 解析失败由调用方报告不兼容，不能把原始错误正文作为展示回退。
// 字段白名单丢弃元数据、会话标识及其他厂商扩展字段。
// Codex 的 SSE 在执行器中已聚合成此结构，控制层不重复实现流协议。
// 本函数不解析 usage，也不影响计费或模型目录能力。
func hostingResponseText(body []byte) string {
	var response struct {
		Status string `json:"status"`
		Output []struct {
			Type    string `json:"type"`
			Role    string `json:"role"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"output"`
	}
	// 执行器只做协议聚合，不保证每个 2xx 正文都符合完成响应结构。
	// 缺少 status 的普通 JSON 也不能作为已完成推理的凭据。
	if json.Unmarshal(body, &response) != nil || response.Status != "completed" {
		return ""
	}
	// 一个响应可含多条消息和多个文本片段，不能只读取 output[0]。
	// 不自行插入分隔符，保留厂商分片拼合后的原始模型文本。
	var text strings.Builder
	for _, item := range response.Output {
		// 消息类型和角色需同时匹配，防止把工具调用参数当自然语言回复。
		if item.Type != "message" || item.Role != "assistant" {
			continue
		}
		for _, content := range item.Content {
			// 保留分段原文次序；不渲染 HTML，也不拼接其他协议中的任意字符串。
			if content.Type == "output_text" {
				text.WriteString(content.Text)
			}
		}
	}
	// 全空白回复没有可展示内容，其余文本保留原始换行和空格。
	// 不强制回复等于 OK，模型措辞偏差不代表连接不通。
	// 拒绝、空内容和不完整结果不会触发第二次生成来补齐文本。
	if strings.TrimSpace(text.String()) == "" {
		return ""
	}
	return text.String()
}
