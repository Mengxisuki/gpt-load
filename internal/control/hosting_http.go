package control

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	app_errors "gpt-load/internal/platform/errors"
	"gpt-load/internal/platform/httproute"
	"gpt-load/internal/platform/response"
	"gpt-load/internal/storage/models"
)

// hostingRoutes 仅由挂靠专用模块注册，普通管理员认证不能授权这些动作。
// owner_ref 必须匹配 New API 针对本次请求核验的用户，不信任调用方自报。
// 每个动作显式声明方法和路径，不提供任意后台 API 转发能力。
// 授权、建号和消费继续使用既有持久化操作标识保证幂等。
// 只有同机可信后端持有断言和内部 Key，不把它们发送到浏览器。
// 查询不隐式换码、刷新或消费；恢复动作仍要求显式原操作 ID。
// 数据面协议与每账号受限 Key 不受本次认证拆分影响。
// 跨服务失败必须显式报告，不能改用公共资源或管理员密钥。
// 变更审计只记录路由和不透明资源引用，不保留授权回调正文。
// 成功信封仍沿用 code/data，调用方必须检查业务码。
// 创建 pending 后须由 New API 持久保存内部 Key，再单独激活。
// 超时后的查询与重试沿用原幂等标识，不自动新建操作。
func (s *Server) hostingRoutes() []httproute.Route {
	routes := []httproute.Route{
		controlRoute("hosting.authorizations.create", http.MethodPost, "/hosting/v1/authorizations", s.handleHostingBegin),
		controlRoute("hosting.authorizations.get", http.MethodGet, "/hosting/v1/authorizations/:stage_id", s.handleHostingStage),
		controlRoute("hosting.authorizations.cancel", http.MethodDelete, "/hosting/v1/authorizations/:stage_id", s.handleHostingStage),
		controlRoute("hosting.authorizations.callback", http.MethodPost, "/hosting/v1/authorizations/:stage_id/callback", s.handleHostingStage),
		controlRoute("hosting.accounts.create", http.MethodPost, "/hosting/v1/accounts", s.handleHostingCreate),
		controlRoute("hosting.accounts.list", http.MethodGet, "/hosting/v1/accounts", s.handleHostingList),
		controlRoute("hosting.accounts.get", http.MethodGet, "/hosting/v1/accounts/:account_ref", s.handleHostingAccount),
		controlRoute("hosting.accounts.update", http.MethodPatch, "/hosting/v1/accounts/:account_ref", s.handleHostingAccount),
		controlRoute("hosting.accounts.delete", http.MethodDelete, "/hosting/v1/accounts/:account_ref", s.handleHostingAccount),
		controlRoute("hosting.accounts.activate", http.MethodPost, "/hosting/v1/accounts/:account_ref/activate", s.handleHostingAccount),
		controlRoute("hosting.accounts.models", http.MethodGet, "/hosting/v1/accounts/:account_ref/models", s.handleHostingAccount),
		controlRoute("hosting.accounts.observation.get", http.MethodGet, "/hosting/v1/accounts/:account_ref/observation", s.handleHostingAccount),
		controlRoute("hosting.accounts.observation", http.MethodPost, "/hosting/v1/accounts/:account_ref/observation-refresh", s.handleHostingAccount),
		controlRoute("hosting.accounts.test", http.MethodPost, "/hosting/v1/accounts/:account_ref/test", s.handleHostingAccount),
		controlRoute("hosting.accounts.reset", http.MethodPost, "/hosting/v1/accounts/:account_ref/reset-credits/consume", s.handleHostingAccount),
		controlRoute("hosting.accounts.reset-status", http.MethodGet, "/hosting/v1/accounts/:account_ref/reset-operations/:operation_id", s.handleHostingAccount),
	}
	// 复用控制面的脱敏审计，仅记录路由名和不透明资源引用。
	// 不将请求体、OAuth 回调、内部 Key 或上游错误正文写入日志。
	for i := range routes {
		if routes[i].Methods[0] == http.MethodGet {
			continue
		}
		routes[i].Handlers = append(gin.HandlersChain{s.auditMutation(newMutationDescriptor(
			routes[i].Name, "hosting", func(c *gin.Context) string {
				if ref := c.Param("account_ref"); validHostingRef(ref) {
					return "hosting:" + ref
				}
				return "hosting:authorization"
			},
		))}, routes[i].Handlers...)
	}
	return routes
}

// 请求体严格限长且拒绝未知字段，防止误把上游秘密、地址或归属覆盖项带入。
// 所有读取至多 16 KiB，完整回调 URL 仅短暂存在于内存且从不记录。
// 不使用宽松绑定，拼错字段必须报错而非被忽略。
// 只允许一个 JSON 值，尾随对象不能改变请求语义。
// 限制针对整个请求体，不依赖客户端 Content-Length。
// 错误响应只说明 JSON 不合法，不返回含秘密的原输入。
// 空体是否允许由动作决定，建号和回调均要求对象。
// 此处不做业务归属判断，业务入口仍校验引用与操作标识。
// 请求解析完成后不保留原始请求体用于审计。
func bindHostingJSON(c *gin.Context, target any) error {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 16*1024)
	decoder := json.NewDecoder(c.Request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return app_errors.ErrInvalidJSON
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return app_errors.ErrInvalidJSON
	}
	return nil
}

// 成功和错误统一禁止缓存，返回数据仍遵循原有管理 API 信封。
// 不能依赖 HTTP 200 判断业务成功；调用方应检查 code。
// 成功码沿用本项目约定，不能套用其他服务的 success 字段。
// 错误映射复用公共服务错误处理，保持状态码和错误码一致。
// no-store 同时应用于成功和失败，防止代理缓存授权摘要。
// 不在错误正文中拼接 OwnerRef、邮箱、原厂响应或回调地址。
// 审计中间件从实际响应获得成败，不由 handler 预先报告成功。
func hostingReply(c *gin.Context, data any, err error) {
	setSecretResponseHeaders(c)
	if err != nil {
		writeServiceError(c, "hosting", err)
		return
	}
	response.SuccessI18n(c, "common.success", data)
}

// 发起授权只接受明确用途，任何同操作 ID 的参数变更都会冲突。
// 创建响应只返回授权地址和 Stage 摘要，不包含 PKCE verifier。
// 请求中的 owner_ref 必须等于已核验的当前用户。
// Oak 需用当前登录用户生成它，不能透传用户随意填写的值。
// 浏览器最终只获得必要的授权 URL、时效与 Stage 状态。
// 响应丢失后同 UUID 重试仍返回同一授权过程。
// 不能将返回的 localhost 回调误认为 Oak 的公网回调。
// 解析后的 owner 必须匹配上下文，不能因为签名有效就信任正文里的身份。
// 匹配后仍显式采用上下文身份，避免未来增加字段转换时引入第二来源。
// 不匹配时在调用授权服务之前终止，不能留下跨用户 Stage。
// New API 的核验失败已经在专用认证链关闭，此处只处理业务字段一致性。
// 不读取普通管理 principal；管理员身份不代表可替任意用户授权。
// 现有 operation_id 继续决定重放语义，断言短期有效不替代持久幂等。
func (s *Server) handleHostingBegin(c *gin.Context) {
	var request HostingAuthorizationRequest
	if err := bindHostingJSON(c, &request); err != nil {
		hostingReply(c, nil, err)
		return
	}
	if request.OwnerRef != c.GetString(hostingOwnerContextKey) {
		hostingReply(c, nil, app_errors.ErrUnauthorized)
		return
	}
	request.OwnerRef = c.GetString(hostingOwnerContextKey)
	result, err := s.service.BeginHostingAuthorization(c.Request.Context(), request)
	hostingReply(c, result, err)
}

// 轮询、取消及回调先查询归属，避免裸 Stage ID 成为接管凭证。
// POST 回调只解析输入，不会向用户提供的 URL 发请求。
// 先验证归属，再区分读取、取消和提交回调。
// 状态查询不要求 OAuth 结果已 ready，允许展示失败与过期。
// 回调只交给既有授权状态机校验 state、时效和单次消费。
// 不得把用户提交的回调 URL 当作抓取目标，避免任意 URL 请求。
// 取消终止的是该授权，不会删除已经挂靠的旧账号。
// 重新授权失败时原账号资源继续保留，可再次开始授权。
// 完整回调含一次性授权码，不记录到普通操作日志。
// 普通管理 Stage 接口仍限管理员，用户不能绕过 Oak 归属入口。
// Stage查找和后续动作共享同一个核验主体，不重新读取未受信query。
func (s *Server) handleHostingStage(c *gin.Context) {
	ctx := c.Request.Context()
	stage := c.Param("stage_id")
	if _, err := s.service.hostingAuthorization(ctx, c.GetString(hostingOwnerContextKey), stage); err != nil {
		hostingReply(c, nil, err)
		return
	}
	switch c.Request.Method {
	case http.MethodGet:
		result, err := s.service.GetCredentialStage(ctx, stage)
		hostingReply(c, result, err)
	case http.MethodDelete:
		err := s.service.CancelCredentialStage(ctx, stage)
		hostingReply(c, gin.H{"status": "cancelled"}, err)
	case http.MethodPost:
		var request struct {
			CallbackURL string `json:"callback_url"`
		}
		if err := bindHostingJSON(c, &request); err != nil {
			hostingReply(c, nil, err)
			return
		}
		result, err := s.service.CompleteCredentialAuthorizationCallback(ctx, stage, request.CallbackURL)
		hostingReply(c, result, err)
	}
}

// 建号响应可能含内部 Key，只有受信后端消费，不能直接转发给用户。
// pending 与 activate 分离保证 Oak 落库失败时账号不会进入数据面。
// 创建使用与授权一致的 operation_id，限制跨用途引用。
// 服务层负责单事务创建，HTTP 层不拆成多个可见管理请求。
// 内部 Key 只在此响应出现，Oak 需要加密持久保存。
// 普通账号列表和详情不提供内部 Key 的恢复入口。
// 本请求不接受测试模型或原厂账号身份的用户声明。
// 成功表示资源建立成功，不表示真实 Codex 推理已经验收。
// body归属一致性在服务调用前检查，失败不能触发建组或签发内部Key。
// 绑定服务仍校验同一用户的Stage，不因身份核验成功而跳过资源归属。
// 主体身份取自核验上下文，正文只是与既有请求DTO保持一致的声明。
// 不向调用方返回其他用户是否存在，不匹配统一认证失败。
// 现有同身份独占与pending状态规则保持不变，不通过认证改造重新授权。
// 首次响应和幂等响应均经过同一认证边界，不给恢复路径额外豁免。
func (s *Server) handleHostingCreate(c *gin.Context) {
	var request HostingCreateRequest
	if err := bindHostingJSON(c, &request); err != nil {
		hostingReply(c, nil, err)
		return
	}
	if request.OwnerRef != c.GetString(hostingOwnerContextKey) {
		hostingReply(c, nil, app_errors.ErrUnauthorized)
		return
	}
	request.OwnerRef = c.GetString(hostingOwnerContextKey)
	result, err := s.service.CreateHostingAccount(c.Request.Context(), request)
	hostingReply(c, result, err)
}

// 用户列表始终带归属查询，管理员全量视图使用已有分组管理页。
// 查询主体来自已核验上下文，原query值仅由中间件用于一致性检查。
// 不返回软删除账号，也不向列表添加任何内部 Key。
// 查询先限定 owner_ref，不能先全量取出再依赖前端过滤。
// 返回顺序按创建时间稳定，用户调用优先级由 Oak 另行保存。
// 每个条目只读取本地观测，不批量向厂商请求刷新。
// 任一摘要无法读取时返回明确错误，避免伪装成空账号列表。
// deleted 不出现在用户列表，但幂等记录仍留在数据库中。
// 列表不分页的首期边界依赖 Oak 对每用户账号数的上限。
// 用户别名原样作为数据返回，前端须按文本渲染。
func (s *Server) handleHostingList(c *gin.Context) {
	owner := c.GetString(hostingOwnerContextKey)
	if !validHostingRef(owner) {
		hostingReply(c, nil, app_errors.ErrValidation)
		return
	}
	var bindings []models.HostingBinding
	if err := s.service.db.WithContext(c.Request.Context()).Where("owner_ref = ? AND status <> ?", owner, "deleted").Order("created_at_ms ASC").Find(&bindings).Error; err != nil {
		hostingReply(c, nil, err)
		return
	}
	items := make([]HostingAccount, 0, len(bindings))
	for _, binding := range bindings {
		item, err := s.service.hostingSummary(c.Request.Context(), binding, false)
		if err != nil {
			hostingReply(c, nil, err)
			return
		}
		items = append(items, item)
	}
	hostingReply(c, gin.H{"items": items}, nil)
}

// 账号动作统一从可信映射取得 Group/Credential ID，永不接收这些远端 ID。
// 重置消费与纯状态查询分开；GET 无论何种状态都不会调用上游。
// 所有子动作共享一次归属读取，不接受浏览器远端资源 ID。
// deleted 账号禁止模型、观测和测试操作。
// 完成的重置结果在解绑后仍可读取，只返回已有操作记录。
// 删除请求本身可重复执行，返回已清理状态。
// 激活不会恢复后来暂停的账号，恢复需明确 PATCH。
// 只读额度接口不等待厂商网络，也不会扣原厂重置次数。
// 刷新额度使用既有节流和观测状态机，错误不会伪造新鲜快照。
// 连接测试需要明确模型和协议，是真实上游请求入口。
// 测试返回的恢复 proof 属于管理恢复权限，不能回传普通用户。
// 消费接口只接受空对象和幂等头，不接受任意原厂请求参数。
// 同 UUID 恢复保留原选卡状态，不因重试另选一张卡。
// 重置查询同时匹配操作 ID、组和凭据，避免跨账号读取结果。
// prepared 与 outcome_unknown 都不是成功，也不能解释成未扣卡。
// 仅 succeeded 解码已存结果，查询不会发起兑换或补刷新。
// 存储结果损坏明确失败，不能以默认零次重置掩盖异常。
// HTTP 200 下仍需检查信封 code，再解析具体动作数据。
// 状态查询与资源删除相互独立，调用方应保留操作引用。
// 业务用户安全摘要应由 Oak 再次投影，禁止透传内部资源关系。
func (s *Server) handleHostingAccount(c *gin.Context) {
	ctx := c.Request.Context()
	// 先以核验主体限定账号，再读取状态或调用原厂动作。
	owner, account := c.GetString(hostingOwnerContextKey), c.Param("account_ref")
	binding, err := s.service.lookupHosting(ctx, owner, account)
	if err != nil {
		hostingReply(c, nil, err)
		return
	}
	path := c.FullPath()
	if binding.Status == "deleted" && c.Request.Method != http.MethodDelete && c.Param("operation_id") == "" {
		hostingReply(c, nil, app_errors.ErrResourceNotFound)
		return
	}
	switch {
	case strings.HasSuffix(path, "/models"):
		result, err := s.service.GetGroupModels(ctx, binding.GroupID)
		hostingReply(c, gin.H{"models": result.Items}, err)
	case strings.HasSuffix(path, "/activate"):
		if err := bindOptionalEmptyJSONObject(c); err != nil {
			hostingReply(c, nil, err)
			return
		}
		result, err := s.service.UpdateHostingAccount(ctx, owner, account, HostingUpdateRequest{}, true)
		hostingReply(c, result, err)
	case strings.HasSuffix(path, "/observation"):
		result, err := s.service.GetCredentialObservation(ctx, binding.GroupID, binding.CredentialID)
		hostingReply(c, result, err)
	case strings.HasSuffix(path, "/observation-refresh"):
		if err := bindOptionalEmptyJSONObject(c); err != nil {
			hostingReply(c, nil, err)
			return
		}
		result, err := s.service.RefreshCredentialObservation(ctx, binding.GroupID, binding.CredentialID)
		hostingReply(c, result, err)
	case strings.HasSuffix(path, "/test"):
		var request CredentialProbeRequest
		if err := bindHostingJSON(c, &request); err != nil {
			hostingReply(c, nil, err)
			return
		}
		result, err := s.service.TestGroupCredential(ctx, binding.GroupID, binding.CredentialID, request)
		// 恢复 proof 属于管理动作，不需要发送给 Oak 普通用户。
		result.RestoreProof = nil
		hostingReply(c, result, err)
	case strings.HasSuffix(path, "/reset-credits/consume"):
		if err := bindOptionalEmptyJSONObject(c); err != nil {
			hostingReply(c, nil, err)
			return
		}
		key, ok := requiredIdempotencyKey(c, "hosting_reset")
		if !ok {
			return
		}
		// 与解绑串行，未决消费时原凭据不能被删掉。
		result, err := s.service.ConsumeCredentialResetCredit(ctx, binding.GroupID, binding.CredentialID, key)
		hostingReply(c, result, err)
	case c.Param("operation_id") != "":
		operationID := c.Param("operation_id")
		if validateIdempotencyKey(operationID) != nil {
			hostingReply(c, nil, app_errors.ErrValidation)
			return
		}
		var operation models.CredentialResetOperation
		err := s.service.db.WithContext(ctx).Where("idempotency_key = ? AND group_id = ? AND credential_id = ?", operationID, binding.GroupID, binding.CredentialID).Take(&operation).Error
		if err != nil {
			hostingReply(c, nil, app_errors.ParseDBError(err))
			return
		}
		result := gin.H{"operation_id": operationID, "status": operation.State, "error_code": operation.ErrorCode, "updated_at_ms": operation.UpdatedAtMS}
		if operation.State == models.CredentialResetOperationSucceeded {
			stored, err := decodeStoredResetCreditResult(operation.ResultJSON)
			if err != nil {
				hostingReply(c, nil, err)
				return
			}
			result["windows_reset"], result["redeemed_at_ms"] = stored.WindowsReset, stored.RedeemedAtMS
		}
		hostingReply(c, result, nil)
	case c.Request.Method == http.MethodDelete:
		err := s.service.DeleteHostingAccount(ctx, owner, account)
		hostingReply(c, gin.H{"status": "deleted"}, err)
	case c.Request.Method == http.MethodPatch:
		var request HostingUpdateRequest
		if err := bindHostingJSON(c, &request); err != nil {
			hostingReply(c, nil, err)
			return
		}
		result, err := s.service.UpdateHostingAccount(ctx, owner, account, request, false)
		hostingReply(c, result, err)
	default:
		result, err := s.service.hostingSummary(ctx, binding, false)
		hostingReply(c, result, err)
	}
}
