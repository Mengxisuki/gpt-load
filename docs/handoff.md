# OpenCode 原生 Responses 上线交接

更新时间：2026-11-04 05:30:59 +00:00。分支 oak，基线 `c2f7279b`；本地未提交、未推送、未部署。用户已批准移除 GPT-Load 对 OpenCode 的强制 Chat 转换，供应商适配由上游承担。

已完成：Go/Zen 的 Responses 执行与探测统一原生；删除模型前缀 resolver、OpenCode 转换准备、exec 包装与恢复及过时测试。通用 Compatible 转换及流尾保护保留。无新增依赖、配置或迁移。默认根地址与既有自定义地址未改变。

验证：新真实本机 HTTP 测试先红（Kimi/GLM/DeepSeek 本地 422、零上游调用）后绿，覆盖两渠道、四模型、两轮工具历史、普通/流式输出、搜索引用、custom、动态声明、会话头、别名、用量与日志。执行层、渠道、网关、dialect、provideradapter、state 回归及 vet/build 已通过；OpenCode/Compatible 流式定向 race 通过。所有网络上游均为本机合成服务，未验证真实供应商搜索。

后续：用户要求发布时，检查实际 diff 并按仓库流程提交推送；oak 推送自动部署并重启，当前未获本轮发布指令。上次线上版本 `oak-c2f7279be06b-41772333177-1` 仍含旧转换。上线后验证指定 Kimi/GLM 的真实 Responses 请求：搜索事件与来源、custom 及动态工具、reasoning.context、流式终态和续轮。不以 HTTP 200 代替搜索验收；不重放用户历史任务或输出凭据。若上游拒绝，不再静默删参数、删工具或改 store 触发回退。

相关事实：官方 Codex 专页提供 kimi-k3 Responses 配置；上游 #57397 报告搜索能力随服务路径而异，尚未核实用户具体模型。会话头缺失的两条旧 Chat 请求在 Caddy 入口就没有会话标识，New API 已开启透传，不需要凭空补随机会话。详见 lessons 的 05:12:49、05:20:11 条目。

关键文件：`internal/channel/modules/opencode_{go,zen}.go`、`internal/execution/bifrost/executor.go`、`compatible.go`、`compatible_stream.go`、`opencode_presets_test.go`、`README_CN.md`。保留其他仓库与本地已有 Caddy 变更，不打包无关内容。
