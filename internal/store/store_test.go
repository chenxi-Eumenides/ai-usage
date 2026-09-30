package store

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func openStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return s
}

func TestOpen(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	dbPath := filepath.Join(dir, "gateway.db")
	fi, err := os.Stat(dbPath)
	if err != nil {
		t.Fatalf("stat db: %v", err)
	}
	if mode := fi.Mode().Perm(); mode != 0600 {
		t.Errorf("db file mode = %04o, want 0600", mode)
	}
}

func TestCRUD(t *testing.T) {
	s := openStore(t)
	defer s.Close()

	inserted, err := s.AddKey("test", "sk-abc", KeyTypeBoth, "myalias", "test note")
	if err != nil {
		t.Fatalf("AddKey: %v", err)
	}
	if !inserted {
		t.Error("first AddKey should return inserted=true")
	}

	records, err := s.ListKeys()
	if err != nil {
		t.Fatalf("ListKeys: %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("ListKeys: got %d records, want 1", len(records))
	}
	r := records[0]
	if r.Provider != "test" || r.Key != "sk-abc" || r.Account != "myalias" || r.Note != "test note" {
		t.Errorf("record mismatch: %+v", r)
	}
	if r.KeyType != KeyTypeBoth {
		t.Errorf("KeyType = %q, want %q", r.KeyType, KeyTypeBoth)
	}
	if r.CreatedAt == "" {
		t.Error("CreatedAt should not be empty")
	}
	id := r.ID

	got, err := s.GetKey(id)
	if err != nil {
		t.Fatalf("GetKey: %v", err)
	}
	if got.Provider != r.Provider || got.Key != r.Key {
		t.Errorf("GetKey mismatch: %+v", got)
	}
	if got.KeyType != KeyTypeBoth {
		t.Errorf("GetKey KeyType = %q, want %q", got.KeyType, KeyTypeBoth)
	}
	if got.Account != "myalias" {
		t.Errorf("GetKey Account = %q, want myalias", got.Account)
	}

	if err := s.UpdateKeyFull(id, "test", "sk-abc", KeyTypePlan, "newalias", "new note"); err != nil {
		t.Fatalf("UpdateKeyFull: %v", err)
	}
	got, err = s.GetKey(id)
	if err != nil {
		t.Fatalf("GetKey after update: %v", err)
	}
	if got.Account != "newalias" || got.Note != "new note" {
		t.Errorf("UpdateKeyFull: got account=%q note=%q", got.Account, got.Note)
	}
	if got.KeyType != KeyTypePlan {
		t.Errorf("UpdateKeyFull: KeyType = %q, want %q", got.KeyType, KeyTypePlan)
	}

	if err := s.DeleteKey(id); err != nil {
		t.Fatalf("DeleteKey: %v", err)
	}
	records, err = s.ListKeys()
	if err != nil {
		t.Fatalf("ListKeys after delete: %v", err)
	}
	if len(records) != 0 {
		t.Errorf("ListKeys after delete: got %d records, want 0", len(records))
	}

	_, err = s.GetKey(id)
	if err == nil {
		t.Error("GetKey after delete should return error")
	}
}

func TestAccountCredentialCRUD(t *testing.T) {
	s := openStore(t)
	defer s.Close()
	if _, err := s.GetAccountCredential("mimo", ""); err != sql.ErrNoRows {
		t.Fatalf("missing cookie error = %v, want sql.ErrNoRows", err)
	}
	if err := s.SetAccountCredential("mimo", "", "sid=first"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetAccountCredential("mimo", "", "sid=updated"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetAccountCredential("mimo", "acct", "sid=other"); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetAccountCredential("mimo", "")
	if err != nil {
		t.Fatal(err)
	}
	if got.Credential != "sid=updated" || got.Provider != "mimo" || got.Account != "" || got.UpdatedAt == "" {
		t.Fatalf("cookie record = %+v", got)
	}
	other, err := s.GetAccountCredential("mimo", "acct")
	if err != nil || other.Credential != "sid=other" {
		t.Fatalf("other account = %+v, err=%v", other, err)
	}
	if err := s.DeleteAccountCredential("mimo", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetAccountCredential("mimo", ""); err != sql.ErrNoRows {
		t.Fatalf("after delete error = %v", err)
	}
}

func TestMigrateLegacyAccountCookies(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "gateway.db")
	raw, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`CREATE TABLE account_cookies (
		provider TEXT NOT NULL, account TEXT NOT NULL DEFAULT '', cookies TEXT NOT NULL,
		updated_at TEXT NOT NULL, PRIMARY KEY(provider, account))`); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`INSERT INTO account_cookies VALUES ('mimo', '', 'sid=legacy', '2026-09-30T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	s, err := Open(dir)
	if err != nil {
		t.Fatalf("Open migration: %v", err)
	}
	defer s.Close()
	got, err := s.GetAccountCredential("mimo", "")
	if err != nil || got.Credential != "sid=legacy" || got.UpdatedAt != "2026-09-30T00:00:00Z" {
		t.Fatalf("migrated credential = %+v, err=%v", got, err)
	}
	var legacyExists bool
	if err := s.db.QueryRow("SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type='table' AND name='account_cookies')").Scan(&legacyExists); err != nil {
		t.Fatal(err)
	}
	if legacyExists {
		t.Fatal("legacy account_cookies table was not removed after migration")
	}
}

func TestAddKeyIdempotent(t *testing.T) {
	s := openStore(t)
	defer s.Close()

	inserted, err := s.AddKey("test", "sk-abc", KeyTypeBoth, "a1", "")
	if err != nil {
		t.Fatalf("first AddKey: %v", err)
	}
	if !inserted {
		t.Error("first AddKey should return inserted=true")
	}

	inserted, err = s.AddKey("test", "sk-abc", KeyTypePlan, "a2", "")
	if err != nil {
		t.Fatalf("second AddKey: %v", err)
	}
	if inserted {
		t.Error("second AddKey should return inserted=false")
	}

	records, err := s.ListKeys()
	if err != nil {
		t.Fatalf("ListKeys: %v", err)
	}
	if len(records) != 1 {
		t.Errorf("ListKeys: got %d records, want 1", len(records))
	}
	if records[0].Account != "a1" {
		t.Errorf("account should be 'a1' (first insert), got %q", records[0].Account)
	}
	if records[0].KeyType != KeyTypeBoth {
		t.Errorf("KeyType should be 'both' (first insert), got %q", records[0].KeyType)
	}
}

func TestKeyTypeVariants(t *testing.T) {
	s := openStore(t)
	defer s.Close()

	cases := []struct {
		keyType string
	}{
		{KeyTypeBoth},
		{KeyTypePlan},
		{KeyTypeBalance},
	}
	for _, c := range cases {
		inserted, err := s.AddKey("test", "sk-"+c.keyType, c.keyType, "", "")
		if err != nil {
			t.Fatalf("AddKey(%q): %v", c.keyType, err)
		}
		if !inserted {
			t.Fatalf("AddKey(%q) should insert", c.keyType)
		}
	}

	records, err := s.ListKeys()
	if err != nil {
		t.Fatalf("ListKeys: %v", err)
	}
	if len(records) != len(cases) {
		t.Fatalf("ListKeys: got %d records, want %d", len(records), len(cases))
	}
	for i, r := range records {
		if r.KeyType != cases[i].keyType {
			t.Errorf("records[%d].KeyType = %q, want %q", i, r.KeyType, cases[i].keyType)
		}
	}
}

func TestMigrateAddsKeyTypeColumn(t *testing.T) {
	dir := t.TempDir()

	// 用旧 schema 手工建库（无 key_type 列），再通过 Open 触发迁移。
	dbPath := filepath.Join(dir, "gateway.db")
	raw, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	if _, err := raw.Exec(`CREATE TABLE keys (
		id INTEGER PRIMARY KEY,
		provider TEXT NOT NULL,
		key TEXT NOT NULL,
		alias TEXT,
		note TEXT,
		created_at TEXT NOT NULL DEFAULT (datetime('now')),
		UNIQUE(provider, key)
	)`); err != nil {
		raw.Close()
		t.Fatalf("create old schema: %v", err)
	}
	if _, err := raw.Exec(`INSERT INTO keys (provider, key, alias, note) VALUES ('legacy', 'sk-legacy', 'old', 'note')`); err != nil {
		raw.Close()
		t.Fatalf("insert legacy row: %v", err)
	}
	raw.Close()

	s, err := Open(dir)
	if err != nil {
		t.Fatalf("Open after migration: %v", err)
	}
	defer s.Close()

	records, err := s.ListKeys()
	if err != nil {
		t.Fatalf("ListKeys after migration: %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("ListKeys: got %d records, want 1", len(records))
	}
	// 旧数据迁移后 key_type 默认 'both'，alias 列已 RENAME 为 account 且数据保留。
	if records[0].KeyType != KeyTypeBoth {
		t.Errorf("legacy KeyType = %q, want %q", records[0].KeyType, KeyTypeBoth)
	}
	if records[0].Key != "sk-legacy" {
		t.Errorf("legacy Key = %q, want sk-legacy", records[0].Key)
	}
	if records[0].Account != "old" {
		t.Errorf("legacy Account = %q, want old（alias 列迁移保留数据）", records[0].Account)
	}

	// 迁移后仍可正常新增 key。
	inserted, err := s.AddKey("test", "sk-new", KeyTypeBalance, "", "")
	if err != nil {
		t.Fatalf("AddKey after migration: %v", err)
	}
	if !inserted {
		t.Error("AddKey after migration should insert")
	}
}

func TestAccountStorageAndUpdate(t *testing.T) {
	s := openStore(t)
	defer s.Close()

	// 账户存取：同 provider 同 account 两条，另一条不同 account。
	if _, err := s.AddKey("test", "sk-acct-a", KeyTypeBoth, "主账户", ""); err != nil {
		t.Fatalf("AddKey a: %v", err)
	}
	if _, err := s.AddKey("test", "sk-acct-b", KeyTypeBoth, "主账户", ""); err != nil {
		t.Fatalf("AddKey b: %v", err)
	}
	if _, err := s.AddKey("test", "sk-other", KeyTypeBoth, "其他", ""); err != nil {
		t.Fatalf("AddKey other: %v", err)
	}
	if _, err := s.AddKey("other-prov", "sk-noacct", KeyTypeBoth, "", ""); err != nil {
		t.Fatalf("AddKey noacct: %v", err)
	}

	records, err := s.ListKeys()
	if err != nil {
		t.Fatalf("ListKeys: %v", err)
	}
	accounts := make(map[string]int)
	for _, r := range records {
		accounts[r.Provider+":"+r.Account]++
	}
	if accounts["test:主账户"] != 2 {
		t.Errorf("test:主账户 应聚合 2 条，got %d", accounts["test:主账户"])
	}
	if accounts["test:其他"] != 1 {
		t.Errorf("test:其他 应为 1 条，got %d", accounts["test:其他"])
	}
	if accounts["other-prov:"] != 1 {
		t.Errorf("other-prov: 空 account 应为 1 条，got %d", accounts["other-prov:"])
	}

	// UpdateKeyFull 可变更 account/provider/key。
	for _, r := range records {
		if r.Provider == "test" && r.Key == "sk-other" {
			if err := s.UpdateKeyFull(r.ID, "test", "sk-other", KeyTypeBoth, "主账户", r.Note); err != nil {
				t.Fatalf("UpdateKeyFull account: %v", err)
			}
		}
	}
	records, err = s.ListKeys()
	if err != nil {
		t.Fatalf("ListKeys after update: %v", err)
	}
	accounts = make(map[string]int)
	for _, r := range records {
		accounts[r.Provider+":"+r.Account]++
	}
	if accounts["test:主账户"] != 3 {
		t.Errorf("UpdateKeyFull 后 test:主账户 应为 3 条，got %d", accounts["test:主账户"])
	}
}

// TestExistingGrpColumnIgnored 验证 T34 建的库（含 grp 列）在 T36 新代码下正常：
// grp 列残留无害（SELECT/INSERT 不再引用），读写不受影响。
func TestExistingGrpColumnIgnored(t *testing.T) {
	dir := t.TempDir()

	dbPath := filepath.Join(dir, "gateway.db")
	raw, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	if _, err := raw.Exec(`CREATE TABLE keys (
		id INTEGER PRIMARY KEY,
		provider TEXT NOT NULL,
		key TEXT NOT NULL,
		key_type TEXT NOT NULL DEFAULT 'both',
		grp TEXT,
		alias TEXT,
		note TEXT,
		created_at TEXT NOT NULL DEFAULT (datetime('now')),
		UNIQUE(provider, key)
	)`); err != nil {
		raw.Close()
		t.Fatalf("create T34 schema: %v", err)
	}
	if _, err := raw.Exec(`INSERT INTO keys (provider, key, key_type, grp, alias, note) VALUES ('legacy', 'sk-legacy', 'both', '旧账户', 'old', 'note')`); err != nil {
		raw.Close()
		t.Fatalf("insert legacy row: %v", err)
	}
	raw.Close()

	s, err := Open(dir)
	if err != nil {
		t.Fatalf("Open with legacy grp column: %v", err)
	}
	defer s.Close()

	records, err := s.ListKeys()
	if err != nil {
		t.Fatalf("ListKeys: %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("ListKeys: got %d records, want 1", len(records))
	}
	if records[0].KeyType != KeyTypeBoth {
		t.Errorf("legacy KeyType = %q, want both", records[0].KeyType)
	}
	if records[0].Account != "old" {
		t.Errorf("legacy Account = %q, want old（alias 列迁移保留数据）", records[0].Account)
	}

	// 已有 grp 列的库仍可正常新增/更新。
	inserted, err := s.AddKey("test", "sk-new", KeyTypeBalance, "账户A", "")
	if err != nil {
		t.Fatalf("AddKey: %v", err)
	}
	if !inserted {
		t.Error("AddKey should insert")
	}
	records, err = s.ListKeys()
	if err != nil {
		t.Fatalf("ListKeys: %v", err)
	}
	var newID int64
	for _, r := range records {
		if r.Provider == "test" && r.Key == "sk-new" {
			newID = r.ID
		}
	}
	if newID == 0 {
		t.Fatalf("未找到新增记录: %+v", records)
	}
	if err := s.UpdateKeyFull(newID, "test", "sk-new", KeyTypePlan, "账户B", ""); err != nil {
		t.Fatalf("UpdateKeyFull: %v", err)
	}
	got, err := s.GetKey(newID)
	if err != nil {
		t.Fatalf("GetKey: %v", err)
	}
	if got.Account != "账户B" || got.KeyType != KeyTypePlan {
		t.Errorf("更新后 account=%q key_type=%q, want 账户B/plan", got.Account, got.KeyType)
	}
}

func TestDataDirCreated(t *testing.T) {
	base := t.TempDir()
	nested := filepath.Join(base, "sub", "deep")
	s, err := Open(nested)
	if err != nil {
		t.Fatalf("Open nested: %v", err)
	}
	s.Close()

	fi, err := os.Stat(nested)
	if err != nil {
		t.Fatalf("stat nested dir: %v", err)
	}
	if !fi.IsDir() {
		t.Error("nested path should be a directory")
	}
}

func TestSortOrder(t *testing.T) {
	s := openStore(t)
	defer s.Close()

	var ids []int64
	for i := 0; i < 3; i++ {
		inserted, err := s.AddKey("test", fmt.Sprintf("sk-key-%d", i), KeyTypeBoth, "", "")
		if err != nil {
			t.Fatalf("AddKey #%d: %v", i, err)
		}
		if !inserted {
			t.Fatalf("AddKey #%d: 未插入", i)
		}
		recs, err := s.ListKeys()
		if err != nil {
			t.Fatalf("ListKeys: %v", err)
		}
		ids = append(ids, recs[len(recs)-1].ID)
	}

	// 新 key 追加到末尾：ListKeys 顺序 = 添加顺序，sort_order 递增
	recs, err := s.ListKeys()
	if err != nil {
		t.Fatalf("ListKeys: %v", err)
	}
	if len(recs) != 3 {
		t.Fatalf("len = %d, want 3", len(recs))
	}
	for i := range recs {
		if recs[i].ID != ids[i] {
			t.Errorf("顺序 %d: got id %d, want %d", i, recs[i].ID, ids[i])
		}
		if recs[i].SortOrder != i {
			t.Errorf("顺序 %d: sort_order = %d, want %d", i, recs[i].SortOrder, i)
		}
	}

	// 重排为 [ids[2], ids[0], ids[1]]
	if err := s.ReorderKeys([]int64{ids[2], ids[0], ids[1]}); err != nil {
		t.Fatalf("ReorderKeys: %v", err)
	}
	recs, err = s.ListKeys()
	if err != nil {
		t.Fatalf("ListKeys after reorder: %v", err)
	}
	wantOrder := []int64{ids[2], ids[0], ids[1]}
	for i := range recs {
		if recs[i].ID != wantOrder[i] {
			t.Errorf("重排后顺序 %d: got id %d, want %d", i, recs[i].ID, wantOrder[i])
		}
		if recs[i].SortOrder != i {
			t.Errorf("重排后顺序 %d: sort_order = %d, want %d", i, recs[i].SortOrder, i)
		}
	}

	// 重排后新增 key 仍追加到末尾
	inserted, err := s.AddKey("test", "sk-key-new", KeyTypeBoth, "", "")
	if err != nil || !inserted {
		t.Fatalf("AddKey new: inserted=%v err=%v", inserted, err)
	}
	recs, err = s.ListKeys()
	if err != nil {
		t.Fatalf("ListKeys after add: %v", err)
	}
	if recs[len(recs)-1].Key != "sk-key-new" {
		t.Errorf("新增 key 应在末尾, got %+v", recs[len(recs)-1])
	}
}
