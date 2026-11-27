package store

import (
	"context"
	"path/filepath"
	"testing"
)

func TestOpenMigratesIdempotently(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "sub", "boxctl.db")

	for i := 0; i < 2; i++ {
		db, err := Open(ctx, path)
		if err != nil {
			t.Fatalf("open #%d: %v", i, err)
		}
		v, err := SchemaVersion(ctx, db)
		if err != nil {
			t.Fatal(err)
		}
		ms, _ := loadMigrations()
		if want := ms[len(ms)-1].version; v != want {
			t.Fatalf("schema version = %d, want %d", v, want)
		}
		var n int
		if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM schema_migrations`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != len(ms) {
			t.Fatalf("schema_migrations rows = %d, want %d", n, len(ms))
		}
		db.Close()
	}
}

func TestForeignKeysEnabled(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, filepath.Join(t.TempDir(), "boxctl.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	_, err = db.ExecContext(ctx, `INSERT INTO proxy_nodes
		(id, subscription_id, kind, protocol, display_name, server, port)
		VALUES ('n1', 'missing-sub', 'subscription', 'trojan', 'x', 'example.com', 443)`)
	if err == nil {
		t.Fatal("want foreign key violation")
	}
}
