package desktop

import (
	"context"
	"database/sql"

	"boxctl/internal/core"
	"boxctl/internal/groups"
)

// Settings 是桌面客户端的内核设置，与路由盒共用结构。
type Settings = core.Settings

const settingsKey = "desktop"

func defaultSettings() Settings {
	return Settings{
		Mode: "rule", TUN: true, Running: false, MixedPort: 7890, DNSGuard: true, Diagnostics: true, ConnStats: true,
		CustomRules: core.DefaultCustomRules(),
	}
}

func settingsStore(db *sql.DB) core.Store {
	return core.Store{DB: db, Key: settingsKey, Defaults: defaultSettings}
}

func loadSettings(ctx context.Context, db *sql.DB) (Settings, error) {
	return settingsStore(db).Load(ctx)
}

func saveSettings(ctx context.Context, db *sql.DB, s Settings) error {
	return settingsStore(db).Save(ctx, s)
}

func seedGroups(ctx context.Context, db *sql.DB, svc *groups.Service) error {
	return core.SeedGroups(ctx, db, svc)
}
