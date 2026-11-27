package store

import (
	"context"
	"path/filepath"
	"testing"
)

// 0015：可用客户端名单为空的账号，补成当时所有未吊销的客户端（ADR-060），已有名单的不变。
func TestMigration0015KeepsExistingAccess(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, filepath.Join(t.TempDir(), "boxctl.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, q := range []string{
		`INSERT INTO devices (id, name, token_hash) VALUES (3, 'a', 'h3'), (1, 'b', 'h1')`,
		`INSERT INTO devices (id, name, token_hash, revoked) VALUES (2, 'c', 'h2', 1)`,
		`INSERT INTO claude_accounts (id, email, devices) VALUES (1, 'old@x.com', '[]'), (2, 'pinned@x.com', '[3]')`,
	} {
		if _, err := db.ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	body, err := migrationFS.ReadFile("migrations/0015_account_devices_explicit.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, string(body)); err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	rows, err := db.QueryContext(ctx, `SELECT email, devices FROM claude_accounts`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var e, d string
		_ = rows.Scan(&e, &d)
		got[e] = d
	}
	if got["old@x.com"] != "[1,3]" || got["pinned@x.com"] != "[3]" {
		t.Fatalf("devices = %v", got)
	}
}
