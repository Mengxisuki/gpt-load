package control

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"unicode/utf8"

	"gorm.io/gorm"
	"gpt-load/internal/channel"
	app_errors "gpt-load/internal/platform/errors"
	"gpt-load/internal/protocol"
	"gpt-load/internal/state"
	stateloader "gpt-load/internal/state/loader"
	"gpt-load/internal/storage/models"
)

// HostingAccount 是内网安全摘要，不包含任何上游授权材料。
// InternalKey 仅在幂等创建响应出现，调用方必须加密保存且不能回传浏览器。
// 账号和所有者引用由 Oak 产生，永不使用邮箱作为权限标识。
// 边界：该结构只用于两服务之间，不是浏览器返回结构。
// 调用方须按实际登录用户过滤字段，不能直接透传整个摘要。
// Group、Credential、AccessKey 三个 ID 供内部对账和排障。
// IdentityFingerprint 是不可逆身份声明，不能用于用户可读展示。
// EmailMask 才是允许呈现给用户的身份提示。
// Observation 是缓存快照，失败不会自动触发额度刷新。
// 账号状态与上游健康是两种概念，暂停账号仍可有新鲜额度。
// 内部 Key 只授权一个账号，跨来源顺序由 New API 处理。
// 首次授权需由用户浏览器访问官方登录页，服务器不代收账号密码。
// 挂靠免费与原厂订阅额度属于业务约定，本服务不创建额外额度。
// 重置机会只能消费账号已经持有的机会，不接收外部卡密。
// 每次推理仅使用一个账号，跨来源重放策略由调用方决定。
// 普通上游限流或未知网络错误不表示该请求没有执行。
// 当前不提供可信未执行响应头，调用方不能据此自动重放。
// 历史重置操作查询是纯本地读取，适用于断线后的结果确认。
// 快照中的 observed_at_ms 不能替代卡详情独立的更新时间。
// 资源身份绑定不由分组名字决定，管理员重命名不会转移所有权。
// 分组列表的分类来源于这份绑定，不接受人工修改分类字段。
// 单实例互斥用于连接过程，数据库唯一索引用于持久约束。
// 不支持多个 gpt-load 进程同时共享该执行数据库进行挂靠变更。
// 业务数据库与执行数据库需要成套备份，内部 Key 必须同步恢复。
// 仅恢复业务数据而缺少执行绑定时应报告不可用，不能回退公共组。
// 这里不实现账号迁移，换所有者需要先安全解绑后重新授权。
// 内部控制契约应与 Oak 同步发布，不能假设旧二进制识别挂靠表。
type HostingAccount struct {
	AccountRef          string                         `json:"account_ref"`
	OwnerRef            string                         `json:"owner_ref"`
	Provider            string                         `json:"provider"`
	Alias               string                         `json:"alias"`
	Status              string                         `json:"status"`
	GroupID             uint                           `json:"group_id"`
	CredentialID        uint                           `json:"credential_id"`
	AccessKeyID         uint                           `json:"access_key_id"`
	IdentityFingerprint string                         `json:"identity_fingerprint"`
	EmailMask           string                         `json:"email_mask"`
	Observation         *CredentialObservationResponse `json:"observation,omitempty"`
	InternalKey         string                         `json:"internal_key,omitempty"`
}

// 授权操作绑定用途，重新授权只能写回原有账号。
// 每个操作 ID 只能与一组固定参数配对，网络重试复用它。
// 调用者从可信会话生成 OwnerRef，不接受浏览器任意覆盖。
// AccountRef 在授权开始前分配，后续步骤沿用同一引用。
// OperationID 必须是 UUID v4，断线重试不能换 UUID。
// 同一 UUID 修改账号或用途会显式冲突。
// connect 为首次连接；reauthorize 保留原账号和资源。
// 重新授权不能借备注或邮箱相似性替换上游身份。
type HostingAuthorizationRequest struct {
	OwnerRef    string `json:"owner_ref"`
	AccountRef  string `json:"account_ref"`
	OperationID string `json:"operation_id"`
	Purpose     string `json:"purpose"`
}

// 创建不接收模型、代理、上游地址或身份，避免客户端扩大权限。
// 模型目录从已完成的浏览器授权向原厂查询。
// StageID 指向本服务保存的单次授权结果。
// OperationID 与发起授权时相同，不能挪用另一授权的结果。
// Alias 是显示文本，裁剪空白后保存，不参与身份比较。
// 请求不接受用户提交的邮箱、身份指纹和原厂令牌。
// 创建返回 pending，要求调用方先可靠保存内部 Key。
// 若响应丢失，原操作重放返回相同内部资源。
type HostingCreateRequest struct {
	OwnerRef    string `json:"owner_ref"`
	AccountRef  string `json:"account_ref"`
	OperationID string `json:"operation_id"`
	StageID     string `json:"stage_id"`
	Alias       string `json:"alias"`
}

// 备注与暂停是仅有的普通可变字段；账号所有者不可修改。
// 采用指针区分字段缺失和显式恢复，拒绝空更新。
// 字段缺失表示保持现状，不等同于空字符串或 false。
// 空备注是有效更新，客户端可主动清除原备注。
// 普通恢复只处理已启用过的账号，pending 必须显式激活。
// 删除账号不可恢复，重新连接必须分配新账号引用。
type HostingUpdateRequest struct {
	Alias  *string `json:"alias"`
	Paused *bool   `json:"paused"`
}

// 引用限定为 URL 安全字符，数据库查询仍全部使用参数化。
// 不透明引用无个人资料含义，最大长度在入口统一约束。
// 限制只检查引用语法，不声明引用属于某个登录用户。
// 归属仍须由数据库 owner/account 联合条件确认。
// 允许冒号兼容 Oak 用户引用格式，不允许斜线改变 URL 层级。
// 长度限制对应数据库列上限，避免不同数据库截断差异。
func validHostingRef(value string) bool {
	if len(value) == 0 || len(value) > 128 {
		return false
	}
	for _, c := range value {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == ':') {
			return false
		}
	}
	return true
}

// lookupHosting 以账号和所有者同时查询，错误不泄露其他用户归属。
// 删除后的记录仅用于原创建操作重放，不开放运行接口。
// 所有用户资源操作复用此读取入口，避免漏写所有者条件。
// 不存在和属于他人的账号得到相同资源不存在错误。
// 数据库故障保持原错误分类，不伪装成不存在。
// 函数保留 deleted 记录供删除和创建幂等判断。
// 是否允许访问 deleted 由具体动作明确决定。
// 纯查询不获取写锁，读取的是数据库已提交状态。
func (s *Service) lookupHosting(ctx context.Context, owner, account string) (models.HostingBinding, error) {
	if !validHostingRef(owner) || !validHostingRef(account) {
		return models.HostingBinding{}, app_errors.ErrValidation
	}
	var binding models.HostingBinding
	err := s.db.WithContext(ctx).Where("owner_ref = ? AND account_ref = ?", owner, account).Take(&binding).Error
	if err != nil {
		return binding, app_errors.ParseDBError(err)
	}
	return binding, nil
}

// 获取安全摘要时不主动连接上游，避免列表读取意外耗费或阻塞。
// 缺失观测在 DTO 中保持缺失，不能编造零额度或已验证状态。
// 这里只读缓存，没有外部网络副作用。
// 列表和普通详情使用 reveal=false，不解密内部调用 Key。
// 创建重放使用 reveal=true，解决响应丢失后的密钥恢复。
// 解密失败必须向调用方报告，不能生成另一把 Key 掩盖故障。
// 删除后的内部资源已不存在，摘要提前返回且不携带 Key。
// 授权材料始终留在凭据存储，摘要不包含其密文或明文。
// 观测缺失保持 unavailable，不能编造额度已耗尽。
// 返回值可用于协调两服务创建状态，不代表真实推理验证通过。
func (s *Service) hostingSummary(ctx context.Context, binding models.HostingBinding, reveal bool) (HostingAccount, error) {
	result := HostingAccount{AccountRef: binding.AccountRef, OwnerRef: binding.OwnerRef, Provider: binding.Provider, Alias: binding.Alias, Status: binding.Status, GroupID: binding.GroupID, CredentialID: binding.CredentialID, AccessKeyID: binding.AccessKeyID, EmailMask: binding.EmailMask}
	if binding.IdentityFingerprint != nil {
		result.IdentityFingerprint = *binding.IdentityFingerprint
	}
	if binding.Status == "deleted" {
		return result, nil
	}
	observation, err := s.GetCredentialObservation(ctx, binding.GroupID, binding.CredentialID)
	if err != nil {
		return result, err
	}
	result.Observation = &observation
	if reveal {
		// 内部 Key 不经通用 reveal HTTP 路由，仅允许受控创建结果重放。
		var key models.AccessKey
		if err := s.db.WithContext(ctx).Take(&key, binding.AccessKeyID).Error; err != nil {
			return result, err
		}
		plaintext, err := s.encryption.Decrypt(key.KeyValue)
		if err != nil {
			return result, err
		}
		result.InternalKey = plaintext
	}
	return result, nil
}

// BeginHostingAuthorization 只开启受控浏览器 OAuth，文件导入不在可信入口。
// 串行保护防止同一幂等操作在两个 HTTP 请求中建立两个可见 Stage。
// 锁顺序固定为 hostingMu 后 hostingStageMu，避免相互等待。
// 前者串行本实例绑定动作，后者隔离临时令牌刷新。
// 同操作重放优先读取原 Stage，不再次向厂商发起授权。
// 已取消或过期的原 Stage 仍返回原状态，需要新操作重新开始。
// 重新授权须先存在本人账号，且账号没有删除。
// 新建账号身份在换码前未知，不能在这里提前宣称身份唯一。
// 先创建 Stage 再保存归属；归属保存失败必须取消 Stage。
// 清理使用不受客户端断线影响的上下文，防止遗留可消费授权。
// 清理失败与原错误同时保留，便于排查中断位置。
// 当前仅 Codex 浏览器 OAuth，通用设备码接口不在本契约中。
func (s *Service) BeginHostingAuthorization(ctx context.Context, request HostingAuthorizationRequest) (CredentialStageResult, error) {
	if !validHostingRef(request.OwnerRef) || !validHostingRef(request.AccountRef) || validateIdempotencyKey(request.OperationID) != nil {
		return CredentialStageResult{}, app_errors.ErrValidation
	}
	if request.Purpose != "connect" && request.Purpose != "reauthorize" {
		return CredentialStageResult{}, app_errors.ErrValidation
	}
	s.hostingMu.Lock()
	defer s.hostingMu.Unlock()
	// 建立首个挂靠授权前，等待已开始的临时凭据刷新完成。
	s.hostingStageMu.Lock()
	defer s.hostingStageMu.Unlock()
	var previous models.HostingAuthorization
	err := s.db.WithContext(ctx).Take(&previous, "operation_id = ?", request.OperationID).Error
	if err == nil {
		if previous.OwnerRef != request.OwnerRef || previous.AccountRef != request.AccountRef || previous.Purpose != request.Purpose {
			return CredentialStageResult{}, app_errors.ErrIdempotencyKeyReused
		}
		return s.GetCredentialStage(ctx, previous.StageID)
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return CredentialStageResult{}, err
	}
	if request.Purpose == "reauthorize" {
		binding, err := s.lookupHosting(ctx, request.OwnerRef, request.AccountRef)
		if err != nil {
			return CredentialStageResult{}, err
		}
		if binding.Status == "deleted" {
			return CredentialStageResult{}, app_errors.ErrResourceNotFound
		}
	}
	result, err := s.BeginCredentialAuthorization(ctx, channel.Codex)
	if err != nil {
		return result, err
	}
	row := models.HostingAuthorization{OperationID: request.OperationID, OwnerRef: request.OwnerRef, AccountRef: request.AccountRef, Purpose: request.Purpose, StageID: result.StageID}
	if err := s.db.WithContext(ctx).Create(&row).Error; err != nil {
		// 映射失败的 Stage 不交给用户；清理错误不能替代原始落库错误。
		cleanupErr := s.CancelCredentialStage(context.WithoutCancel(ctx), result.StageID)
		return CredentialStageResult{}, errors.Join(err, cleanupErr)
	}
	return result, nil
}

// hostingAuthorization 是所有查询、回调和取消共用的归属入口。
// Stage ID 本身不是授权凭据，不允许凭猜测 ID 接管别人的登录。
// 用途映射与 Stage 秘密分开保存，查询不解密授权材料。
// 所有者从 Oak 内部请求取得，不能由 Stage ID 推导。
// 此入口不消耗 Stage，轮询可重复调用。
// 最终单次消费仍由原 Stage 状态机和事务保证。
func (s *Service) hostingAuthorization(ctx context.Context, owner, stage string) (models.HostingAuthorization, error) {
	if !validHostingRef(owner) {
		return models.HostingAuthorization{}, app_errors.ErrValidation
	}
	var row models.HostingAuthorization
	err := s.db.WithContext(ctx).Where("owner_ref = ? AND stage_id = ?", owner, stage).Take(&row).Error
	if err != nil {
		return row, app_errors.ParseDBError(err)
	}
	return row, nil
}

// CreateHostingAccount 原子创建私有组、唯一身份、凭据和受限内部 Key。
// 发布前资源都处于 pending，调用方持久保存 Key 后才能显式激活。
// 官方 OAuth 换码产生的 Stage 才可信；用户上传的 JSON 永远不能借此建号。
// 函数承担单数据库原子边界，不承担两服务分布式事务。
// 模型发现失败时不创建组、Key 或半完成绑定。
// 发现之前检查身份冲突，发现之后在写事务内再次检查。
// 配置写屏障同时保护普通后台操作，不能绕过私有声明。
// 新建时先记录绑定再消费 Stage，使统一目的地检查认识新组。
// 事务中 CredentialID 暂为零，提交前必须补齐真实 ID。
// 编译快照再次核对唯一凭据和唯一组过滤器。
// 资源 ID 由数据库分配，调用方不能选择现有公共组冒充私有组。
// 重授权沿用原资源，只更新同一身份的授权秘密。
// 旧凭据进入需重授权状态后，复用已有消费逻辑原位恢复。
// 重授权事务失败回滚旧状态，不留下半更新凭据。
// 同一操作重放不执行发现、不再次消费 Stage。
// 已删除操作重放只返回删除状态，不复活资源。
// 创建本身不调用真实推理或消耗重置机会。
// Key 仅开放 HTTP Chat/Responses，实际模型能力由渠道决定。
// 通用配置校验阻止把私有组改成第三方上游地址。
// 注册表协调成功后才发布快照，数据库与运行态更新复用原机制。
// 调用方保存成功后再激活，保存失败的 pending 资源可安全重试。
// 内部 Key 不能发给客户端，否则会绕过 Oak 的统一策略和日志。
// 这层不处理钱包金额或套餐预算，也不跨账号重试请求。
func (s *Service) CreateHostingAccount(ctx context.Context, request HostingCreateRequest) (HostingAccount, error) {
	if !validHostingRef(request.OwnerRef) || !validHostingRef(request.AccountRef) || validateIdempotencyKey(request.OperationID) != nil || !utf8.ValidString(request.Alias) || len(request.Alias) > 128 {
		return HostingAccount{}, app_errors.ErrValidation
	}
	s.hostingMu.Lock()
	defer s.hostingMu.Unlock()
	authorization, err := s.hostingAuthorization(ctx, request.OwnerRef, request.StageID)
	if err != nil {
		return HostingAccount{}, err
	}
	if authorization.AccountRef != request.AccountRef || authorization.OperationID != request.OperationID {
		return HostingAccount{}, app_errors.ErrForbidden
	}
	existing, existingErr := s.lookupHosting(ctx, request.OwnerRef, request.AccountRef)
	if existingErr == nil && existing.OperationID == request.OperationID {
		if existing.StageID != request.StageID {
			return HostingAccount{}, app_errors.ErrIdempotencyKeyReused
		}
		return s.hostingSummary(ctx, existing, true)
	}
	if existingErr == nil && authorization.Purpose != "reauthorize" {
		return HostingAccount{}, app_errors.ErrDuplicateResource
	}
	if existingErr != nil && !errors.Is(existingErr, app_errors.ErrResourceNotFound) {
		return HostingAccount{}, existingErr
	}
	if existingErr != nil && authorization.Purpose != "connect" {
		return HostingAccount{}, app_errors.ErrResourceNotFound
	}
	stages, err := s.loadConsumableCredentialStages(s.db.WithContext(ctx), channel.Codex, models.ConnectionTypeSubscription, []string{request.StageID}, false)
	if err != nil {
		return HostingAccount{}, err
	}
	stage := stages[0]
	if stage.AuthorizationMethod != string(channel.AuthorizationBrowserOAuth) {
		return HostingAccount{}, app_errors.ErrForbidden
	}
	if existingErr == nil && (existing.Status == "deleted" || existing.IdentityFingerprint == nil || *existing.IdentityFingerprint != stage.IdentityFingerprint) {
		return HostingAccount{}, app_errors.ErrStagedCredentialMismatch
	}
	// 先查重再发现模型，避免重复账号在 Stage 阶段发生刷新。
	if err := s.hostingIdentityAvailable(s.db.WithContext(ctx), stage.IdentityFingerprint, existing.GroupID); err != nil {
		return HostingAccount{}, err
	}
	discovery, err := s.DiscoverModels(ctx, ModelDiscoveryRequest{ChannelID: channel.Codex, ConnectionType: models.ConnectionTypeSubscription, Params: json.RawMessage(`{}`), StagedCredentialID: stage.ID})
	if err != nil {
		return HostingAccount{}, err
	}
	groupModels := make([]GroupModel, 0, len(discovery.Models))
	for _, model := range discovery.Models {
		groupModels = append(groupModels, GroupModel{ID: model.ID})
	}
	if len(groupModels) == 0 {
		return HostingAccount{}, app_errors.ErrNoActiveCredential
	}
	rawModels, err := json.Marshal(groupModels)
	if err != nil {
		return HostingAccount{}, err
	}
	var binding models.HostingBinding
	var entries []state.CredentialEntry
	_, err = s.writeGroupConfig(ctx, func(tx *gorm.DB) error {
		// 同事务复核身份，普通管理导入也使用相同写屏障和身份门禁。
		if err := s.hostingIdentityAvailable(tx, stage.IdentityFingerprint, existing.GroupID); err != nil {
			return err
		}
		if existingErr == nil {
			binding = existing
			// 重授权仅更换相同身份的秘密，所有资源 ID 与内部 Key 保持稳定。
			if err := tx.Model(&models.Credential{}).Where("id = ?", binding.CredentialID).Update("auth_state", models.CredentialAuthStateReauthorizationRequired).Error; err != nil {
				return err
			}
			if _, _, err := s.consumeCredentialStages(tx, binding.GroupID, channel.Codex, models.ConnectionTypeSubscription, []string{stage.ID}); err != nil {
				return err
			}
			binding.OperationID, binding.StageID = request.OperationID, stage.ID
			if err := tx.Save(&binding).Error; err != nil {
				return err
			}
		} else {
			group := models.Group{Name: "hosting-" + request.AccountRef, ChannelID: string(channel.Codex), ConnectionType: models.ConnectionTypeSubscription, Params: models.JSON(`{}`), Models: models.JSON(rawModels), Overrides: models.JSON(`{}`), Enabled: true}
			if err := s.initializeValidationProtocol(&group); err != nil {
				return err
			}
			if err := tx.Create(&group).Error; err != nil {
				return err
			}
			// 只开放首期 HTTP 协议；不让内部 Key 借通用路由进入图片/实时入口。
			key, _, err := s.newAccessKeyRow("hosting-"+request.AccountRef, AccessKeyFilters{Groups: []uint{group.ID}, Protocols: []protocol.Protocol{protocol.OpenAIResponses, protocol.OpenAICompletions}}, 0, "")
			if err != nil {
				return err
			}
			key.Status = string(state.AccessKeyStatusDisabled)
			if err := tx.Create(&key).Error; err != nil {
				return err
			}
			binding = models.HostingBinding{AccountRef: request.AccountRef, OwnerRef: request.OwnerRef, OperationID: request.OperationID, StageID: stage.ID, Provider: "codex", Alias: strings.TrimSpace(request.Alias), IdentityFingerprint: &stage.IdentityFingerprint, GroupID: group.ID, AccessKeyID: key.ID, Status: "pending"}
			// 私有声明先写入，consume 的统一门禁才能识别此次合法转入。
			if err := tx.Create(&binding).Error; err != nil {
				return app_errors.ParseDBError(err)
			}
			if _, _, err := s.consumeCredentialStages(tx, group.ID, channel.Codex, models.ConnectionTypeSubscription, []string{stage.ID}); err != nil {
				return err
			}
			var credential models.Credential
			if err := tx.Where("group_id = ?", group.ID).Take(&credential).Error; err != nil {
				return err
			}
			binding.CredentialID = credential.ID
			// SafeSummary 由受控换码生成，不输出原厂完整身份。
			var account CredentialStageAccount
			if err := json.Unmarshal(stage.SafeSummaryJSON, &account); err != nil {
				return err
			}
			binding.EmailMask = account.EmailMask
			if err := tx.Save(&binding).Error; err != nil {
				return err
			}
		}
		var loadErr error
		entries, loadErr = stateloader.BuildGroupCredentialEntriesWithProxy(ctx, tx, binding.GroupID, s.encryption)
		return loadErr
	}, func() error { _, err := s.reconcileRegistryGroup(binding.GroupID, entries); return err })
	if err != nil {
		return HostingAccount{}, err
	}
	return s.hostingSummary(ctx, binding, true)
}

// 全局身份声明同时检查普通组，不能把已运行的账号复制成私有账号。
// 同一原账号重授权可排除自身组，其他任何组都视为冲突。
// 身份指纹来自官方浏览器换码返回的账户标识。
// 不使用邮箱判重，邮箱可能变化或被其他输入伪造。
// 普通组中的同身份凭据同样阻止挂靠，避免双份刷新状态。
// 只排除同账号原组，为原位重授权保留合法路径。
// 数据库唯一索引是第二道约束，不代替明确的业务冲突错误。
// 软删除将指纹置空后，未来的新账号可重新声明该身份。
func (s *Service) hostingIdentityAvailable(tx *gorm.DB, fingerprint string, ownGroup uint) error {
	var count int64
	if err := tx.Model(&models.Credential{}).Where("identity_fingerprint = ? AND group_id <> ?", fingerprint, ownGroup).Count(&count).Error; err != nil {
		return err
	}
	if count > 0 {
		return app_errors.ErrDuplicateCredentialIdentity
	}
	if err := tx.Model(&models.HostingBinding{}).Where("identity_fingerprint = ? AND group_id <> ?", fingerprint, ownGroup).Count(&count).Error; err != nil {
		return err
	}
	if count > 0 {
		return app_errors.ErrDuplicateCredentialIdentity
	}
	return nil
}

// UpdateHostingAccount 同事务更新执行 Key 和持久状态，发布后新请求立即受限。
// pending 只能用 activate 激活，普通恢复不能绕过创建确认。
// 状态和内部 Key 状态必须在一个事务中改变。
// 调用方不能单独把绑定标为 active 而留下 disabled Key。
// 激活重放只转换 pending，不能恢复后来被用户暂停的账号。
// 普通恢复要求显式 paused=false，避免无意恢复服务。
// 暂停只阻止新请求，不承诺强制终止已开始的流。
// 本函数不修改凭据健康，授权失效仍需重新授权。
// 备注修改不触发厂商调用，也不改变用户归属。
// 快照发布失败向调用方返回错误，不能报告成功。
// 暂停账号仍可查看额度和处理已有重置操作，推理测试需先显式恢复。
// 独立的上游限额状态由观测返回，不写入业务启停状态。
func (s *Service) UpdateHostingAccount(ctx context.Context, owner, account string, request HostingUpdateRequest, activate bool) (HostingAccount, error) {
	s.hostingMu.Lock()
	defer s.hostingMu.Unlock()
	binding, err := s.lookupHosting(ctx, owner, account)
	if err != nil {
		return HostingAccount{}, err
	}
	if binding.Status == "deleted" {
		return HostingAccount{}, app_errors.ErrResourceNotFound
	}
	if request.Alias != nil && (!utf8.ValidString(*request.Alias) || len(*request.Alias) > 128) {
		return HostingAccount{}, app_errors.ErrValidation
	}
	if !activate && request.Alias == nil && request.Paused == nil {
		return HostingAccount{}, app_errors.ErrValidation
	}
	if activate && binding.Status == "pending" {
		binding.Status = "active"
	}
	if request.Paused != nil {
		if binding.Status == "pending" {
			return HostingAccount{}, app_errors.ErrInvalidCredentialState
		}
		binding.Status = "active"
		if *request.Paused {
			binding.Status = "paused"
		}
	}
	if request.Alias != nil {
		binding.Alias = strings.TrimSpace(*request.Alias)
	}
	_, err = s.writeConfig(ctx, func(tx *gorm.DB) error {
		keyStatus := state.AccessKeyStatusDisabled
		if binding.Status == "active" {
			keyStatus = state.AccessKeyStatusActive
		}
		if err := tx.Model(&models.AccessKey{}).Where("id = ?", binding.AccessKeyID).Update("status", keyStatus).Error; err != nil {
			return err
		}
		return tx.Save(&binding).Error
	}, nil)
	if err != nil {
		return HostingAccount{}, err
	}
	return s.hostingSummary(ctx, binding, false)
}

// DeleteHostingAccount 先持久停用，再判断是否存在未决重置。
// 未决时保留原凭据及操作，供同 UUID 恢复；绝不把未知结果当未消费。
// 第一阶段停用必须先落库，第二阶段失败仍保持不可调用。
// 删除与 reset 的 prepared 记录建立共享同一互斥边界。
// 消费已进入 prepared 后，即使网络尚未返回也不得删凭据。
// 未知结果需要使用原 UUID 恢复，不能删除后换 UUID 再消费。
// 已完成或明确拒绝的重置操作不阻止资源清理。
// Key 直接删除，不通过清空组过滤器解除引用。
// 组删除使用数据库既有外键级联清理凭据及其从属数据。
// 绑定保留 tombstone，原创建操作不会在重试时重复建号。
// 身份指纹置空释放独占声明，历史资源 ID 仍可用于对账。
// 注册表删除与凭据运行态回收在写屏障内完成。
// 操作历史只含结果，不因此保存已删除的凭据明文。
// 历史备份清理由部署保留策略负责，本函数不宣称即时擦除备份。
func (s *Service) DeleteHostingAccount(ctx context.Context, owner, account string) error {
	binding, err := s.lookupHosting(ctx, owner, account)
	if err != nil || binding.Status == "deleted" {
		return err
	}
	s.hostingMu.Lock()
	defer s.hostingMu.Unlock()
	// 加锁后重新读取，不能覆盖刚完成的重授权或并发删除。
	binding, err = s.lookupHosting(ctx, owner, account)
	if err != nil || binding.Status == "deleted" {
		return err
	}
	// 独立提交停用状态，后续未决检查失败也不恢复数据面权限。
	_, err = s.writeConfig(ctx, func(tx *gorm.DB) error {
		if err := tx.Model(&models.AccessKey{}).Where("id = ?", binding.AccessKeyID).Update("status", "disabled").Error; err != nil {
			return err
		}
		return tx.Model(&binding).Update("status", "paused").Error
	}, nil)
	if err != nil {
		return err
	}
	_, err = s.writeGroupConfig(ctx, func(tx *gorm.DB) error {
		var pending int64
		if err := tx.Model(&models.CredentialResetOperation{}).Where("credential_id = ? AND state IN ?", binding.CredentialID, []string{"prepared", "outcome_unknown"}).Count(&pending).Error; err != nil {
			return err
		}
		if pending > 0 {
			return app_errors.ErrResetCreditOutcomeUnknown
		}
		// 不将 Key 的 groups 清空；直接删除 Key，避免空过滤产生全局权限。
		if err := tx.Delete(&models.AccessKey{}, binding.AccessKeyID).Error; err != nil {
			return err
		}
		if err := tx.Delete(&models.Group{}, binding.GroupID).Error; err != nil {
			return err
		}
		return tx.Model(&binding).Updates(map[string]any{"status": "deleted", "identity_fingerprint": nil}).Error
	}, func() error {
		s.registry.RemoveGroup(binding.GroupID)
		s.retireCredentialRuntime(binding.CredentialID)
		return nil
	})
	return err
}

// 普通 Stage 消费也检查私有声明，防止管理员误把私有凭据复制到公共组。
// 由授权映射确认的 Stage 必须消费到其指定账号，不能仅凭 Stage ID 使用。
// 该门禁挂在所有 Stage 消费路径，包含管理员普通建组。
// 仅限制已声明私有的身份，公共组原有身份规则保持不变。
// 受控 Stage 必须进入它绑定的账号，不能借管理入口换目标。
// 查询失败直接中止事务，不能当作未发现挂靠声明。
// 绑定创建和凭据消费在同一事务，合法首次转入可读取未提交绑定。
// 检查目标所有者和账号引用，组名称及备注均不构成授权。
func (s *Service) validateHostingStageDestination(tx *gorm.DB, stage models.CredentialStage, groupID uint) error {
	var bindings []models.HostingBinding
	if err := tx.Where("identity_fingerprint = ?", stage.IdentityFingerprint).Find(&bindings).Error; err != nil {
		return err
	}
	for _, binding := range bindings {
		if binding.GroupID != groupID {
			return app_errors.ErrDuplicateCredentialIdentity
		}
	}
	var authorization models.HostingAuthorization
	err := tx.Where("stage_id = ?", stage.ID).Take(&authorization).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	var target models.HostingBinding
	if err := tx.Where("account_ref = ? AND owner_ref = ? AND group_id = ?", authorization.AccountRef, authorization.OwnerRef, groupID).Take(&target).Error; err != nil {
		return app_errors.ErrForbidden
	}
	return nil
}

// 导入前读取声明而不是凭据秘密；存在挂靠时暂停 Codex 文件导入。
// 原因是文件身份未验签且导入会提前刷新，浏览器授权仍然可用。
// 这是明确拒绝并报告原因的边界，不会静默回退到不可信导入。
// 首期文件身份未完成签名验证，不能拿它抢占私有身份。
// 原文件导入还会旋转刷新令牌，因此必须在网络调用前拒绝。
// 已存在私有绑定时，限制覆盖普通管理导入入口。
// 首个挂靠授权尚未建号时也要保护，避免初始化期间的竞态。
// 授权终止且无存量挂靠后，文件导入可重新按原规则运行。
// 临时 Stage 需要刷新时也执行此检查，正式凭据刷新不受影响。
// 浏览器新授权通常具有足够有效期，过期则重新开始授权。
// 错误码固定且可定位，客户端不应静默回退到其他导入途径。
func (s *Service) forbidHostedCodexImport(ctx context.Context, channelID channel.ID) error {
	if channelID != channel.Codex {
		return nil
	}
	var count int64
	if err := s.db.WithContext(ctx).Model(&models.HostingBinding{}).Where("status <> ?", "deleted").Count(&count).Error; err != nil {
		return err
	}
	if count == 0 {
		// 已发起但尚未建号的授权也要保护，避免首个账号与文件刷新竞争。
		if err := s.db.WithContext(ctx).Model(&models.HostingAuthorization{}).
			Joins("JOIN credential_stages ON credential_stages.id = hosting_authorizations.stage_id").
			Where("credential_stages.status IN ?", []string{"pending_authorization", "ready", "exchanging"}).Count(&count).Error; err != nil {
			return err
		}
	}
	if count > 0 {
		return &app_errors.APIError{HTTPStatus: 409, Code: "HOSTING_BROWSER_AUTH_REQUIRED", Message: "用户挂靠启用后，Codex 凭据文件导入暂停，请使用浏览器授权"}
	}
	return nil
}
