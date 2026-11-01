package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"gpt-load/internal/channel"
	"gpt-load/internal/platform/config"
	app_errors "gpt-load/internal/platform/errors"
	"gpt-load/internal/protocol"
	"gpt-load/internal/scheduler"
	"gpt-load/internal/state"
	"gpt-load/internal/storage/models"
	"gpt-load/internal/subscription/providers/codex"
)

// 测试只替换厂商换码与模型发现网络，真实执行归属、事务和快照代码。
// 每个账号都经过浏览器 Stage 状态机，避免直接插绑定自证权限正确。
// 桩仅负责官方换码结果，不替换创建、归属或调度实现。
// state 必须一致，否则模拟换码立即失败。
// 返回令牌有效期足够，避免测试意外调用真实刷新网络。
// 模型发现仅返回一个固定模型，使权限断言不依赖远程模型目录。
// 每个测试独立数据库和加密器，不共享运行态账号。
// 回调 URL 使用驱动真正声明的 localhost 路径。
// 真实 Stage 状态机完成 pending 到 ready 的转换。
// 返回结果供后续建号消费，测试不直接构造 consumed Stage。
// 所有邮箱和令牌均为测试固定字符串，不读取开发者账号文件。
// 测试库通过既有迁移链创建，新增模型不会在用例内单独建表。
// 没有通过断言数据库插入成功来替代权限行为测试。
// 归属使用带冒号的 Oak 引用，验证入口语法兼容真实集成形式。
// 同一测试多次授权时会重设外部换码桩，不影响前一个已存凭据。
// 每次换码生成相同身份对应的虚构秘密，便于验证原位重授权。
// 模拟模型目录不承诺生产环境一定存在该模型。
// 未配置真实代理和厂商出口，因此不产生外部网络费用。
// 状态机和持久幂等断言可重复运行，不依赖系统固定日期。
// 只有测试返回成功后才报告本地隔离行为通过。
// 真实客户端兼容、厂商限额和重置消费仍需另外验收。
func hostingTestAuthorization(t *testing.T, fixture serviceFixture, owner, account, identity, operation, purpose string) CredentialStageResult {
	t.Helper()
	setCodexAuthorizationCompletion(t, fixture.service, func(_ context.Context, completion codex.BrowserAuthorizationCompletion) (codex.Credential, error) {
		if completion.ExpectedState != completion.ReturnedState {
			return codex.Credential{}, errors.New("state mismatch")
		}
		return codex.Credential{Type: "codex", AccountID: identity, Email: "owner@example.com", AccessToken: "access-" + identity, RefreshToken: "refresh-" + identity, Expire: time.Now().Add(time.Hour).Format(time.RFC3339)}, nil
	})
	setCodexModelDiscovery(t, fixture.service, func(context.Context, codex.Credential) ([]codex.Model, error) {
		return []codex.Model{{ID: "gpt-test"}}, nil
	})
	stage, err := fixture.service.BeginHostingAuthorization(t.Context(), HostingAuthorizationRequest{OwnerRef: owner, AccountRef: account, OperationID: operation, Purpose: purpose})
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := url.Parse(stage.AuthorizationURL)
	if err != nil {
		t.Fatal(err)
	}
	callback := "http://localhost:1455/auth/callback?code=provider-code&state=" + url.QueryEscape(parsed.Query().Get("state"))
	result, err := fixture.service.CompleteCredentialAuthorizationCallback(t.Context(), stage.StageID, callback)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

// pending 不可调用，显式激活才发布授权，暂停和删除可即时收回新请求。
// 同时验证全量 Key、其他账号 Key 与自身内部 Key 的调度结果不同。
// 验收第一阶段：建号返回 pending 且内部 Key 已持久保存。
// 同一创建请求重放必须返回相同 Key，避免遗留多个执行入口。
// 公共 Key 在私有账号创建后签发，测试未来资源不自动授权。
// pending Key 不应进入按哈希鉴权表，即使元数据索引仍存在。
// 显式激活后仅绑定 Key 获得该组权限。
// 调度器检查使用真实编译后的模型路由，而非测试复制条件。
// 跨用户读取统一返回不存在，避免暴露账号归属。
// 普通 Key 编辑不能清空私有 Key 的唯一组筛选。
// 普通 Group 删除不能绕过绑定管理造成悬挂声明。
// 失败的管理更新须回滚，随后暂停和删除仍应成功。
// 暂停验证 Key 状态而非元数据是否消失，停用元数据仍用于管理。
// 第一次删除必须真正移除凭据，而不是只隐藏列表条目。
// 第二次删除验证可安全重放，不要求用户手动判断上次结果。
// 删除后原创建重放不能返回旧 Key 或重建同一资源。
// 测试使用虚构凭据，不进行真实推理或消耗原厂重置机会。
func TestHostingLifecycleIsolationAndIdempotency(t *testing.T) {
	fixture := newServiceFixture(t)
	stage := hostingTestAuthorization(t, fixture, "oak:1", "account-1", "identity-1", resetCreditTestKey, "connect")
	request := HostingCreateRequest{OwnerRef: "oak:1", AccountRef: "account-1", OperationID: resetCreditTestKey, StageID: stage.StageID, Alias: "工作账号"}
	account, err := fixture.service.CreateHostingAccount(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	if account.Status != "pending" || account.InternalKey == "" {
		t.Fatalf("missing pending key: %#v", account)
	}
	repeated, err := fixture.service.CreateHostingAccount(t.Context(), request)
	if err != nil || repeated.GroupID != account.GroupID || repeated.InternalKey != account.InternalKey {
		t.Fatalf("creation not idempotent: %v", err)
	}
	// 创建之前的普通无限制 Key 不能因为新增挂靠组而获得访问权。
	public, err := fixture.service.CreateAccessKey(t.Context(), AccessKeyCreateRequest{Name: "public"})
	if err != nil {
		t.Fatal(err)
	}
	snapshot := fixture.manager.Current()
	if snapshot.AllowsGroup(snapshot.AccessKeysByID[public.ID], account.GroupID) {
		t.Fatal("unfiltered public key can enter private group")
	}
	if _, exists := snapshot.AccessKeysByHash[fixture.encryption.Hash(account.InternalKey)]; exists {
		t.Fatal("pending key active")
	}
	account, err = fixture.service.UpdateHostingAccount(t.Context(), "oak:1", "account-1", HostingUpdateRequest{}, true)
	if err != nil {
		t.Fatal(err)
	}
	snapshot = fixture.manager.Current()
	ownKey := snapshot.AccessKeysByID[account.AccessKeyID]
	query := scheduler.Query{AccessKey: ownKey, ClientProtocol: protocol.OpenAIResponses, ExternalModel: stringPointer("gpt-test")}
	if !snapshot.AllowsGroup(ownKey, account.GroupID) {
		t.Fatal("owner cannot enter active group")
	}
	// 调度器必须应用同一门禁，不能仅 UI 模型列表过滤。
	query.AccessKey = snapshot.AccessKeysByID[public.ID]
	if ids := scheduler.CandidateGroupIDsForQuery(snapshot, query); len(ids) != 0 {
		t.Fatalf("public scheduler leaked %v", ids)
	}
	if _, err := fixture.service.lookupHosting(t.Context(), "oak:2", "account-1"); !errors.Is(err, app_errors.ErrResourceNotFound) {
		t.Fatalf("other owner can see account: %v", err)
	}
	// 普通管理编辑不能扩大挂靠内部 Key 作用域或删除私有执行资源。
	if _, err := fixture.service.UpdateAccessKey(t.Context(), account.AccessKeyID, AccessKeyUpdateRequest{Filters: &AccessKeyFilters{}}); err == nil {
		t.Fatal("empty private key filter accepted")
	}
	if err := fixture.service.DeleteGroup(t.Context(), account.GroupID); err == nil {
		t.Fatal("private group deletion bypassed binding")
	}
	paused := true
	if _, err := fixture.service.UpdateHostingAccount(t.Context(), "oak:1", "account-1", HostingUpdateRequest{Paused: &paused}, false); err != nil {
		t.Fatal(err)
	}
	if key := fixture.manager.Current().AccessKeysByID[account.AccessKeyID]; key.Status != state.AccessKeyStatusDisabled {
		t.Fatal("paused key remains active")
	}
	// 分类由实际绑定派生，暂停账号不能因凭据健康而显示为可服务。
	// 管理列表同时提供不透明归属引用，方便排障但不输出原厂身份。
	workspace, err := fixture.service.ListModernGroups(t.Context())
	if err != nil || len(workspace.Items) != 1 || workspace.Items[0].Category != "hosting" || workspace.Items[0].OwnerRef != "oak:1" || workspace.Items[0].Availability != "paused" {
		t.Fatalf("hosting management category: %v", err)
	}
	if err := fixture.service.DeleteHostingAccount(t.Context(), "oak:1", "account-1"); err != nil {
		t.Fatal(err)
	}
	if err := fixture.service.DeleteHostingAccount(t.Context(), "oak:1", "account-1"); err != nil {
		t.Fatal(err)
	}
	// 删除后资源确实不存在，幂等重放也不能再返回失效内部 Key。
	var count int64
	fixture.db.Model(&models.Credential{}).Where("id = ?", account.CredentialID).Count(&count)
	if count != 0 {
		t.Fatal("deleted credential retained")
	}
	deleted, err := fixture.service.CreateHostingAccount(t.Context(), request)
	if err != nil || deleted.Status != "deleted" || deleted.InternalKey != "" {
		t.Fatalf("deleted replay: %#v %v", deleted, err)
	}
}

// 同一受控身份不得跨用户挂靠；普通 Stage 也不能复制进公共组。
// 文件导入在副作用前拒绝，不触发 Refresh Token 旋转。
// 两个不同 Oak 所有者使用同一模拟原厂身份完成浏览器授权。
// 第一账号先声明身份，第二账号必须在建号前返回明确冲突。
// 第二个 Stage 仍存在，用来测试普通管理建组不能绕过归属。
// 如果只在新 HTTP 接口校验，普通 CreateGroup 就会错误接受它。
// 验证跨入口约束的目标是防止同一账号出现两套刷新状态。
// 文件导入检查发生在网络副作用前，不依赖 Token 是否即将过期。
// 返回固定错误码，让调用方能够提示用户改用浏览器授权。
// 未把所有导入错误都算通过，错误类别必须精确匹配。
// 该测试不声称验证了厂商令牌签名，可信身份依赖官方换码路径。
// 授权冲突不删除第一账号，也不转移它的所属用户。
func TestHostingDuplicateIdentityAndFileImportBoundary(t *testing.T) {
	fixture := newServiceFixture(t)
	first := hostingTestAuthorization(t, fixture, "oak:1", "one", "same", resetCreditTestKey, "connect")
	_, err := fixture.service.CreateHostingAccount(t.Context(), HostingCreateRequest{OwnerRef: "oak:1", AccountRef: "one", OperationID: resetCreditTestKey, StageID: first.StageID})
	if err != nil {
		t.Fatal(err)
	}
	second := hostingTestAuthorization(t, fixture, "oak:2", "two", "same", "9f0f4c32-89d2-4bcb-9e19-052940dc2f17", "connect")
	if _, err := fixture.service.CreateHostingAccount(t.Context(), HostingCreateRequest{OwnerRef: "oak:2", AccountRef: "two", OperationID: "9f0f4c32-89d2-4bcb-9e19-052940dc2f17", StageID: second.StageID}); !errors.Is(err, app_errors.ErrDuplicateCredentialIdentity) {
		t.Fatalf("duplicate allowed: %v", err)
	}
	// 复制 Stage 到一个普通组同样必须被拦截。
	_, err = fixture.service.CreateGroup(t.Context(), GroupCreateRequest{Name: stringPointer("public-copy"), ChannelID: channel.Codex, ConnectionType: models.ConnectionTypeSubscription, Params: json.RawMessage(`{}`), Models: optionalGroupModels{Set: true, Values: []GroupModel{{ID: "gpt-test"}}}, StagedCredentialIDs: []string{second.StageID}, ConfirmSameTarget: true})
	if err == nil {
		t.Fatal("ordinary group imported private identity")
	}
	_, err = fixture.service.ImportCredentialStage(t.Context(), channel.Codex, []byte(`{"type":"codex","access_token":"a","refresh_token":"r","account_id":"same"}`))
	var apiError *app_errors.APIError
	if !errors.As(err, &apiError) || apiError.Code != "HOSTING_BROWSER_AUTH_REQUIRED" {
		t.Fatalf("file import error: %v", err)
	}
}

// 内网 HTTP 接口真实测试权限和结果查询，GET 绝不调用消费函数。
// 未决消费解绑保留资源并停用 Key，才能安全续办同一个 UUID。
// 使用真实 Gin 注册和管理认证链，不直接调用 handler 绕过鉴权。
// 管理员凭据允许请求，但仍须携带正确的 owner_ref。
// 另一个 owner 请求相同账号应得到资源不存在。
// 专属数据面 Key 也无管理接口权限，不能读取重置操作。
// 未提供任何凭据时应拒绝认证，避免开放内部状态查询。
// 测试预先持久化未知结果，GET 必须返回原状态。
// 没有配置消费网络桩，读取过程不需要触发任何消费函数。
// 解绑未知操作时应报结果未定，而非返回删除成功。
// 失败后 Key 已停用，防止用户误以为删除中账号仍继续被选择。
// 原 Credential 保留，可继续使用同一个幂等 UUID 核实结果。
// 测试没有创建新 UUID 重试消费，也不把 unknown 当作未消费。
// HTTP 响应校验覆盖状态码和安全结果正文。
// 这些行为与真实账号是否具有重置机会无关，因此可离线稳定复现。
func TestHostingHTTPResetReadAndDeleteGuard(t *testing.T) {
	fixture := newServiceFixture(t)
	stage := hostingTestAuthorization(t, fixture, "oak:1", "reset-account", "reset-id", resetCreditTestKey, "connect")
	account, err := fixture.service.CreateHostingAccount(t.Context(), HostingCreateRequest{OwnerRef: "oak:1", AccountRef: "reset-account", OperationID: resetCreditTestKey, StageID: stage.StageID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.service.UpdateHostingAccount(t.Context(), "oak:1", "reset-account", HostingUpdateRequest{}, true); err != nil {
		t.Fatal(err)
	}
	operation := models.CredentialResetOperation{IdempotencyKey: resetCreditTestKey, GroupID: account.GroupID, CredentialID: account.CredentialID, RedeemRequestID: resetCreditTestKey, State: models.CredentialResetOperationOutcomeUnknown, RequestDigest: []byte("test"), CreatedAtMS: 1, UpdatedAtMS: 1}
	if err := fixture.db.Create(&operation).Error; err != nil {
		t.Fatal(err)
	}
	initControlI18n(t)
	engine := gin.New()
	NewServer(&config.Config{AuthKey: "test-admin"}, fixture.service).RegisterRoutes(engine)
	for _, test := range []struct {
		owner, key string
		status     int
	}{{"oak:1", "test-admin", 200}, {"oak:2", "test-admin", 404}, {"oak:1", account.InternalKey, 403}, {"oak:1", "", 401}} {
		request := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/api/hosting/v1/accounts/reset-account/reset-operations/%s?owner_ref=%s", resetCreditTestKey, url.QueryEscape(test.owner)), nil)
		if test.key != "" {
			request.Header.Set("Authorization", "Bearer "+test.key)
		}
		result := httptest.NewRecorder()
		engine.ServeHTTP(result, request)
		if result.Code != test.status {
			t.Fatalf("HTTP %s got %d %s", test.owner, result.Code, result.Body)
		}
		if result.Code == 200 && !strings.Contains(result.Body.String(), "outcome_unknown") {
			t.Fatal("unknown result hidden")
		}
	}
	// 操作未决时不能删除原凭据；私有数据面已暂停。
	if err := fixture.service.DeleteHostingAccount(t.Context(), "oak:1", "reset-account"); !errors.Is(err, app_errors.ErrResetCreditOutcomeUnknown) {
		t.Fatalf("delete pending: %v", err)
	}
	if key := fixture.manager.Current().AccessKeysByID[account.AccessKeyID]; key.Status != state.AccessKeyStatusDisabled {
		t.Fatal("delete-pending key still active")
	}
	var stored models.Credential
	if err := fixture.db.Take(&stored, account.CredentialID).Error; err != nil {
		t.Fatal("unknown reset lost credential")
	}
}

// 所有空映射快照保留旧公共行为，挂靠快照拒绝未授权 Key。
// 不复制调度算法，直接验证公共权限实现的可见行为。
// 第一例验证普通无限制 Key 不能访问私有组。
// 第二例验证绑定 Key 与唯一组筛选共同成立时允许使用。
// 第三例验证他人的私有组不会因同一厂商而被授权。
// 第四例验证内部 Key 缺失过滤器时也不能流入公共池。
// 第五例保留旧公共组的空筛选语义，避免破坏已有平台 Key。
// 只检验公开权限方法的返回结果，不在测试中重写其算法。
// 快照测试不代替生命周期测试，数据库约束由前面的真实路径验证。
func TestHostingSnapshotPermission(t *testing.T) {
	snapshot := &state.ConfigSnapshot{HostingGroups: map[uint]uint{1: 7, 2: 8}}
	for _, test := range []struct {
		key     state.AccessKeyView
		group   uint
		allowed bool
	}{
		{state.AccessKeyView{ID: 9}, 1, false},
		{state.AccessKeyView{ID: 7, Filters: state.FilterSet{Groups: map[uint]struct{}{1: {}}}}, 1, true},
		{state.AccessKeyView{ID: 7, Filters: state.FilterSet{Groups: map[uint]struct{}{1: {}}}}, 2, false},
		{state.AccessKeyView{ID: 7}, 3, false},
		{state.AccessKeyView{ID: 9}, 3, true},
	} {
		if snapshot.AllowsGroup(test.key, test.group) != test.allowed {
			t.Fatalf("permission key=%d group=%d", test.key.ID, test.group)
		}
	}
}

// 重授权保留资源和内部 Key，只接受原先已绑定的同一身份。
// 重放激活不能恢复用户后来暂停的账号，防止跨服务旧请求越过新状态。
// 重新授权解决授权失效，不是重新分配资源的入口。
// 若产生新 Key，Oak 保存的内部凭据会立刻失效。
// 若产生新 CredentialID，原有刷新互斥和历史操作会断开。
// 若改变暂停状态，后台恢复授权会意外恢复用户调用。
// 因此本用例同时检查资源标识、Key 和业务状态。
// 再次重放新操作检查 Stage 已消费之后仍可安全重试。
// 测试账户身份完全相同，换身份场景由服务端 mismatch 分支拒绝。
// 不以成功模型发现代替真实推理验收。
func TestHostingReauthorizationKeepsResources(t *testing.T) {
	fixture := newServiceFixture(t)
	stage := hostingTestAuthorization(t, fixture, "oak:1", "one", "same", resetCreditTestKey, "connect")
	account, err := fixture.service.CreateHostingAccount(t.Context(), HostingCreateRequest{OwnerRef: "oak:1", AccountRef: "one", OperationID: resetCreditTestKey, StageID: stage.StageID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.service.UpdateHostingAccount(t.Context(), "oak:1", "one", HostingUpdateRequest{}, true); err != nil {
		t.Fatal(err)
	}
	// 先激活再暂停，模拟调用方断线后重试很早之前的激活请求。
	// 仅修改元数据不足以通过：后续检查真实 Key 状态和数据库资源 ID。
	paused := true
	if _, err := fixture.service.UpdateHostingAccount(t.Context(), "oak:1", "one", HostingUpdateRequest{Paused: &paused}, false); err != nil {
		t.Fatal(err)
	}
	replayed, err := fixture.service.UpdateHostingAccount(t.Context(), "oak:1", "one", HostingUpdateRequest{}, true)
	if err != nil || replayed.Status != "paused" {
		t.Fatalf("activation replay: %s %v", replayed.Status, err)
	}
	// 第二次浏览器授权属于原账号，使用新的操作 UUID。
	// 不能复用首次授权码，也不能把同一邮箱当作同一上游身份。
	operation := "9f0f4c32-89d2-4bcb-9e19-052940dc2f17"
	stage = hostingTestAuthorization(t, fixture, "oak:1", "one", "same", operation, "reauthorize")
	request := HostingCreateRequest{OwnerRef: "oak:1", AccountRef: "one", OperationID: operation, StageID: stage.StageID}
	updated, err := fixture.service.CreateHostingAccount(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	// 原位更新确保正式凭据刷新锁仍绑定同一 CredentialID。
	// 暂停状态不随重新授权自动恢复，由用户显式选择是否恢复服务。
	if updated.GroupID != account.GroupID || updated.CredentialID != account.CredentialID || updated.InternalKey != account.InternalKey || updated.Status != "paused" {
		t.Fatal("reauthorization changed resource identity or status")
	}
	if _, err := fixture.service.CreateHostingAccount(t.Context(), request); err != nil {
		t.Fatal(err)
	}
}

// 首个授权刚发起就要阻止文件刷新，不能等账号创建完成才设边界。
// 使用原导入入口，验证返回明确错误而不是悄悄跳过身份校验。
// 该场景发生于首次开通挂靠，尚未有持久私有组。
// 旧文件导入可能刷新同一账号，而授权随后会创建独立凭据。
// 保护范围因此需要包含进行中的授权映射。
// 测试不提前插入绑定，确保覆盖真实初始化窗口。
// 错误必须可定位，不能吞掉导入失败后伪造空 Stage。
// 账号最终是否创建不影响正在授权期间的保护要求。
// 已结束的授权与无挂靠资源时仍可按原导入规则运行。
func TestHostingPendingAuthorizationProtectsImports(t *testing.T) {
	fixture := newServiceFixture(t)
	_, err := fixture.service.BeginHostingAuthorization(t.Context(), HostingAuthorizationRequest{OwnerRef: "oak:1", AccountRef: "one", OperationID: resetCreditTestKey, Purpose: "connect"})
	if err != nil {
		t.Fatal(err)
	}
	// 这里故意不提交 OAuth 回调，数据库尚无 HostingBinding。
	// 如果保护只查 binding 表，这个请求就会进入旧的文件导入路径。
	_, err = fixture.service.ImportCredentialStage(t.Context(), channel.Codex, []byte(`{"type":"codex","access_token":"test","refresh_token":"test","account_id":"one"}`))
	var apiErr *app_errors.APIError
	if !errors.As(err, &apiErr) || apiErr.Code != "HOSTING_BROWSER_AUTH_REQUIRED" {
		t.Fatalf("pending import protection: %v", err)
	}
}
