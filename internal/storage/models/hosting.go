package models

// HostingBinding 将一个 Oak 账号绑定到一套独占执行资源。
// 归属只由服务端控制，显示名称或 Group 名称不能改变权限。
// 已删除记录保留操作标识用于幂等，身份声明通过指针置空释放。
// 数据库唯一约束覆盖身份、执行组、凭据和内部 Key。
// OwnerRef 不设置外部数据库外键，两服务各自管理持久数据。
// Status 是平台生命周期；原厂健康与额度存在原观测表。
// 记录删除使用状态标识，以便原创建请求安全重放。
type HostingBinding struct {
	AccountRef  string `gorm:"type:varchar(128);primaryKey"`
	OwnerRef    string `gorm:"type:varchar(128);not null;index"`
	OperationID string `gorm:"type:char(36);not null;uniqueIndex"`
	StageID     string `gorm:"type:varchar(36);not null"`
	Provider    string `gorm:"type:varchar(32);not null"`
	Alias       string `gorm:"type:varchar(128);not null"`
	// 指纹由受控浏览器换码结果派生，永不接收客户端声明的身份。
	IdentityFingerprint *string `gorm:"type:varchar(128);uniqueIndex"`
	EmailMask           string  `gorm:"type:varchar(255);not null"`
	// 所有资源在同一事务建立；状态 pending 时数据面不能使用。
	GroupID      uint   `gorm:"not null;uniqueIndex"`
	CredentialID uint   `gorm:"not null;uniqueIndex"`
	AccessKeyID  uint   `gorm:"not null;uniqueIndex"`
	Status       string `gorm:"type:varchar(32);not null"`
	CreatedAtMS  int64  `gorm:"autoCreateTime:milli"`
	UpdatedAtMS  int64  `gorm:"autoUpdateTime:milli"`
}

// HostingAuthorization 只保存授权归属，不复制 PKCE 和上游令牌。
// OperationID 是调用方稳定 UUID，响应丢失后查询同一 Stage。
// 只允许浏览器换码，文件导入不会被标记为可信挂靠授权。
type HostingAuthorization struct {
	OperationID string `gorm:"type:char(36);primaryKey"`
	OwnerRef    string `gorm:"type:varchar(128);not null;index"`
	AccountRef  string `gorm:"type:varchar(128);not null;index"`
	Purpose     string `gorm:"type:varchar(32);not null"`
	StageID     string `gorm:"type:varchar(36);not null;uniqueIndex"`
	CreatedAtMS int64  `gorm:"autoCreateTime:milli"`
}
