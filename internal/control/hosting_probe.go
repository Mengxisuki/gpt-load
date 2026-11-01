package control

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"gpt-load/internal/channel"
	"gpt-load/internal/execution"
	app_errors "gpt-load/internal/platform/errors"
	"gpt-load/internal/protocol"
)

// TestHostingAccount 仅测试已核验归属的 Codex 私有账号，不进入公共调度池。
// 通用 TestGroupCredential 仍拒绝订阅组，因为 Codex 未声明 OperationProbe。
// 此处使用正常 ResponsesCreate，凭据准备与刷新继续由订阅执行器负责。
// 测试可能消耗少量原厂额度；成功不修改账号启停或凭据健康状态。
// owner 必须来自请求断言的核验结果，不能使用浏览器直接声明的归属。
// 返回值只描述本次模型和协议的结果，不保证其他模型也能使用。
// 执行中的请求沿用既有停用语义：暂停禁止新请求，不强制中断在途调用。
func (s *Service) TestHostingAccount(ctx context.Context, owner, account string, request CredentialProbeRequest) (CredentialProbeResponse, error) {
	// 与其他挂靠操作相同，归属判断以持久绑定为准，不接受任意组或凭据 ID。
	binding, err := s.lookupHosting(ctx, owner, account)
	if err != nil {
		return CredentialProbeResponse{}, err
	}
	if binding.Status == "deleted" {
		return CredentialProbeResponse{}, app_errors.ErrResourceNotFound
	}
	// 首次账号需先保存内部 Key 并激活；暂停账号不能借测试入口发起推理。
	// New API 的待验证状态与此处的执行层 active 不同，前者仍可请求测试。
	// 此检查不调用激活接口，避免一次诊断改变用户原先的启停选择。
	if binding.Status != "active" {
		return CredentialProbeResponse{}, app_errors.ErrInvalidCredentialState
	}
	// 首期契约只支持显式模型和 Responses，缺失字段不能偷偷选择默认模型。
	// null 与字段缺失分别由 optionalField 保留，均不代表使用账号默认值。
	// 输入模型沿用既有长度和字符约束，避免另造一套验证规则。
	if !request.Model.Set || request.Model.Null || !request.Protocol.Set || request.Protocol.Null || request.Protocol.Value != protocol.OpenAIResponses {
		return CredentialProbeResponse{}, app_errors.ErrValidation
	}
	model, err := normalizeValidationModel(request.Model.Value)
	if err != nil {
		return CredentialProbeResponse{}, err
	}
	// 复用现有读取边界，确认凭据仍属于绑定的订阅组。
	// 绑定中已固定唯一凭据，不能从组中重新挑选“更健康”的其他账号。
	// 数据库缺失或读取失败直接返回，不能降级到公共组执行。
	row, credential, err := s.loadSubscriptionCredentialTarget(ctx, binding.GroupID, binding.CredentialID)
	if err != nil {
		return CredentialProbeResponse{}, err
	}
	snapshot := s.manager.Current()
	if s.executor == nil || snapshot == nil {
		return CredentialProbeResponse{}, app_errors.ErrInternalServer
	}
	group, exists := snapshot.Groups[binding.GroupID]
	if !exists || group.ChannelID != channel.Codex {
		return CredentialProbeResponse{}, app_errors.ErrInvalidCredentialState
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
		return CredentialProbeResponse{}, app_errors.ErrValidation
	}
	// 解密复用订阅凭据规范化，不能将 OAuth 令牌当作普通 api_key 使用。
	// 明文只进入执行规格，不能加入返回结构、业务错误或审计日志。
	// 凭据副本使用后清理，数据库继续保留加密内容。
	canonical, _, err := s.decodeCredential(row, credential)
	if err != nil {
		return CredentialProbeResponse{}, err
	}
	defer clear(canonical)
	// 使用账号现有代理策略，保持测试和正常推理的网络出口一致。
	// 凭据级代理优先级由既有函数解析，测试不引入独立出口配置。
	// 代理指纹随规格传递，订阅准备能够识别网络上下文的变化。
	network, err := s.credentialNetworkContext(ctx, s.db, row, credential)
	if err != nil {
		return CredentialProbeResponse{}, err
	}
	requestID, err := s.newExecutionID()
	if err != nil {
		return CredentialProbeResponse{}, app_errors.ErrInternalServer
	}
	attemptID, err := s.newExecutionID()
	if err != nil {
		return CredentialProbeResponse{}, app_errors.ErrInternalServer
	}
	// 固定短输入，不接受浏览器提供的任意 prompt、工具或会话标识。
	// 明确关闭服务端存储，测试不创建需要后续管理的会话。
	// Codex 对生成参数存在限制，因此只使用原生 Responses 的必要字段。
	body, err := json.Marshal(map[string]any{"model": upstreamModel, "input": "Reply only OK.", "store": false})
	if err != nil {
		return CredentialProbeResponse{}, app_errors.ErrInternalServer
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
		return CredentialProbeResponse{}, app_errors.ErrInternalServer
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
		return CredentialProbeResponse{}, err
	}
	// 使用稳定的安全分类；不回传厂商正文、身份、令牌或内部恢复证明。
	// failed 与 inconclusive 都是业务结果，只有明确通过才返回 passed。
	// CanRestore 和 RestoreProof 保持零值，用户测试不能获得管理恢复权限。
	outcome, reason := classifyCredentialProbeResult(result)
	return CredentialProbeResponse{Outcome: outcome, Model: model, Protocol: protocol.OpenAIResponses,
		LatencyMS: time.Since(started).Milliseconds(), Reason: reason, TestedAtMS: s.now().UTC().UnixMilli()}, nil
}
