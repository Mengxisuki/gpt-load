package loader

import (
	"encoding/json"
	"fmt"
	"gpt-load/internal/storage/models"
)

// validateHostingRows 在所有配置发布入口统一验证私有资源关系。
// 普通管理编辑、幂等写入以及进程重启都必须满足同一个约束。
// 验证失败不发布快照，也不能把损坏的私有资源降级为公共组。
// 输入来自同一数据库视图，不能以运行态缓存代替归属关系。
// 只对非删除绑定验证；删除事务保留 tombstone 但清理执行资源。
// 启动读配置也必须执行检查，不能仅在新接口中校验。
// 普通管理编辑如破坏约束，整个写事务应回滚。
// 内部 Key 过滤器采用既有 JSON 契约，不另写一套解析器。
// 暂停和待激活账号必须禁用 Key，避免只依赖 UI 状态。
// 组渠道固定 Codex，首期不把其他订阅渠道混入挂靠。
// 模型目录可以更新，但凭据身份及唯一资源关系不能变更。
// 官方默认上游省略 base_url；非空覆盖值在此被拒绝。
// 代理仍属于运维网络配置，不等同于替换业务上游。
// 身份比较不解密令牌，降低配置编译触及秘密的范围。
// 同组凭据必须恰好一个，不允许管理员追加第二身份。
// 跨组同身份会产生独立刷新状态，因此明确拒绝。
// 数据库错误或配置损坏应阻止发布，不静默关闭隔离。
func validateHostingRows(rows compileRows) error {
	groups := make(map[uint]models.Group, len(rows.groups))
	for _, group := range rows.groups {
		groups[group.ID] = group
	}
	keys := make(map[uint]models.AccessKey, len(rows.accessKeys))
	for _, key := range rows.accessKeys {
		keys[key.ID] = key
	}
	// 凭据只需要身份和归属元数据，这里不解密任何上游秘密。
	credentials := make(map[uint][]models.Credential)
	for _, credential := range rows.credentials {
		credentials[credential.GroupID] = append(credentials[credential.GroupID], credential)
	}
	for _, binding := range rows.hosting {
		group, groupExists := groups[binding.GroupID]
		key, keyExists := keys[binding.AccessKeyID]
		owned := credentials[binding.GroupID]
		var filters filterDTO
		if err := json.Unmarshal(key.Filters, &filters); err != nil {
			return fmt.Errorf("invalid hosting key policy")
		}
		// 账号只能有一个凭据，Key 只能指向同一个组，ID 必须与绑定完全匹配。
		if !groupExists || !keyExists || group.ChannelID != "codex" || len(owned) != 1 || owned[0].ID != binding.CredentialID || binding.IdentityFingerprint == nil || owned[0].IdentityFingerprint != *binding.IdentityFingerprint || len(filters.Groups) != 1 || filters.Groups[0] != binding.GroupID {
			return fmt.Errorf("invalid hosting resource ownership")
		}
		// pending/paused 的数据面 Key 必须保持停用，编辑后台不能越过激活协议。
		if binding.Status != "active" && key.Status != "disabled" {
			return fmt.Errorf("inactive hosting key must remain disabled")
		}
		var params map[string]any
		if err := json.Unmarshal(group.Params, &params); err != nil {
			return err
		}
		// 固定官方目标，避免把已有 OAuth 凭据转发给管理员误填的第三方地址。
		if value, exists := params["base_url"]; exists && value != "" {
			return fmt.Errorf("hosting upstream must use official endpoint")
		}
		// 浏览器换码得到的身份在整个实例内独占，包含普通组中的账号。
		for _, credential := range rows.credentials {
			if credential.GroupID != binding.GroupID && credential.IdentityFingerprint == *binding.IdentityFingerprint {
				return fmt.Errorf("hosting identity is duplicated")
			}
		}
	}
	return nil
}
