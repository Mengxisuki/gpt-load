package state

// AllowsGroup 是私有组的唯一权限判定，调度和目录共用。
// 私有组即使遇到空过滤 Key 也拒绝；只有绑定的内部 Key 可用。
// 显式单组过滤是第二道约束，防止管理编辑扩大该 Key 的权限。
// 映射中存在组即表示私有，值零表示资源尚未激活或已暂停。
// 缺失私有声明才沿用普通组规则，不能把值零当作公共组。
// 调用方使用同一次请求的快照，避免读取新旧配置混合的权限。
// 内部 Key 的原生过滤器仍必须匹配，声明本身不能扩大它。
// 判断只负责组权限，Key 的禁用、过期和模型过滤由既有入口处理。
// 此方法不选择账号顺序，也不判断钱包和套餐额度。
func (snapshot *ConfigSnapshot) AllowsGroup(key AccessKeyView, groupID uint) bool {
	if snapshot == nil {
		return false
	}
	if privateKey, private := snapshot.HostingGroups[groupID]; private {
		if privateKey == 0 || key.ID != privateKey || len(key.Filters.Groups) != 1 {
			return false
		}
	}
	// 挂靠内部 Key 不能在目标组被删除或配置损坏时流入公共池。
	for privateGroup, privateKey := range snapshot.HostingGroups {
		if privateKey != 0 && key.ID == privateKey && groupID != privateGroup {
			return false
		}
	}
	if len(key.Filters.Groups) == 0 {
		return true
	}
	_, allowed := key.Filters.Groups[groupID]
	return allowed
}
