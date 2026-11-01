package migrations

import (
	"fmt"
	"gorm.io/gorm"
	"gpt-load/internal/storage/models"
)

const ID0032 = "0032_hosting"

// Up0032 只增加挂靠归属与授权映射，不搬动已有组和凭据。
// 两表独立于日志保留策略，恢复后仍能识别停用资源和原操作。
// 使用 GORM 的跨数据库建表能力，重复执行只补齐缺失结构。
// 迁移不复制现有凭据，也不把旧组自动识别为用户挂靠。
// 新表随主迁移链启动，不能由请求路径临时建表。
// 身份唯一索引使用可空列，删除后允许其他绑定重新声明。
// 不透明引用按字符串存储，避免与 Oak 数据库自增主键耦合。
// 此升级兼容已有公共组，无须修改其 Key 过滤器。
// 恢复程序可重复执行，但遇到不满足索引的已有数据会明确失败。
// 回滚旧二进制前仍须使用成套数据库备份，不能忽略迁移版本。
// 数据库类型支持表示使用统一迁移结构，不等于外部实例矩阵已验收。
func Up0032(db *gorm.DB) error {
	if err := db.AutoMigrate(&models.HostingBinding{}, &models.HostingAuthorization{}); err != nil {
		return err
	}
	return Validate0032(db)
}

// Validate0032 校验本版本必须拥有的身份与操作唯一索引。
// 已有表存在但索引缺失不能被当作迁移完成。
func Validate0032(db *gorm.DB) error {
	for _, row := range []any{&models.HostingBinding{}, &models.HostingAuthorization{}} {
		if !db.Migrator().HasTable(row) {
			return fmt.Errorf("hosting table missing")
		}
	}
	for _, index := range []string{"idx_hosting_bindings_identity_fingerprint", "idx_hosting_bindings_operation_id", "idx_hosting_bindings_group_id", "idx_hosting_bindings_access_key_id"} {
		if !db.Migrator().HasIndex(&models.HostingBinding{}, index) {
			return fmt.Errorf("hosting index missing: %s", index)
		}
	}
	return nil
}

// 新表没有破坏性迁移步骤；恢复时由 Up 幂等补齐。
// 老版本完全没有这些表也是有效的升级起点。
func ValidateRecoverable0032(db *gorm.DB) error {
	switch db.Dialector.Name() {
	case "sqlite", "mysql", "postgres":
		return nil
	}
	return fmt.Errorf("unsupported hosting database")
}
