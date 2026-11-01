# Oak 私有账号执行契约 v1

本页描述本地已实现的执行层契约。真实 OAuth、真实 Codex 客户端、厂商出口及耗卡尚未验证，不能据本页直接向普通用户开放。New API 负责用户、统一 Key、来源顺序与计费；gpt-load 只执行单个私有账号。

## 代码导航与配置

- `internal/control/hosting.go`：授权归属、幂等建号、激活、暂停、重授权和解绑。
- `internal/control/hosting_http.go`：受管理认证保护的内部接口、脱敏审计。
- `internal/storage/models/hosting.go`、`migrations/0032_hosting.go`：绑定与授权表、数据库迁移。
- `internal/state/hosting.go`：调度、普通/Codex 目录、WebSocket、个人模型视图共用的权限入口。
- `internal/state/loader/hosting.go`：启动和所有配置发布前校验绑定、唯一身份与内部 Key。
- `internal/subscription/providers/codex/driver.go`：复用原厂额度/重置驱动，补独立卡详情状态。
- `web/src/frontends/modern/features/groups/`：资源分类、归属引用搜索与展示。

沿用既有 `AUTH_KEY` 管理认证和单实例部署，gpt-load 继续仅监听 `127.0.0.1:3001`。无新增第三方服务、依赖或环境变量。`AUTH_KEY` 和内部 Key 只由可信后端持有，不能给用户浏览器。部署配置见 [Oak 部署说明](../deploy/oak/README.md)。

## 公共约定

前缀 `/api/hosting/v1`，请求头 `Authorization: Bearer <AUTH_KEY>`，成功信封 `{ "code": 0, "data": ... }`。错误使用已有 HTTP 状态码和 `code/message`；调用者不能只看 HTTP 200。敏感响应设置 no-store，变更记录仅包含路由、不透明资源引用和成败，不记录 OAuth 回调和 Key。

`owner_ref` 是 Oak 后端从登录身份生成的引用，例如 `oak:1`；`account_ref` 在授权开始前分配。两者最长 128 字符，仅允许 ASCII 字母、数字、`-`、`_`、`:`。操作 UUID 必须满足现有 UUID v4 校验。除创建/授权 body 外，所有资源动作通过 query 传 `owner_ref`。

服务端每次同时匹配 owner/account 或 owner/stage，其他用户与不存在资源统一返回不存在。普通数据面 AccessKey 无权访问这些管理路由。

## 授权与建号

| 方法与路径 | 输入 | 返回与约束 |
| --- | --- | --- |
| POST `/authorizations` | `{owner_ref,account_ref,operation_id,purpose}` | purpose 为 `connect` 或 `reauthorize`；同 UUID 重放同 Stage |
| GET `/authorizations/:stage_id` | query owner_ref | 只读 Stage 摘要 |
| POST `/authorizations/:stage_id/callback` | `{callback_url}` + query owner_ref | 回调仅解析，不抓取该 URL；复用原 state/TTL/单次换码 |
| DELETE `/authorizations/:stage_id` | query owner_ref | 取消本人授权 |
| POST `/accounts` | `{owner_ref,account_ref,stage_id,operation_id,alias}` | operation_id 必须与授权相同，首次创建 pending；返回 internal_key |
| POST `/accounts/:ref/activate` | `{}` + query owner_ref | pending → active；重复激活不会恢复后来 paused 的账号 |
| PATCH `/accounts/:ref` | `{alias?,paused?}` + query owner_ref | 只允许备注和启停；恢复须显式 paused:false |
| DELETE `/accounts/:ref` | query owner_ref | 先停用；若存在未决重置则拒绝删除并保留凭据 |

Stage DTO 为 `{stage_id,status,authorization_method?,authorization_url?,redirect_uri?,account,expires_at_ms,error_code?}`。状态为 `pending_authorization | exchanging | ready | consumed | failed | cancelled | expired | outcome_unknown`。Codex 使用浏览器授权和既有 localhost 回调；没有新增设备码。官方登录页仍由用户浏览器访问，不能承诺首次授权不需要额外网络条件。

绑定摘要：

```json
{
  "account_ref": "opaque-account",
  "owner_ref": "oak:1",
  "provider": "codex",
  "alias": "工作账号",
  "status": "pending",
  "group_id": 1,
  "credential_id": 1,
  "access_key_id": 1,
  "identity_fingerprint": "内部不可逆指纹",
  "email_mask": "脱敏邮箱",
  "observation": {"state": "unavailable", "snapshot": null}
}
```

`internal_key` 仅创建及其幂等重放返回。Oak 必须加密保存再激活；列表和详情不返回 Key。资源 ID/指纹也应在 Oak 投影时剔除，不直接透传给浏览器。重新授权使用原 account_ref、新操作 UUID、新 Stage，要求同一原厂身份，沿用组、凭据、内部 Key 与启停状态。已删除的原创建操作仅返回 deleted，不重建资源。

身份由官方浏览器换码产生；同身份在本实例跨所有组独占，普通管理建组也不能复制。首期没有开放文件导入：存在挂靠绑定或进行中挂靠授权时，普通 Codex 文件导入以及临时 Stage 令牌刷新也返回 `HOSTING_BROWSER_AUTH_REQUIRED`，避免未验签文件身份与双刷新。新鲜浏览器授权可正常发现模型；Stage 需要刷新时重新开始授权。正式已绑定凭据继续使用原有凭据管理器刷新。

## 账号读取、额度与连接测试

| 方法与路径 | 返回 |
| --- | --- |
| GET `/accounts` | `{items:[摘要]}`，仅本人非删除账号 |
| GET `/accounts/:ref` | 安全内部摘要，不含 internal_key |
| GET `/accounts/:ref/models` | `{models:[{id,...}]}`，目录不代表真实推理已验收 |
| GET `/accounts/:ref/observation` | 当前缓存观测，不调用上游 |
| POST `/accounts/:ref/observation-refresh` | body `{}`，复用观测刷新/节流 |
| POST `/accounts/:ref/test` | body `{model,protocol:"openai-responses"}`，真实连接测试；不返回 restore_proof |

观测沿用 `state/snapshot/observed_at_ms/last_attempt_at_ms/last_error_code`。`snapshot` 中保留额度窗口、套餐摘要、独立积分与原厂重置机会。新增字段只位于 snapshot：

```json
{"reset_credit_observation":{"state":"partial","error_code":"RESET_CREDIT_DETAILS_UNAVAILABLE"}}
```

子状态为 `fresh | partial | unavailable`，fresh 带 `observed_at_ms`。主额度成功但卡详情失败时，有主计数为 partial，否则 unavailable；不能把缺失卡数显示成 0。空卡列表成功才表示已观测到零张。积分 `credits` 的字段为 `balance?:string,has_credits?:bool,unlimited?:bool,observed_at_ms?:int64`，不是钱包金额。

## 重置消费与恢复

- POST `/accounts/:ref/reset-credits/consume`：body `{}`，`Idempotency-Key: <UUID v4>`。消费账号原厂已有机会，不是外部卡密兑换；不接受用户指定上游参数。
- GET `/accounts/:ref/reset-operations/:operation_id`：只查询持久结果，不消费、不补刷新。返回 `{operation_id,status,error_code?,updated_at_ms,windows_reset?,redeemed_at_ms?}`。
- 状态为 `prepared | succeeded | rejected | outcome_unknown`。成功消费响应沿用 `{status,windows_reset,redeemed_at_ms?,observation?,observation_pending?,replayed}`。
- 恢复未知结果时再次 POST **原 UUID**；不得换 UUID。成功但观测失败仍是消费成功，`observation_pending=true` 不代表可再消费。
- 管理员与 Oak 共用同一操作建立屏障和持久消费逻辑。prepared 落库后释放创建/删除互斥，后续观测不会阻塞同 UUID 读取成功结果。
- 解绑先停用数据面；prepared/outcome_unknown 阻止删除 Group/Credential。终态后可再次解绑；完成的操作结果在解绑后仍可查询。

## 数据面与隔离

数据面保持 `http://127.0.0.1:3001/v1/responses`，Bearer 为该账号内部 Key，不加 group 路径。内部 Key 精确绑定单组，仅开放 Chat/Responses 协议。私有组即使遇到 `groups=[]` 的旧全量 Key 也拒绝；其他用户 Key、伪造组筛选同样拒绝。私有 Key 不能流入公共组。

权限在调度、OpenAI 普通目录、Codex 专用目录、WebSocket 建连/续轮和个人模型视图统一应用。启动与配置发布检查唯一凭据、唯一身份、Key 过滤器和官方上游目标；破坏绑定的普通后台编辑会失败。

跨账号、包月与钱包选择由 New API 决定。gpt-load 本轮**不提供可信“尚未执行”响应头**。上游 429、5xx、超时均不能被调用方自动解释为可重放；当前请求停止，下次请求可用最新缓存明确耗尽的观测决定跳过。暂停/解绑禁止新请求，不保证强杀已有流或等待其排空。

## 运维与验证

先发布含迁移 0032 和私有门禁的 gpt-load，再发布兼容 New API。开启入口前检查管理 API 版本、回环绑定、两服务数据及密钥成套备份和恢复。旧 gpt-load 不支持契约应明确不可用，不回退到公共组/管理 Key 执行。迁移后不能只换旧二进制回滚。

本地验证：

```sh
DOCKER_CONFIG=/private/tmp/oak-gpt-docker-tools go test ./... -count=1 -timeout=180s
go vet ./...
cd web
pnpm run lint
pnpm run build
```

全量 Go、vet、Node 24.22.1 下前端 lint/类型/构建通过。Docker Compose 使用隔离 2.39.4，系统旧版不支持既有测试的 `--no-env-resolution`；该工具配置仅在本机临时目录，非项目运行依赖。新增测试覆盖生命周期、同身份冲突、全量/他人 Key 隔离、真实 HTTP 鉴权、纯读重置结果、未决解绑、暂停激活重放、重授权资源稳定、首次授权导入保护、卡子状态、SQLite/MySQL/PostgreSQL 新安装、升级与重放。网关模拟外部执行器验证无权请求的执行次数为零。外部迁移测试 `TestExternalHostingMigrationContract` 通过 URL 格式的 `GPT_LOAD_DATABASE_TEST_DSN` 显式启用（不接受 PostgreSQL 的 key=value 或 MySQL 原生 DSN），使用既有 helper 创建和清理独立随机 MySQL 数据库/PostgreSQL schema，不修改原业务表，不输出 DSN。

未验证：真实厂商 OAuth/出口/推理、真实 Codex CLI 的流式与工具/compact、真实消费重置机会、线上容量与备份恢复。本轮未提交、推送或部署。
