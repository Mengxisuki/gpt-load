package control

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"gpt-load/internal/channel"
	"gpt-load/internal/execution"
	"gpt-load/internal/execution/cpa"
	"gpt-load/internal/outboundproxy"
	"gpt-load/internal/platform/config"
	app_errors "gpt-load/internal/platform/errors"
	"gpt-load/internal/protocol"
	"gpt-load/internal/scheduler"
	"gpt-load/internal/state"
	"gpt-load/internal/storage/models"
	"gpt-load/internal/subscription"
	"gpt-load/internal/subscription/providers/codex"
)

// 测试传输层仅消费请求并返回固定 Responses 完成事件，不访问真实厂商。
// 通过函数接收器保留 HTTP 请求断言，复用真实 CPA 转换和凭据准备。
type hostingProbeTransport func(*http.Request) (*http.Response, error)

// 不提供默认网络回退，未预期的请求也只能进入当前测试函数。
// 原样传递模拟传输错误，让适配器负责转换失败结果。
func (transport hostingProbeTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	return transport(request)
}

// 挂靠连接测试必须发起本人账号的 Responses 请求，不能走 API-Key 专用探测。
// 通过真实 HTTP 路由、授权状态机和数据库构造账号，仅替换厂商网络执行器。
// 回归起点是合法挂靠请求返回 403，不能通过放开普通订阅 Probe 门禁来修复。
// 本例覆盖控制 HTTP 与执行规格；真实原厂可用性不属于离线测试结论。
// 普通组的订阅禁令由已有回归独立覆盖，本测试不修改或替代该权限边界。
// 所有凭据均来自本测试的固定虚构身份，不读取开发者本地授权文件。
// 授权目录只有一个模型，使模型范围断言不依赖外部模型更新。
func TestHostingHTTPAccountProbeUsesSubscriptionResponses(t *testing.T) {
	fixture := newServiceFixture(t)
	stage := hostingTestAuthorization(t, fixture, "oak:1", "probe-account", "probe-id", resetCreditTestKey, "connect")
	account, err := fixture.service.CreateHostingAccount(t.Context(), HostingCreateRequest{OwnerRef: "oak:1", AccountRef: "probe-account", OperationID: resetCreditTestKey, StageID: stage.StageID})
	if err != nil {
		t.Fatal(err)
	}
	// 激活使用真实服务；测试入口不负责替用户恢复暂停或修改账号状态。
	// 执行层激活只表示内部 Key 已可靠保存，与 New API 的模型验证状态不同。
	// 缺少这个前置动作时应拒绝测试，不能由测试请求偷偷完成激活。
	if _, err := fixture.service.UpdateHostingAccount(t.Context(), "oak:1", "probe-account", HostingUpdateRequest{}, true); err != nil {
		t.Fatal(err)
	}
	executor := &credentialProbeTestExecutor{result: successfulCredentialProbeResult()}
	fixture.service.executor = executor
	initControlI18n(t)
	// 核验端只模拟 New API 已验证的归属，不绕过 gpt-load 的身份中间件。
	// JWT 的签名和有效期由跨服务认证专项用例覆盖，此处只关注账号测试路由。
	// 保留两个归属返回值，以实际数据库绑定验证其他用户无法访问。
	verifier := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		owner := "oak:1"
		if r.Header.Get("Authorization") == "Bearer "+hostingTestAssertion("oak:2") {
			owner = "oak:2"
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "data": map[string]string{"owner_ref": owner}})
	}))
	defer verifier.Close()
	t.Setenv("OAK_NEW_API_URL", verifier.URL)
	engine := gin.New()
	NewServer(&config.Config{AuthKey: "test-admin"}, fixture.service).RegisterRoutes(engine)
	// 每次 HTTP 请求携带归属断言；未知账号与非本人账号遵守相同隐藏规则。
	// 请求模型及协议均走 JSON 解码，避免直接调用服务跳过接口契约。
	// 服务成功信封必须能解码为原有 ProbeResponse，保证 New API 可读取。
	probe := func(owner, body string) *httptest.ResponseRecorder {
		return serveCredentialRequest(t, engine, http.MethodPost, "/api/hosting/v1/accounts/probe-account/test?owner_ref="+url.QueryEscape(owner), body, hostingTestAssertion(owner), "")
	}
	const body = `{"model":"gpt-test","protocol":"openai-responses"}`
	response := probe("oak:1", body)
	if response.Code != http.StatusOK {
		t.Fatalf("probe HTTP %d: %s", response.Code, response.Body)
	}
	var envelope struct {
		Data CredentialProbeResponse `json:"data"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	// 成功必须来自一次真实执行；目录可见不能直接转换为 passed。
	// 不产生 restore proof，普通用户不能把诊断结果用于后台凭据恢复。
	// 使用执行次数区分“一次成功调用”和只检查本地配置的假成功。
	if envelope.Data.Outcome != CredentialProbeOutcomePassed || envelope.Data.CanRestore || envelope.Data.RestoreProof != nil {
		t.Fatalf("unexpected result: %#v", envelope.Data)
	}
	calls := executor.recordedCalls()
	if len(calls) != 1 {
		t.Fatalf("execution count = %d", len(calls))
	}
	call := calls[0]
	// 精确绑定凭据、模型和原生操作，防止误用公共账号或通用 Probe 路由。
	// 协议和路径同时检查，避免只修改 operation 却保留旧探测请求体。
	// 失败诊断只打印操作名与内部测试 ID，不能输出完整 CredentialSnapshot。
	if call.Credential.ID != account.CredentialID || call.ChannelID != string(channel.Codex) || call.Operation != execution.OperationResponsesCreate || call.ClientProtocol != protocol.OpenAIResponses || call.UpstreamModel != "gpt-test" || call.Path != "/v1/responses" || call.Method != http.MethodPost {
		t.Fatalf("wrong subscription request: operation=%s credential=%d", call.Operation, call.Credential.ID)
	}
	if err := call.Validate(); err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	// store=false 是账号测试的边界，不能因普通 Responses 默认存储而遗留会话。
	// 输入内容由服务固定生成，本断言只关心存在必要字段，不复制生成实现。
	if json.Unmarshal(call.Body, &payload) != nil || payload["model"] != "gpt-test" || payload["store"] != false || payload["input"] == nil {
		t.Fatal("invalid stateless probe body")
	}
	// 再将入口实际构造的请求送入真实订阅适配器，防止桩接受了错误操作。
	// 虚构令牌仍有效，因此不需要刷新；身份与版本由真实凭据管理器校验。
	// 适配器会重新核验渠道声明，能识别只绕过 subscription 门禁的错误修复。
	// 本次复用的是入口捕获的同一个规格，不为测试单独拼装合法请求。
	// 数据库、注册表及身份代数均来自真实授权流程，准备阶段也必须通过。
	manager := subscription.NewCredentialManager(fixture.db, fixture.encryption, fixture.registry, fixture.mutations, fixture.service.subscriptions)
	adapter := cpa.NewAdapter(manager, fixture.channelRegistry)
	transportCalls := 0
	transport := hostingProbeTransport(func(request *http.Request) (*http.Response, error) {
		transportCalls++
		raw, err := io.ReadAll(request.Body)
		if err != nil {
			return nil, err
		}
		// 这里只检查虚构身份，失败输出不能包含 Authorization 或完整正文。
		// 传输层只观察最终厂商 HTTP 形状，不能替适配器补缺失的授权头。
		// 原厂账号头与 Bearer 必须同时属于选定账号，防止混用授权材料。
		if request.Header.Get("Authorization") != "Bearer access-probe-id" || request.Header.Get("Chatgpt-Account-Id") != "probe-id" {
			t.Error("subscription identity was not forwarded")
		}
		var upstream map[string]any
		// 即使 CPA 重写了兼容字段，最终模型与无状态约束仍应保持。
		// 返回 Codex 实际使用的 SSE 完成事件，验证 unary 聚合路径可读取。
		// 空 output 只用于网络桩，不把生成内容质量作为连接通过条件。
		if json.Unmarshal(raw, &upstream) != nil || upstream["model"] != "gpt-test" || upstream["store"] != false {
			t.Error("invalid provider request")
		}
		response := "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_probe\",\"object\":\"response\",\"status\":\"completed\",\"model\":\"gpt-test\",\"output\":[],\"usage\":{\"input_tokens\":4,\"output_tokens\":1,\"total_tokens\":5}}}\n\n"
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(response)), Request: request}, nil
	})
	// 显式代理会覆盖 CPA 的测试 transport，只在副本上取消代理以封锁外部网络。
	// 生产请求保留账号网络设置；上面的规格断言检查的是入口原始请求。
	// transport 本身没有默认网络实现，即使 URL 错误也不能访问真实账号。
	// 此副本只在测试中存在，不改写注册表、数据库或生产代理选择。
	providerSpec := call.Clone()
	providerSpec.Proxy = outboundproxy.Effective{}
	providerCtx := context.WithValue(t.Context(), "cliproxy.roundtripper", http.RoundTripper(transport))
	providerResult := adapter.Execute(providerCtx, providerSpec)
	if providerResult.Error != nil || providerResult.StatusCode != http.StatusOK || transportCalls != 1 {
		t.Fatalf("provider adapter failed: status=%d calls=%d error=%v", providerResult.StatusCode, transportCalls, providerResult.Error)
	}
	// 不允许通过测试接口提交任意 prompt、跨账号或扩展协议。
	// 拒绝必须发生在执行器之前，避免非法请求也消耗用户额度。
	// 他人归属即使知道 account_ref 也不能触发模型执行。
	// 目录外模型应在本地拒绝，而不是依赖原厂拒绝来实现权限范围。
	// 普通 Completions 支持不意味着本测试入口允许扩展为其他协议。
	// 空 JSON 不能触发默认模型选择，否则 UI 未选择模型也可能产生费用。
	for _, tc := range []struct{ owner, body string }{{"oak:2", body}, {"oak:1", `{"model":"missing-model","protocol":"openai-responses"}`}, {"oak:1", `{"model":"gpt-test","protocol":"openai-completions"}`}, {"oak:1", `{}`}} {
		if got := probe(tc.owner, tc.body); got.Code == http.StatusOK {
			t.Fatalf("invalid request accepted: %s", got.Body)
		}
	}
	if len(executor.recordedCalls()) != 1 {
		t.Fatal("invalid request reached upstream")
	}
	// 上游拒绝、限流和超时保持稳定分类，不得被 HTTP 200 信封误写为成功。
	// 401 表示凭据拒绝，只有明确结果才归类为 failed。
	// 429 与网络超时无法证明账号失效，必须保留 inconclusive。
	// 每个错误也要实际经过 HTTP 序列化，保证 reason 没有被安全投影丢掉。
	// 对错误结果不进行第二次执行，避免诊断请求隐式消耗更多额度。
	for _, tc := range []struct {
		result  execution.AttemptResult
		outcome CredentialProbeOutcome
		reason  CredentialProbeReason
	}{
		{failedCredentialProbeResult(401, execution.ErrorKindHTTP, execution.FailureHintInvalidCredential), CredentialProbeOutcomeFailed, CredentialProbeReasonInvalidCredential},
		{failedCredentialProbeResult(429, execution.ErrorKindHTTP, ""), CredentialProbeOutcomeInconclusive, CredentialProbeReasonRateLimited},
		{failedCredentialProbeResult(0, execution.ErrorKindTimeout, ""), CredentialProbeOutcomeInconclusive, CredentialProbeReasonTimeout},
	} {
		executor.result = tc.result
		got := probe("oak:1", body)
		if got.Code != http.StatusOK || json.Unmarshal(got.Body.Bytes(), &envelope) != nil {
			t.Fatalf("business result lost: %s", got.Body)
		}
		if envelope.Data.Outcome != tc.outcome || envelope.Data.Reason == nil || *envelope.Data.Reason != tc.reason {
			t.Fatalf("wrong failure result: %#v", envelope.Data)
		}
	}
	// 暂停及删除都禁止新的推理测试，读取额度的权限不等于调用模型的权限。
	// 暂停用公开服务动作执行，不能直接改绑定表来绕过内部 Key 联动。
	// 删除后需要返回不存在，不能仅把上游凭据缺失包装为未知测试结果。
	// 最后再次检查执行次数，防止失败响应之前已经偷偷请求过厂商。
	paused := true
	if _, err := fixture.service.UpdateHostingAccount(t.Context(), "oak:1", "probe-account", HostingUpdateRequest{Paused: &paused}, false); err != nil {
		t.Fatal(err)
	}
	before := len(executor.recordedCalls())
	if got := probe("oak:1", body); got.Code == http.StatusOK {
		t.Fatal("paused account tested")
	}
	if err := fixture.service.DeleteHostingAccount(t.Context(), "oak:1", "probe-account"); err != nil {
		t.Fatal(err)
	}
	if got := probe("oak:1", body); got.Code != http.StatusNotFound {
		t.Fatalf("deleted account HTTP %d", got.Code)
	}
	if len(executor.recordedCalls()) != before {
		t.Fatal("inactive account reached upstream")
	}
}

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
// 挂靠断言允许本人请求；管理员凭据不具有此命名空间的权限。
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
// 核验桩只接受两名测试用户的专用断言，不接受管理员Key。
// 两名用户都能通过身份验证，但只有资源所有者能读到重置结果。
// 他人请求得到404，避免确认账号或未决操作是否存在。
// 私有内部Key是真实建号返回值，用来覆盖数据面凭据的用途隔离。
// 列表之外的重置查询也必须接入新模块，不能保留旧管理认证路径。
// 认证和业务查询使用同一个注册器，覆盖生产模块组合方式。
// 默认断言核验只读，不会改变预置的outcome_unknown状态。
// 未决解绑的停用行为由原服务执行，不由新认证层模拟。
// Stage换码仍用厂商桩，测试不需要用户登录或访问原厂网络。
// 凭据继续保留的断言避免误把“删除请求失败”实现成部分数据删除。
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
	verifier := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		owner := ""
		for _, candidate := range []string{"oak:1", "oak:2"} {
			if r.Header.Get("Authorization") == "Bearer "+hostingTestAssertion(candidate) {
				owner = candidate
			}
		}
		if owner != "oak:1" && owner != "oak:2" {
			w.WriteHeader(401)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "data": map[string]string{"owner_ref": owner}})
	}))
	defer verifier.Close()
	t.Setenv("OAK_NEW_API_URL", verifier.URL)
	engine := gin.New()
	NewServer(&config.Config{AuthKey: "test-admin"}, fixture.service).RegisterRoutes(engine)
	for _, test := range []struct {
		owner, key string
		status     int
	}{{"oak:1", hostingTestAssertion("oak:1"), 200}, {"oak:2", hostingTestAssertion("oak:2"), 404}, {"oak:1", "test-admin", 401}, {"oak:1", account.InternalKey, 401}, {"oak:1", "", 401}} {
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

// 真实路由链只替换外部 New API 核验服务，不替换本地鉴权和归属逻辑。
// 核验桩按已签请求快照检查字段，确保发送的是实际方法、URI 和原始字节摘要。
// 短期断言与普通后台、数据面凭据分别测试，不能混用权限域。
// 过期和伪造令牌由身份服务拒绝；本层必须将拒绝传播到业务入口之前。
// 请求字节被更改时即使 JSON 意义类似也不能通过原断言。
// 归属匹配另设“核验成功但请求 owner 错误”，证明不是只依赖外部桩隔离。
// 所有测试使用隔离数据库和回环端口，不读取或修改生产账号。
// URL中的冒号使用百分号转义，用来发现核验前擅自重编码的错误。
// read断言只授权GET列表，改成POST即使路径相同也应失效。
// POST列表路径有真实建号路由，方法篡改不会被405提前挡住。
// 不存在的方法由通用注册器终止，不能用其405假冒认证回归通过。
// 修改账号路径测试资源定位同样受签名请求URI约束。
// GET附加正文测试空体哈希不能被简单忽略。
// 修改幂等头测试消费重试标识不能脱离原请求断言。
// wrong-query断言专门返回与query不同的用户，覆盖本地再次比对。
// duplicate-query即使两个值相同也拒绝，固定唯一解析语义。
// 授权和创建的owner在body中，两个入口分别覆盖，避免只修列表读取。
// 普通后台会话路由作为反向边界，挂靠断言不能充当管理员。
// 原管理员成功用例确保隔离没有顺便破坏既有登录。
// 临时失败不做数据库清理或状态修复，业务不应在认证前执行。
// 回调阶段和真实原厂限额不在这个请求认证测试的责任内。
func TestHostingRequestVerificationIsolation(t *testing.T) {
	fixture := newServiceFixture(t)
	initControlI18n(t)
	type assertion struct {
		request hostingVerificationRequest
		owner   string
	}
	// 这里保存的是外部核验服务的预期输入，不实现 JWT 的签发或验证算法。
	// 真正的过期、域隔离和签名回归由 New API 验证器负责。
	tickets := map[string]assertion{}
	add := func(token, method, uri, body, key, owner string) {
		digest := sha256.Sum256([]byte(body))
		tickets[hostingTestAssertion(token)] = assertion{hostingVerificationRequest{method, uri, hex.EncodeToString(digest[:]), key}, owner}
	}
	listURI := "/api/hosting/v1/accounts?owner_ref=oak%3A1"
	add("read", "GET", listURI, "", "", "oak:1")
	add("wrong-query", "GET", "/api/hosting/v1/accounts?owner_ref=oak%3A2", "", "", "oak:1")
	add("duplicate-query", "GET", listURI+"&owner_ref=oak%3A1", "", "", "oak:1")
	add("post", "POST", "/api/hosting/v1/authorizations", `{"owner_ref":"oak:2"}`, "original-key", "oak:1")
	add("create", "POST", "/api/hosting/v1/accounts", `{"owner_ref":"oak:2"}`, "", "oak:1")
	// 验证服务收到的字段必须完整相等，任一不匹配统一返回401。
	// 校验端不返回“不匹配的哪一项”，防止调用方获得令牌内部信息。
	var verificationCalls atomic.Int64
	verifier := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		verificationCalls.Add(1)
		if r.Method != "POST" || r.URL.RequestURI() != "/api/internal/hosting/verify-request" {
			t.Error("unexpected verification target")
			w.WriteHeader(500)
			return
		}
		var got hostingVerificationRequest
		want, ok := tickets[strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")]
		if json.NewDecoder(r.Body).Decode(&got) != nil || !ok || got != want.request {
			w.WriteHeader(401)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "data": map[string]string{"owner_ref": want.owner}})
	}))
	defer verifier.Close()
	t.Setenv("OAK_NEW_API_URL", verifier.URL)
	engine := gin.New()
	NewServer(&config.Config{AuthKey: "test-admin"}, fixture.service).RegisterRoutes(engine)
	// GET成功是完整公开行为验证，其他变体均不得获得账号列表或创建 Stage。
	// 创建和授权正文均检查已核验 owner，不能自报别人的归属。
	for _, tc := range []struct {
		name, token, method, uri, body, key string
		status                              int
	}{
		{"valid", "read", "GET", listURI, "", "", 200},
		{"tampered-owner", "read", "GET", "/api/hosting/v1/accounts?owner_ref=oak%3A2", "", "", 401},
		{"tampered-path", "read", "GET", "/api/hosting/v1/accounts/other?owner_ref=oak%3A1", "", "", 401},
		{"tampered-method", "read", "POST", listURI, "", "", 401},
		{"tampered-body", "read", "GET", listURI, "{}", "", 401},
		{"tampered-idempotency", "read", "GET", listURI, "", "changed", 401},
		{"wrong-query-owner", "wrong-query", "GET", "/api/hosting/v1/accounts?owner_ref=oak%3A2", "", "", 401},
		{"duplicate-query-owner", "duplicate-query", "GET", listURI + "&owner_ref=oak%3A1", "", "", 401},
		{"wrong-body-owner", "post", "POST", "/api/hosting/v1/authorizations", `{"owner_ref":"oak:2"}`, "original-key", 401},
		{"wrong-create-owner", "create", "POST", "/api/hosting/v1/accounts", `{"owner_ref":"oak:2"}`, "", 401},
		{"expired", "expired", "GET", listURI, "", "", 401},
		{"admin", "test-admin", "GET", listURI, "", "", 401},
		{"login-jwt", "ordinary-login-jwt", "GET", listURI, "", "", 401},
		{"access-key", "ordinary-access-key", "GET", listURI, "", "", 401},
		{"assertion-not-admin", "read", "GET", "/api/auth/session", "", "", 401},
		{"admin-unchanged", "test-admin", "GET", "/api/auth/session", "", "", 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.uri, strings.NewReader(tc.body))
			token := tc.token
			switch tc.token {
			case "test-admin", "ordinary-access-key":
			case "ordinary-login-jwt":
				token = "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.e30.signature"
			default:
				token = hostingTestAssertion(tc.token)
			}
			before := verificationCalls.Load()
			req.Header.Set("Authorization", "Bearer "+token)
			if tc.key != "" {
				req.Header.Set("Idempotency-Key", tc.key)
			}
			result := httptest.NewRecorder()
			engine.ServeHTTP(result, req)
			if result.Code != tc.status {
				t.Fatalf("status=%d want=%d body=%s", result.Code, tc.status, result.Body)
			}
			if (tc.name == "admin" || tc.name == "login-jwt" || tc.name == "access-key" || tc.name == "assertion-not-admin") && verificationCalls.Load() != before {
				t.Fatal("unrelated credential sent to verifier")
			}
		})
	}

	// 格式过滤是负向门禁：字段缺失、坏Base64、非JSON和错误算法均不出网。
	// 4096字节限制在解析前执行，不能让超长令牌穿过前端代理进入验证器。
	// 每个请求比较核验计数，明确证明敏感凭据没有被转发。
	// 有效外形但恰好等于管理员密钥时也必须在本地拒绝。
	// 算法为RS256但用途相同仍拒绝，不能让服务间算法协商扩大信任域。
	// 空payload与坏头部各走独立格式检查，均不触发远端验证。
	// JSON null不是可用JWT头部，即使解析无错误也缺少必需类型。
	// 测试令牌最大长度以整个Bearer值计量，不只限制header段。
	// 特殊管理员用新的Server实例，避免修改运行时密钥摘要自证。
	// 原子计数跨HTTP服务goroutine观察网络调用，兼容竞态检测。
	// 请求完成后检查次数，不依赖等待或猜测异步调用已经结束。
	// 测试数据从不含真实管理员秘密，失败信息也不回显令牌。
	// 这些检查不依赖外部核验桩的返回值，避免“发出去再拒绝”的假通过。
	for _, token := range []string{"broken", "%%%.e30.signature", "e30..signature", "bnVsbA.e30.signature", "eyJhbGciOiJSUzI1NiIsInR5cCI6Im9hay1ob3N0aW5nLXJlcXVlc3Qrand0In0.e30.signature", strings.Repeat("a", 4097)} {
		before := verificationCalls.Load()
		req := httptest.NewRequest("GET", listURI, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		result := httptest.NewRecorder()
		engine.ServeHTTP(result, req)
		if result.Code != 401 || verificationCalls.Load() != before {
			t.Fatal("invalid format forwarded")
		}
	}
	shapedAdmin := hostingTestAssertion("shaped-admin")
	adminEngine := gin.New()
	NewServer(&config.Config{AuthKey: shapedAdmin}, fixture.service).RegisterRoutes(adminEngine)
	before := verificationCalls.Load()
	req := httptest.NewRequest("GET", listURI, nil)
	req.Header.Set("Authorization", "Bearer "+shapedAdmin)
	result := httptest.NewRecorder()
	adminEngine.ServeHTTP(result, req)
	if result.Code != 401 || verificationCalls.Load() != before {
		t.Fatal("administrator secret forwarded")
	}
	// 所有被拒绝的创建入口不能留下未授权的持久 Stage。
	// 断言签名正确也不足以跨用户访问，业务归属仍由服务层查询约束。
	var count int64
	if err := fixture.db.Model(&models.HostingAuthorization{}).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("unauthorized stage persisted: %d %v", count, err)
	}
}

// 身份服务故障时挂靠保持关闭，不能把未知结果当认证成功。
// 重定向和环境代理均不能把断言发向配置以外的地址。
// 响应限长覆盖有效JSON加大量空白，避免只限制解码对象而漏掉尾部。
// 无效配置在路由创建时捕获，普通后台服务仍能运行。
// 所有异常返回固定错误码，不回显外部服务正文和断言。
// 401代表用户断言未获认证；500代表核验服务基础设施失败。
// 本层将基础设施故障映射503，让调用者区分身份拒绝和服务不可用。
// 不把核验端的错误JSON当业务成功，不接受错误码包装下的data。
// 302用同机捕获服务验证没有跟随重定向，避免测试实际访问外网。
// malformed覆盖HTTP成功但正文不可解析，不能静默返回空账号列表。
// success:false覆盖只检查HTTP状态的错误实现。
// 缺失owner覆盖成功标志存在但没有可授权身份的情况。
// 超长响应在完整读取上限处拒绝，不能忽略大量尾随空白。
// 失联端口使用已关闭测试服务器，不依赖开发机固定空闲端口。
// 因超时与断连共用网络错误分支，这里用即时拒绝连接稳定复现关闭。
// 单测不睡眠等待三秒，实际总超时由http.Client配置保证。
// 无效配置测试不启网络服务，以保证错误目标不会收到请求。
// localhost与公网字面IP都拒绝，分别覆盖名字解析和地址边界。
// 用户信息、路径、查询、fragment是独立限制，逐项覆盖。
// HTTP和HTTPS代理环境均设置为捕获端点，核验客户端不读取它们。
// 此测试不模拟TLS握手，也不宣称跨主机传输已经验证。
// 失败响应中加入虚构秘密标记，确认对客户端错误输出已脱敏。
// 捕获端点计数最终必须为零，任何额外访问都使回归失败。
// 所有子例新建路由以读取各自配置，符合生产只在启动时配置的行为。
// 本地测试的临时身份服务仅代表外部边界，不取代New API密码学测试。
func TestHostingVerificationFailsClosed(t *testing.T) {
	initControlI18n(t)
	fixture := newServiceFixture(t)
	var redirected atomic.Int64
	trap := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { redirected.Add(1); w.WriteHeader(200) }))
	defer trap.Close()
	// 即便进程环境设置了代理，此内部验证客户端也不应调用它。
	// 回环地址在系统代理中通常有例外，本测试仍与配置字段静态检查共同覆盖。
	t.Setenv("HTTP_PROXY", trap.URL)
	t.Setenv("HTTPS_PROXY", trap.URL)
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{"rejected", 401, `{"secret":"not-for-client"}`},
		{"failed", 500, `{"secret":"not-for-client"}`},
		{"redirect", 302, ""},
		{"malformed", 200, "broken-json"},
		{"false", 200, `{"success":false}`},
		{"missing-owner", 200, `{"success":true,"data":{}}`},
		{"oversized", 200, `{"success":true,"data":{"owner_ref":"oak:1"}}` + strings.Repeat(" ", 4096)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			verifier := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Location", trap.URL)
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer verifier.Close()
			t.Setenv("OAK_NEW_API_URL", verifier.URL)
			engine := gin.New()
			NewServer(&config.Config{AuthKey: "admin"}, fixture.service).RegisterRoutes(engine)
			req := httptest.NewRequest("GET", "/api/hosting/v1/accounts?owner_ref=oak%3A1", nil)
			req.Header.Set("Authorization", "Bearer "+hostingTestAssertion("failure"))
			result := httptest.NewRecorder()
			engine.ServeHTTP(result, req)
			want := 503
			if tc.status == 401 {
				want = 401
			}
			if result.Code != want || strings.Contains(result.Body.String(), "not-for-client") {
				t.Fatalf("unsafe verification response: %d %s", result.Code, result.Body)
			}
		})
	}
	// 非回环目标和URI附加内容必须在发起网络调用前拒绝。
	// localhost 被拒是为了避免DNS和hosts映射改变固定身份服务。
	for _, endpoint := range []string{"http://localhost:3000", "http://192.0.2.1:3000", "http://127.0.0.1:3000/path", "http://127.0.0.1:3000?x=1", "http://user@127.0.0.1:3000", "http://127.0.0.1:3000#x"} {
		t.Setenv("OAK_NEW_API_URL", endpoint)
		if _, err := hostingVerificationURL(); err == nil {
			t.Errorf("unsafe endpoint accepted: %s", endpoint)
		}
	}
	// 已关闭的回环端口模拟身份服务停止，不能回退到管理员认证。
	stopped := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	stopped.Close()
	t.Setenv("OAK_NEW_API_URL", stopped.URL)
	engine := gin.New()
	NewServer(&config.Config{AuthKey: "admin"}, fixture.service).RegisterRoutes(engine)
	req := httptest.NewRequest("GET", "/api/hosting/v1/accounts?owner_ref=oak%3A1", nil)
	req.Header.Set("Authorization", "Bearer "+hostingTestAssertion("failure"))
	result := httptest.NewRecorder()
	engine.ServeHTTP(result, req)
	if result.Code != 503 || redirected.Load() != 0 {
		t.Fatalf("fail-open or redirect: status=%d redirected=%d", result.Code, redirected.Load())
	}
}

// 测试只构造专用头部与虚构签名，不在执行层复制真实签发逻辑。
// payload标签供外部核验桩定位预期请求，不代表可受信的用户身份。
// 签名字段刻意无效，只有测试中的回环桩接受；真实New API必须拒绝。
// 普通凭据用例不经过此函数，证明本地筛选在网络调用前生效。
func hostingTestAssertion(label string) string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"HS256","typ":"oak-hosting-request+jwt"}`))
	return header + "." + base64.RawURLEncoding.EncodeToString([]byte(label)) + ".signature"
}
