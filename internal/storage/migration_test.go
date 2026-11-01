package storage

import (
	"net/url"
	"os"
	"reflect"
	"strings"
	"testing"

	"gorm.io/gorm"

	migrationfiles "gpt-load/internal/storage/migrations"
)

func TestMigrationRegistryContainsOrderedMigrations(t *testing.T) {
	wantIDs := []string{
		migrationfiles.ID0001,
		migrationfiles.ID0002,
		migrationfiles.ID0003,
		migrationfiles.ID0004,
		migrationfiles.ID0005,
		migrationfiles.ID0006,
		migrationfiles.ID0007,
		migrationfiles.ID0008,
		migrationfiles.ID0009,
		migrationfiles.ID0010,
		migrationfiles.ID0011,
		migrationfiles.ID0012,
		migrationfiles.ID0013,
		migrationfiles.ID0014,
		migrationfiles.ID0015,
		migrationfiles.ID0016,
		migrationfiles.ID0017,
		migrationfiles.ID0018,
		migrationfiles.ID0019,
		migrationfiles.ID0020,
		migrationfiles.ID0021,
		migrationfiles.ID0022,
		migrationfiles.ID0023,
		migrationfiles.ID0024,
		migrationfiles.ID0025,
		migrationfiles.ID0026,
		migrationfiles.ID0027,
		migrationfiles.ID0028,
		migrationfiles.ID0029,
		migrationfiles.ID0030,
		migrationfiles.ID0031,
		migrationfiles.ID0032,
	}
	if len(migrations) != len(wantIDs) {
		t.Fatalf("migration registry length = %d, want %d", len(migrations), len(wantIDs))
	}
	for index, entry := range migrations {
		if entry.ID != wantIDs[index] || entry.Up == nil ||
			entry.Validate == nil || entry.ValidateRecoverable == nil {
			t.Fatalf("migration registry entry %d = %#v", index, entry)
		}
	}
}

func TestMigrationRegistryUsesOneOrderedChainForFreshAndExistingDatabases(t *testing.T) {
	entries, calls := testMigrationRegistry()

	fresh := openInternalMigrationTestDatabase(t)
	if err := applyMigrationRegistry(fresh, entries); err != nil {
		t.Fatalf("migrate fresh database: %v", err)
	}
	if !reflect.DeepEqual(*calls, []string{"0001_test", "0002_test"}) {
		t.Fatalf("fresh migration calls = %v, want [0001_test 0002_test]", *calls)
	}

	existing := openInternalMigrationTestDatabase(t)
	if err := existing.AutoMigrate(&schemaMigration{}); err != nil {
		t.Fatalf("create existing migration ledger: %v", err)
	}
	if err := existing.Create(&schemaMigration{ID: entries[0].ID}).Error; err != nil {
		t.Fatalf("record existing migration: %v", err)
	}
	*calls = nil
	if err := applyMigrationRegistry(existing, entries); err != nil {
		t.Fatalf("migrate existing database: %v", err)
	}
	if !reflect.DeepEqual(*calls, []string{"0002_test"}) {
		t.Fatalf("existing migration calls = %v, want [0002_test]", *calls)
	}
}

func TestApplyMigrationRegistryRejectsOutOfOrderEntries(t *testing.T) {
	entries, _ := testMigrationRegistry()
	entries[0], entries[1] = entries[1], entries[0]

	err := applyMigrationRegistry(openInternalMigrationTestDatabase(t), entries)
	if err == nil || !strings.Contains(err.Error(), "migration registry entry 1") {
		t.Fatalf("applyMigrationRegistry() error = %v, want out-of-order registry rejection", err)
	}
}

func testMigrationRegistry() ([]migration, *[]string) {
	calls := make([]string, 0, 2)
	entry := func(id string) migration {
		return migration{
			ID: id,
			Up: func(*gorm.DB) error {
				calls = append(calls, id)
				return nil
			},
			Validate:            func(*gorm.DB) error { return nil },
			ValidateRecoverable: func(*gorm.DB) error { return nil },
		}
	}
	return []migration{entry("0001_test"), entry("0002_test")}, &calls
}

// 已有数据库升级只增加新表；重复迁移不得丢失归属数据。
// SQLite 使用真实迁移链，覆盖发布后再次启动的场景。
// 迁移输入停在上一版本，真实模拟线上已有数据库。
// 新版本首次执行需要创建授权表及身份唯一索引。
// 插入数据只覆盖无外部凭据的授权归属，不包含秘密。
// 再次迁移后操作数不变，确保历史幂等记录保留。
// 结构校验独立执行，防止台账存在而实际表不完整。
// 默认使用真实 SQLite；外部测试入口复用相同断言。
// 外部数据库由既有 helper 创建独立随机库或 schema。
func TestHostingMigrationUpgradeAndReplay(t *testing.T) {
	testHostingMigrationContract(t, openInternalMigrationTestDatabase)
}

// 外部矩阵通过环境变量显式启用，不把开发数据库作为默认目标。
// helper 只创建并清理本次随机命名的资源，不修改原数据库表。
// DSN 不进入日志和断言正文，凭据由运行环境提供。
// 测试流程与 SQLite 完全一致，不能另写弱化版外部断言。
func TestExternalHostingMigrationContract(t *testing.T) {
	dsn := os.Getenv("GPT_LOAD_DATABASE_TEST_DSN")
	if dsn == "" {
		t.Skip("GPT_LOAD_DATABASE_TEST_DSN is not set")
	}
	// 必须先检查 URL；key=value DSN 会被底层当作 SQLite 文件名。
	// 拒绝时不输出原输入，避免测试日志包含连接凭据。
	parsed, err := url.Parse(dsn)
	if err != nil || (parsed.Scheme != "mysql" && parsed.Scheme != "postgres" && parsed.Scheme != "postgresql") {
		t.Fatal("external hosting migration requires a MySQL/PostgreSQL URL")
	}
	testHostingMigrationContract(t, func(t *testing.T) *gorm.DB { return openExternalIncrementalMigrationDatabase(t, dsn) })
}

// 每个场景使用独立数据库，避免新安装被上一个升级用例预建表。
// fresh 检验完整链；upgrade 先建立 0031 基线，再追加 0032。
// 两个场景都插入持久归属并重复迁移，保证不丢失幂等状态。
// 这里只检查结构和数据保留，跨账号行为由控制层真实路径验证。
func testHostingMigrationContract(t *testing.T, open func(*testing.T) *gorm.DB) {
	for _, scenario := range []string{"fresh", "upgrade"} {
		t.Run(scenario, func(t *testing.T) {
			db := open(t)
			if scenario == "upgrade" {
				if err := applyMigrationRegistry(db, migrations[:len(migrations)-1]); err != nil {
					t.Fatal(err)
				}
			}
			// 旧版本基线没有挂靠表，避免把 AutoMigrate 预先建表误当升级通过。
			// 通过迁移台账执行新增版本，不直接调用模型建表替代真实流程。
			if db.Migrator().HasTable("hosting_bindings") {
				t.Fatal("hosting table already exists")
			}
			if err := applyMigrationRegistry(db, migrations); err != nil {
				t.Fatal(err)
			}
			if err := db.Exec("INSERT INTO hosting_authorizations (operation_id, owner_ref, account_ref, purpose, stage_id, created_at_ms) VALUES (?, ?, ?, ?, ?, ?)", "migration-operation", "oak:1", "account", "connect", "stage", 1).Error; err != nil {
				t.Fatal(err)
			}
			// 第二次启动应保留原操作引用，防止幂等信息在重建时消失。
			// 校验器还必须确认唯一索引存在，而非只看迁移台账的版本号。
			if err := applyMigrationRegistry(db, migrations); err != nil {
				t.Fatal(err)
			}
			if err := migrationfiles.Validate0032(db); err != nil {
				t.Fatal(err)
			}
			var count int64
			if err := db.Table("hosting_authorizations").Count(&count).Error; err != nil || count != 1 {
				t.Fatalf("authorization count=%d err=%v", count, err)
			}
		})
	}
}
