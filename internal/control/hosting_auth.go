package control

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/sirupsen/logrus"
	app_errors "gpt-load/internal/platform/errors"
	"gpt-load/internal/platform/httproute"
	"gpt-load/internal/platform/i18n"
	"gpt-load/internal/platform/utils"
)

// 已核验归属仅保存在当前请求上下文，不接受浏览器自报身份。
// 这是挂靠专用权限，不设置管理员或数据面 AccessKey principal。
// 已核验身份不返回浏览器，也不作为下一请求可复用的会话。
const hostingOwnerContextKey = "gpt-load.hosting.owner"

// 核验只发送请求摘要，不把原厂回调、授权码和内部 Key 传回身份服务。
// 字段按原始 HTTP 请求计算；查询串、JSON 字节及幂等标识均绑定断言。
// 空体同样有确定的 SHA256，不能用空字符串代替。
// request_uri 保留转义及查询顺序，不先解码重组后再核验。
// body_sha256 为小写十六进制，不能改成 Base64 或重新序列化 JSON。
// 幂等头为空时也发送空字符串，不能省略后由验证端猜测默认值。
// 这四项由执行端计算，客户端传入同名 JSON 字段不能覆盖它们。
type hostingVerificationRequest struct {
	Method         string `json:"method"`
	RequestURI     string `json:"request_uri"`
	BodySHA256     string `json:"body_sha256"`
	IdempotencyKey string `json:"idempotency_key"`
}

// HostingHTTPModule 将用户挂靠从管理员认证链分离。
// 注册器的 AuthControl 是控制面类别，具体认证由独立中间件执行。
// 不添加匿名例外，也不让短期断言进入普通管理员路由。
// 更具体的命名空间沿用注册器的 404/405 终止规则，不进入业务。
// 数据面路由和每账号受限 Key 的权限不变。
// 注册时显式提供认证器，不能用 AuthNone 把内部地址当成权限保证。
// 普通 /api 模块仍独立注册，不通过路径分支绕过其认证链。
// 路由级审计在认证后执行，未认证请求由专用认证审计记录。
func (s *Server) HostingHTTPModule() httproute.Module {
	return httproute.Module{
		Name: "hosting", Owner: httproute.OwnerControl, Auth: httproute.AuthControl,
		Prefix: "/api", NamespacePrefixes: []string{"/api/hosting/v1"},
		BeforeAuth: gin.HandlersChain{i18n.Middleware()}, Authenticate: s.authenticateHosting(),
		NotFound: controlRouteNotFound, MethodNotAllowed: controlMethodNotAllowed,
		Routes: s.hostingRoutes(),
	}
}

// 只允许部署者配置字面回环地址，验证终点不能由请求或令牌选择。
// 禁止 DNS 名称、用户信息、路径、查询和 fragment，消除任意回调能力。
// 默认端口对应同机 New API；该配置不是服务秘密。
// HTTP 仅用于同机回环，不承诺适用于跨机器明文部署。
// 配置错误使挂靠不可用，不影响普通后台登录或降级到管理 Key。
// 配置仅在注册时读取，运维修改后需要重启使新端点生效。
// 回环目标使用原生 HTTP 连接，不借助系统代理或配置中的跳板。
// 允许 IPv4 和 IPv6 字面回环，但不允许 zone、远程地址或名字解析。
// 尾部斜杠也作为路径拒绝，防止配置与文档约定出现隐式差异。
func hostingVerificationURL() (string, error) {
	raw := os.Getenv("OAK_NEW_API_URL")
	if raw == "" {
		raw = "http://127.0.0.1:3000"
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return "", app_errors.ErrValidation
	}
	ip := net.ParseIP(parsed.Hostname())
	if (parsed.Scheme != "http" && parsed.Scheme != "https") || ip == nil || !ip.IsLoopback() || parsed.User != nil || parsed.Path != "" || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" || parsed.Opaque != "" {
		return "", app_errors.ErrValidation
	}
	return parsed.String() + "/api/internal/hosting/verify-request", nil
}

// hostingAssertionFormat 只作负向筛选，不信任尚未验签的头部声明。
// 管理Key、数据面Key和普通登录JWT不能被转发给New API验证端。
// 专用typ与算法仅表示值得核验的格式，绝不直接建立主体权限。
// 三段均须存在，头部必须是标准无填充Base64URL中的JSON对象。
// 上限与New API保持4096字节，避免放大跨服务输入或截断后继续解析。
// 即使格式正确，也必须经过固定New API端点的签名与请求绑定校验。
// 不将普通JWT的默认typ视为兼容格式，避免错把登录身份当服务授权。
// 格式失败不产生核验网络请求，减少把其他用途秘密传到身份服务的风险。
// 本函数不判断用户、签名或时间，不可单独用作身份校验器。
func hostingAssertionFormat(token string) bool {
	if len(token) > 4096 {
		return false
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return false
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return false
	}
	var header struct {
		Algorithm string `json:"alg"`
		Type      string `json:"typ"`
	}
	return json.Unmarshal(raw, &header) == nil && header.Algorithm == "HS256" && header.Type == "oak-hosting-request+jwt"
}

// 鉴权只向固定本机身份服务查询，不使用 AUTH_KEY、会话 Cookie 或 AccessKey。
// 三秒总超时包含连接和读响应；失败不会继续执行任何业务动作。
// 独立 Transport 禁止环境代理，重定向也一律不跟随。
// 默认禁用压缩，响应体大小限制不会被解压后绕过。
// 客户端在路由注册时建立并复用连接，不在每次请求新建连接池。
// 不重试核验请求、不缓存身份结果，停用用户由 New API 实时判断。
// New API 负责签名、域、用途、30 秒时效和用户状态，本层不复制其用户库。
// 不读取 JWT 内未验证的 owner；只有验证服务的成功响应可建立身份。
// 核验是只读操作，同断言有效期内可重复核验，不等于一次性消费票据。
// 写操作仍由原有 operation UUID 持久幂等；这里不建立第二套 nonce 存储。
// 请求被取消时同步取消核验，不让已断开的请求继续建立授权。
// 认证故障不应触发创建、激活、刷新或原厂消费等后续动作。
// 认证成功仅授权当前路由动作，不授权任意管理 API 或其他账号。
// TLS 使用 Go 默认校验，不为本地 HTTPS 引入跳过证书验证选项。
func (s *Server) authenticateHosting() gin.HandlerFunc {
	endpoint, configErr := hostingVerificationURL()
	client := &http.Client{
		Timeout:       3 * time.Second,
		Transport:     &http.Transport{Proxy: nil, DisableCompression: true, MaxIdleConns: 8, IdleConnTimeout: 30 * time.Second},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	return func(c *gin.Context) {
		authenticated := false
		// 拒绝事件只记录固定路由模板和状态，不记录含 owner 的查询串。
		// 与成功变更审计分离，可定位认证前失败且不假装已经进入业务。
		// 不存令牌指纹、原始响应或回调正文，避免审计变成凭据泄露源。
		// 此处不使用管理员失败锁；一个用户的断言失效不能锁死全站内网调用。
		defer func() {
			if !authenticated {
				utils.LogPlaneBestEffort(s.logger, logrus.WarnLevel, utils.LogPlaneControl, logrus.Fields{
					"event": "hosting_auth_failed", "route": c.FullPath(), "status_code": c.Writer.Status(),
				}, "Hosting authorization failed")
			}
		}()
		// no-store 也覆盖认证失败，不能缓存含内部凭据的控制响应。
		// 认证头只接受一个值，防止上下游对重复头采用不同解释。
		setSecretResponseHeaders(c)
		fields := strings.Fields(c.GetHeader("Authorization"))
		if len(c.Request.Header.Values("Authorization")) != 1 || len(fields) != 2 || !strings.EqualFold(fields[0], "Bearer") || !hostingAssertionFormat(fields[1]) {
			hostingReply(c, nil, app_errors.ErrUnauthorized)
			c.Abort()
			return
		}
		// 即使管理员碰巧设置了符合断言外形的密钥，也不能把它转发出去。
		// 复用既有固定时间摘要比较，不引入管理员明文读取或暴露接口。
		credentialDigest := sha256.Sum256([]byte(fields[1]))
		if s.compareDigest(credentialDigest[:], s.authDigest[:]) == 1 {
			hostingReply(c, nil, app_errors.ErrUnauthorized)
			c.Abort()
			return
		}
		// 请求体上限与现有挂靠 JSON 入口一致，读取后恢复给真实 handler。
		// 不信任 Content-Length，流式输入也只能读取至上限加一字节。
		body, err := io.ReadAll(io.LimitReader(c.Request.Body, 16*1024+1))
		if err != nil || len(body) > 16*1024 {
			hostingReply(c, nil, app_errors.ErrInvalidJSON)
			c.Abort()
			return
		}
		c.Request.Body = io.NopCloser(bytes.NewReader(body))
		if configErr != nil {
			hostingVerificationUnavailable(c)
			return
		}
		// 多个幂等头无法形成唯一签名语义，必须在网络调用前拒绝。
		if len(c.Request.Header.Values("Idempotency-Key")) > 1 {
			hostingReply(c, nil, app_errors.ErrUnauthorized)
			c.Abort()
			return
		}
		digest := sha256.Sum256(body)
		payload, err := json.Marshal(hostingVerificationRequest{
			Method: c.Request.Method, RequestURI: c.Request.URL.RequestURI(),
			BodySHA256: hex.EncodeToString(digest[:]), IdempotencyKey: c.GetHeader("Idempotency-Key"),
		})
		if err != nil {
			hostingVerificationUnavailable(c)
			return
		}
		// 仅转发断言，不转发客户端 Cookie、Host 或代理头。
		// 日志和错误消息均不包含断言、请求体或验证端响应正文。
		request, err := http.NewRequestWithContext(c.Request.Context(), http.MethodPost, endpoint, bytes.NewReader(payload))
		if err != nil {
			hostingVerificationUnavailable(c)
			return
		}
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Authorization", "Bearer "+fields[1])
		result, err := client.Do(request)
		if err != nil {
			hostingVerificationUnavailable(c)
			return
		}
		defer result.Body.Close()
		if result.StatusCode == http.StatusUnauthorized || result.StatusCode == http.StatusForbidden {
			hostingReply(c, nil, app_errors.ErrUnauthorized)
			c.Abort()
			return
		}
		// 只接受明确的 200 成功信封，5xx、重定向和畸形结果全部关闭。
		// 核验端故障与无效用户凭据分别报告 503 和 401。
		raw, err := io.ReadAll(io.LimitReader(result.Body, 4097))
		if err != nil || len(raw) > 4096 || result.StatusCode != http.StatusOK {
			hostingVerificationUnavailable(c)
			return
		}
		var verified struct {
			Success bool `json:"success"`
			Data    struct {
				OwnerRef string `json:"owner_ref"`
			} `json:"data"`
		}
		if json.Unmarshal(raw, &verified) != nil || !verified.Success || !validHostingRef(verified.Data.OwnerRef) {
			hostingVerificationUnavailable(c)
			return
		}
		// 归属唯一来源是已验证的断言，query 只允许声明同一个 owner。
		// 创建/授权在 body 中携带归属，由对应 handler 在解析后比较。
		// 拒绝重复查询项，不能让不同解析器选择不同 owner。
		query, err := url.ParseQuery(c.Request.URL.RawQuery)
		owners := query["owner_ref"]
		bodyOwnerRoute := c.Request.Method == http.MethodPost && (c.FullPath() == "/api/hosting/v1/authorizations" || c.FullPath() == "/api/hosting/v1/accounts")
		if err != nil || (!bodyOwnerRoute && len(owners) != 1) || len(owners) > 1 || (len(owners) == 1 && owners[0] != verified.Data.OwnerRef) {
			hostingReply(c, nil, app_errors.ErrUnauthorized)
			c.Abort()
			return
		}
		// 业务层继续以 owner/account 双条件查询，认证成功不替代对象权限。
		// 只将已验证的归属放入上下文，不将整个核验响应暴露给后续 handler。
		c.Set(hostingOwnerContextKey, verified.Data.OwnerRef)
		authenticated = true
		c.Next()
	}
}

// 核验基础设施故障使用独立固定错误码，不透出秘密或网络细节。
// 明确中止 Gin 链，防止出现失败响应后仍触发消费的情况。
// 调用者应恢复身份服务，不能拿管理员凭据重试绕过。
// 该错误不暗示厂商状态，也不能被当作账号额度不足自动跨来源重放。
// 不改变任何账号状态，恢复核验服务后原请求可按原幂等标识再确认。
func hostingVerificationUnavailable(c *gin.Context) {
	hostingReply(c, nil, &app_errors.APIError{HTTPStatus: http.StatusServiceUnavailable, Code: "HOSTING_VERIFICATION_UNAVAILABLE", Message: "Hosting verification is unavailable"})
	c.Abort()
}
