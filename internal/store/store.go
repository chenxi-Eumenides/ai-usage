// Package store 提供 SQLite 持久化层，管理 API Key。
//
// 使用 modernc.org/sqlite（纯 Go，无 CGO），驱动名 "sqlite"。
// SQLite 单写者模式：db.SetMaxOpenConns(1)，避免锁竞争。
package store

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

// KeyType 常量定义 key 的类型标记。
// 与 provider.UsageTypePlan / UsageTypeBalance 值一致，server 可直接透传给 FetchUsage。
const (
	KeyTypePlan     = "plan"     // 套餐：只查询套餐类用量
	KeyTypeBalance  = "balance"  // 余额：只查询余额类用量
	KeyTypeBoth     = "both"     // 两者都尝试（默认）：余额/套餐接口都试，有哪个显示哪个
	KeyTypeDisabled = "disabled" // 禁用：不参与用量查询与仪表盘显示（key 仍保留在 Key 管理）
)

// KeyRecord 表示一条 API Key 记录。
type KeyRecord struct {
	ID        int64
	Provider  string
	Key       string
	KeyType   string // KeyTypePlan / KeyTypeBalance / KeyTypeBoth / KeyTypeDisabled，默认 "both"
	Account   string // 账户名：同 provider 同 account 的 key 视为同一账户，聚合为一张卡片，空串不聚合
	Note      string
	CreatedAt string
	SortOrder int // 用户自定义排序序号（越小越靠前），列表按 (sort_order, id) 排序
}

// AccountCredential 表示某 provider/account 的手动凭证。
type AccountCredential struct {
	Provider   string
	Account    string
	Credential string
	UpdatedAt  string
}

// Store 封装数据库连接和操作方法。
type Store struct {
	db *sql.DB
}

// Open 打开或创建 SQLite 数据库，确保目录、权限和 schema 就绪。
//
// dataDir 为数据库文件所在目录，自动创建（0700）。
// 数据库文件 gateway.db 权限固定为 0600。
// 已有文件权限宽松于 0600 时打印告警（不阻断）。
func Open(dataDir string) (*Store, error) {
	if err := os.MkdirAll(dataDir, 0700); err != nil {
		return nil, fmt.Errorf("store.Open: mkdir %s: %w", dataDir, err)
	}

	dbPath := filepath.Join(dataDir, "gateway.db")

	if fi, err := os.Stat(dbPath); err == nil {
		if mode := fi.Mode().Perm(); mode > 0600 {
			fmt.Fprintf(os.Stderr, "WARNING: %s permissions are %04o, expected 0600\n", dbPath, mode)
		}
	}

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Errorf("store.Open: sql.Open: %w", err)
	}

	db.SetMaxOpenConns(1)

	if _, err := db.Exec("PRAGMA journal_mode=WAL"); err != nil {
		db.Close()
		return nil, fmt.Errorf("store.Open: PRAGMA journal_mode: %w", err)
	}
	if _, err := db.Exec("PRAGMA busy_timeout=5000"); err != nil {
		db.Close()
		return nil, fmt.Errorf("store.Open: PRAGMA busy_timeout: %w", err)
	}

	if err := os.Chmod(dbPath, 0600); err != nil {
		db.Close()
		return nil, fmt.Errorf("store.Open: chmod %s: %w", dbPath, err)
	}

	if err := createSchema(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("store.Open: schema: %w", err)
	}
	if err := migrateSchema(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("store.Open: migrate: %w", err)
	}

	return &Store{db: db}, nil
}

// Close 关闭数据库连接。
func (s *Store) Close() error {
	return s.db.Close()
}

// AddKey 插入一条 API Key。provider+key 组合唯一，已存在时 inserted 返回 false。
// keyType 取值 KeyTypePlan / KeyTypeBalance / KeyTypeBoth。
// 新 key 的 sort_order 取当前最大值 + 1（追加到列表末尾）。
func (s *Store) AddKey(provider, key, keyType, account, note string) (inserted bool, err error) {
	result, err := s.db.Exec(
		"INSERT OR IGNORE INTO keys (provider, key, key_type, account, note, sort_order) VALUES (?, ?, ?, ?, ?, (SELECT COALESCE(MAX(sort_order), -1) + 1 FROM keys))",
		provider, key, keyType, account, note,
	)
	if err != nil {
		return false, fmt.Errorf("AddKey: %w", err)
	}
	n, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("AddKey: RowsAffected: %w", err)
	}
	return n > 0, nil
}

// ListKeys 返回所有 Key 记录，按 (sort_order, id) 升序（用户拖拽排序优先，
// sort_order 相同的旧数据按 id 兜底）。
func (s *Store) ListKeys() ([]KeyRecord, error) {
	rows, err := s.db.Query("SELECT id, provider, key, key_type, account, note, created_at, sort_order FROM keys ORDER BY sort_order, id")
	if err != nil {
		return nil, fmt.Errorf("ListKeys: %w", err)
	}
	defer rows.Close()

	var records []KeyRecord
	for rows.Next() {
		var r KeyRecord
		var account, note sql.NullString
		if err := rows.Scan(&r.ID, &r.Provider, &r.Key, &r.KeyType, &account, &note, &r.CreatedAt, &r.SortOrder); err != nil {
			return nil, fmt.Errorf("ListKeys: scan: %w", err)
		}
		r.Account = account.String
		r.Note = note.String
		records = append(records, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("ListKeys: rows: %w", err)
	}
	return records, nil
}

// GetKey 获取指定 ID 的记录，不存在返回 sql.ErrNoRows。
func (s *Store) GetKey(id int64) (*KeyRecord, error) {
	var r KeyRecord
	var account, note sql.NullString
	err := s.db.QueryRow(
		"SELECT id, provider, key, key_type, account, note, created_at, sort_order FROM keys WHERE id = ?",
		id,
	).Scan(&r.ID, &r.Provider, &r.Key, &r.KeyType, &account, &note, &r.CreatedAt, &r.SortOrder)
	if err != nil {
		return nil, err
	}
	r.Account = account.String
	r.Note = note.String
	return &r, nil
}

// UpdateKeyFull 更新指定 ID 的全部可变字段（provider/key 可变更）。
// keyType 取值 KeyTypePlan / KeyTypeBalance / KeyTypeBoth。
func (s *Store) UpdateKeyFull(id int64, provider, key, keyType, account, note string) error {
	_, err := s.db.Exec("UPDATE keys SET provider = ?, key = ?, key_type = ?, account = ?, note = ? WHERE id = ?",
		provider, key, keyType, account, note, id)
	if err != nil {
		return fmt.Errorf("UpdateKeyFull: %w", err)
	}
	return nil
}

// DeleteKey 删除指定 ID 的记录。
func (s *Store) DeleteKey(id int64) error {
	_, err := s.db.Exec("DELETE FROM keys WHERE id = ?", id)
	if err != nil {
		return fmt.Errorf("DeleteKey: %w", err)
	}
	return nil
}

// GetAccountCredential 获取账号级凭证，不存在时返回 sql.ErrNoRows。
func (s *Store) GetAccountCredential(provider, account string) (*AccountCredential, error) {
	var credential AccountCredential
	err := s.db.QueryRow("SELECT provider, account, credential, updated_at FROM account_credentials WHERE provider = ? AND account = ?", provider, account).
		Scan(&credential.Provider, &credential.Account, &credential.Credential, &credential.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return &credential, nil
}

// SetAccountCredential 新增或更新账号级凭证。
func (s *Store) SetAccountCredential(provider, account, credential string) error {
	_, err := s.db.Exec(`INSERT INTO account_credentials (provider, account, credential, updated_at)
		VALUES (?, ?, ?, ?) ON CONFLICT(provider, account) DO UPDATE SET credential = excluded.credential, updated_at = excluded.updated_at`,
		provider, account, credential, time.Now().UTC().Format(time.RFC3339))
	if err != nil {
		return fmt.Errorf("SetAccountCredential: %w", err)
	}
	return nil
}

// DeleteAccountCredential 删除账号级凭证；目标不存在时也视为成功。
func (s *Store) DeleteAccountCredential(provider, account string) error {
	if _, err := s.db.Exec("DELETE FROM account_credentials WHERE provider = ? AND account = ?", provider, account); err != nil {
		return fmt.Errorf("DeleteAccountCredential: %w", err)
	}
	return nil
}

// ReorderKeys 按传入的 id 顺序整体重排（事务内逐条更新 sort_order = 下标）。
// 调用方负责保证 ids 与现有 key 集合一致；多余/缺失的 id 不会导致失败
// （不在列表中的 id 不更新，缺失的保持原 sort_order），但会破坏顺序语义，
// 因此调用方应先做集合校验。
func (s *Store) ReorderKeys(ids []int64) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("ReorderKeys: begin: %w", err)
	}
	defer tx.Rollback()

	for i, id := range ids {
		if _, err := tx.Exec("UPDATE keys SET sort_order = ? WHERE id = ?", i, id); err != nil {
			return fmt.Errorf("ReorderKeys: update id=%d: %w", id, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("ReorderKeys: commit: %w", err)
	}
	return nil
}

// createSchema 在启动时执行 CREATE TABLE IF NOT EXISTS。
// 注意：T34 曾加 grp 列，T36 删除 group 语义后新库不再建该列；
// 已有库残留的 grp 列不被引用（SELECT/INSERT 不涉及），无影响。
// 账户列名为 account（T：alias 遗留字段名已改，旧库由 migrateSchema 迁移）。
func createSchema(db *sql.DB) error {
	schema := `
	CREATE TABLE IF NOT EXISTS keys (
		id INTEGER PRIMARY KEY,
		provider TEXT NOT NULL,
		key TEXT NOT NULL,
		key_type TEXT NOT NULL DEFAULT 'both',
		account TEXT,
		note TEXT,
		created_at TEXT NOT NULL DEFAULT (datetime('now')),
		sort_order INTEGER NOT NULL DEFAULT 0,
		UNIQUE(provider, key)
	);
	CREATE TABLE IF NOT EXISTS account_credentials (
		provider TEXT NOT NULL,
		account TEXT NOT NULL DEFAULT '',
		credential TEXT NOT NULL,
		updated_at TEXT NOT NULL DEFAULT (datetime('now')),
		PRIMARY KEY(provider, account)
	);
	`
	_, err := db.Exec(schema)
	return err
}

// migrateSchema 检测旧库缺列并 ALTER 补齐/迁移（仅追加/改名列，不破坏数据）。
// 当前支持：
//   - keys 表缺 key_type → ADD COLUMN 默认 'both'；缺 sort_order → ADD COLUMN 默认 0
//   - keys 表有旧 alias 列且无 account 列 → RENAME COLUMN alias TO account（数据保留）
func migrateSchema(db *sql.DB) error {
	var legacyCookieTable bool
	if err := db.QueryRow("SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type='table' AND name='account_cookies')").Scan(&legacyCookieTable); err != nil {
		return fmt.Errorf("migrateSchema: check account_cookies: %w", err)
	}
	if legacyCookieTable {
		tx, err := db.Begin()
		if err != nil {
			return fmt.Errorf("migrateSchema: begin credential migration: %w", err)
		}
		defer tx.Rollback()
		if _, err := tx.Exec(`INSERT INTO account_credentials (provider, account, credential, updated_at)
			SELECT provider, account, cookies, updated_at FROM account_cookies
			WHERE 1
			ON CONFLICT(provider, account) DO UPDATE SET credential = excluded.credential, updated_at = excluded.updated_at`); err != nil {
			return fmt.Errorf("migrateSchema: copy account_cookies: %w", err)
		}
		if _, err := tx.Exec("DROP TABLE account_cookies"); err != nil {
			return fmt.Errorf("migrateSchema: drop account_cookies: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("migrateSchema: commit credential migration: %w", err)
		}
	}

	rows, err := db.Query("PRAGMA table_info(keys)")
	if err != nil {
		return fmt.Errorf("migrateSchema: table_info(keys): %w", err)
	}
	defer rows.Close()

	hasKeyType, hasSortOrder, hasAlias, hasAccount := false, false, false, false
	for rows.Next() {
		var cid, notnull, pk int
		var name, ctype string
		var dfltValue sql.NullString
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dfltValue, &pk); err != nil {
			return fmt.Errorf("migrateSchema: scan: %w", err)
		}
		switch name {
		case "key_type":
			hasKeyType = true
		case "sort_order":
			hasSortOrder = true
		case "alias":
			hasAlias = true
		case "account":
			hasAccount = true
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("migrateSchema: rows: %w", err)
	}

	if !hasKeyType {
		if _, err := db.Exec("ALTER TABLE keys ADD COLUMN key_type TEXT NOT NULL DEFAULT 'both'"); err != nil {
			return fmt.Errorf("migrateSchema: ALTER TABLE keys ADD COLUMN key_type: %w", err)
		}
	}
	if !hasSortOrder {
		if _, err := db.Exec("ALTER TABLE keys ADD COLUMN sort_order INTEGER NOT NULL DEFAULT 0"); err != nil {
			return fmt.Errorf("migrateSchema: ALTER TABLE keys ADD COLUMN sort_order: %w", err)
		}
	}
	// 旧 alias 列迁移为 account（仅当新列不存在时，避免重复改名/冲突）。
	if hasAlias && !hasAccount {
		if _, err := db.Exec("ALTER TABLE keys RENAME COLUMN alias TO account"); err != nil {
			return fmt.Errorf("migrateSchema: ALTER TABLE keys RENAME COLUMN alias TO account: %w", err)
		}
	}
	return nil
}
