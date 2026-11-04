# 重大决策

## 2026-11-01 07:52:24 +00:00 — 独立原生内网服务与上游分支隔离

背景：用户要求先将当前 GPT-Load 部署到 `ssh wdm`，并沿用 NewApi 的官方跟踪/开发发布分支分工；用户明确选择暂不接域名。服务器约1.7GiB内存、无Docker，已运行NewApi等服务。

备选：安装Docker运行官方镜像，或沿用现有原生二进制/systemd。选择原生部署和独立低权限用户，构建放在本机/GitHub托管Runner，运行时独立SQLite与数据目录，只监听回环3001；不改变现有业务数据库与代理配置。

分支选择`upstream-main`纯跟踪官方，`oak`放定制和CI/CD，保留`main`。既有官方CI依赖私有self-hosted Runner且有源码合同测试，因此新增独立发布工作流而不重写官方工作流。自动合入上游不属于当前范围。

代价：用户需SSH隧道访问管理页，公网API待接域名后另行开放。发布时短暂停止本应用并成套备份SQLite、管理/加密密钥和环境；新程序尝试启动后失败不盲目降级。部署不等于已完成用户自助挂靠或真实上游联调。相关实现见`deploy/oak/`与`.github/workflows/oak-deploy.yml`。

## 2026-11-01 08:08:58 +00:00 — 接通独立环境与自动推送发布

背景：分支与服务已部署，用户本人完成GitHub CLI登录授权。选择仓库默认分支`oak`、独立`oak-production`环境并只允许oak分支；专用受限SSH密钥仅交给部署任务，不复用NewApi或个人SSH私钥。

推送`c8f46584`实际触发的[运行41287052506](https://github.com/Mengxisuki/gpt-load/actions/runs/41287052506)构建与部署均成功，已确认发布前SQLite与密钥成套备份及新版本健康。手动入口默认只构建；官方跟踪分支不触发发布。代价是每次oak推送都会完整检查并短暂停止本应用，恢复仍需遵守迁移边界。

## 2026-11-01 14:40:54 +00:00 — 私有账号执行与统一来源调度分离

背景：用户确认免费 Codex 挂靠、多账号与同一 Oak Key 排序挂靠/包月/钱包，并授权按方案实现。旧 AccessKey 空组过滤允许全部资源；单靠分组名称或 UI 分类无法隔离用户账号。

备选：复用公共组、给用户独立业务 Key，或由 New API 管统一来源并给每账号独立受限内部 Key。选择后者：本地新增绑定/授权表与内部契约，创建 pending，业务侧保存后激活；调度、两种模型目录、WebSocket 和个人视图统一私有门禁；所有配置发布校验唯一凭据、身份与 Key 作用域。管理分类由绑定派生。

代价：两服务需同步发布和成套备份；内部 Key 不可发给浏览器。首期仅可信浏览器换码，有挂靠或进行中授权时拒绝 Codex 文件导入及临时 Stage 刷新，正式凭据刷新保留。未添加可信未执行响应头，429/5xx 不自动作为跨来源重放依据。详见 [执行契约](../hosting-api.md)、`internal/control/hosting.go`、`internal/state/hosting.go`。本地测试及 SQLite/MySQL/PostgreSQL 新安装、升级和重复迁移已通过，真实授权/推理/耗卡和线上部署尚未完成。


## 2026-11-01 16:11:29 +00:00 — 挂靠控制由 New API 用户请求断言授权

背景：旧挂靠控制接口要求 New API 持有 gpt-load 完整管理员 `AUTH_KEY`，用户明确拒绝此权限安排，要求沿用 New API 用户体系。此项补充并替代 2026-11-01 14:40:54 条目中控制面使用管理认证的实现，账号私有绑定和受限数据面 Key 保持不变。

备选：继续共享管理员秘密、另设长期服务密钥，或由 New API 依据当前用户签发请求限定短期断言并在线核验。选择第三项：gpt-load 独立挂靠 HTTP 模块，在本地排除其他凭据后向固定字面回环 New API 验证真实 method/request_uri/body SHA256/Idempotency-Key，业务归属只采用核验响应。不存在共享管理密钥或共享 CRYPTO_SECRET；仅配置非秘密 `OAK_NEW_API_URL`。

代价：每次挂靠控制调用多一次本机核验，New API 核验不可用时失败关闭；两服务须按新合同协同发布。断言30秒有效并可重复只读核验，不是一次性票据；写动作继续由原操作UUID持久幂等。无新增数据库或第三方依赖。实现见 `internal/control/hosting_auth.go` 与 [执行契约](../hosting-api.md)，控制面/容器完整本地回归已通过；未部署此修正，真实厂商授权、推理和耗卡仍不在本次验证范围。

## 2026-11-03 15:26:33 +00:00 — OpenCode 已知 Chat 模型在执行层复用协议转换

背景：用户两次映射分别命中 GLM 和 Kimi，新任务同样被上游以 `reasoning.context` 不支持拒绝。已授权只围绕新任务切换实现。官方端点目录将这些家族和 DeepSeek 列为 Chat；网关提供 Responses 路径不等于其每个模型原生支持全部 Responses 参数。

备选：逐字段删除、在 New API 与 GPT-Load 两层各写兼容规则，或在最终渠道选择后复用现有 Compatible 转换。选择第三项，通过现有模型路由 resolver 为 OpenCode Go/Zen 的 `glm-`、`kimi-`、`deepseek-` 选择转换，其他模型保持原生；普通请求、流式、探测共用模型判断。

代价：GPT 推理选项不能表达为这些目标的等效档位，沿用 Compatible 不转发的合同；只支持可移植的消息/函数工具，旧会话、加密/压缩历史、托管工具及自定义语法显式拒绝。不新增依赖、配置或数据库列。真实网关测试确认日志标注转换模式和实际 Chat 协议；不承诺所有模型家族或客户端。实现和复现见 `README_CN.md`、`internal/execution/bifrost/opencode_presets_test.go`；生产发布与真实厂商验收尚未执行。

## 2026-11-04 01:55:03 +00:00 — 将已知 exec 文法等价包装为 Chat 函数

背景：用户授权实现新请求所需的 custom/grammar 适配。现场 `exec` 文法的 `plain_source` 分支覆盖任意非空字符串，普通 Chat function 可以等价表达；不能只放宽白名单后让 SDK 丢弃工具。

备选：通用 Lark 解析器与任意 custom 桥接、删除工具，或只支持已证明等价的声明。选择最后一种：精确匹配默认 namespace 的 exec 与已知 grammar，以单字段 `input` JSON 包装发送，普通和流式输出严格解包，并转换下一轮调用、结果及工具选择。复用既有 SDK 的命名空间、历史和消费证据链，不引入依赖。补充并限定 2026-11-03 15:26:33 条目中自定义语法的拒绝边界。

代价：流式 exec 源码须在完整参数校验后一次发出，无法逐字符显示生成过程；文法变更或其他 custom 工具继续明确拒绝，需以新样例重新确认合同。无效输出返回错误并保留用量，不生成假的完成态。实现及回归见 `internal/execution/bifrost/opencode_exec.go`、`opencode_exec_test.go`；本地验证通过，未发布，未验证真实模型的工具生成质量。


## 2026-11-04 05:30:59 +00:00 — OpenCode 协议转换交回上游

背景：本地按模型家族强制 Chat 已阻断 web_search，后续需要持续维护 custom/grammar 及工具历史特例；官方 Codex 专页明确支持 Kimi 直接使用 Go Responses，由上游适配。用户在本轮明确批准不在 GPT-Load 层转换。

备选：继续增加厂商工具转换与搜索执行服务，或恢复 OpenCode 原生 Responses。采用后者，删除模型前缀 resolver、OpenCode 转换准备及专用 exec 包装/恢复，复用原生凭据过滤、模型别名、用量与错误处理。无新的搜索服务、依赖或配置，其他渠道继续其现有转换。

本条替代 2026-11-03 15:26:33 的 OpenCode 家族转换选择，以及 2026-11-04 01:55:03 的 exec 特例选择；通用 Compatible 的命名空间处理与流式尾块修复仍保留。代价是 GPT 参数及全部原生工具交由上游接受或拒绝，不能保证上游每个模型的搜索、custom 或旧会话兼容。已有根地址不自动迁移，不改 store 来操纵路由，不丢工具制造成功。

依据：`internal/channel/modules/opencode_{go,zen}.go`、`opencode_presets_test.go::TestOpenCodeMappedResponsesNativeTools`。新测试在旧代码上对三个目标家族返回本地 422/零上游调用，改动后真实本机 HTTP 双回合通过，GPT 对照保留。上游能力仍需发布后验收；未调用真实模型或改生产。说明见 README_CN 的跨模型章节。
