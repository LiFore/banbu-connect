package core

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"

	"boxctl/internal/store"
)

// 客户端「退出」（ADR-085）：Pause 停内核但不改设置，Resume 按设置恢复；明确启停后以设置为准。
func TestPauseKeepsSettings(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "boxctl.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	st := Store{DB: db, Key: "test", Defaults: func() Settings { return Settings{Running: true, Mode: "rule"} }}
	s := NewSupervisor(Data{}, st, Hooks{}, "", t.TempDir(), "x", slog.New(slog.NewTextHandler(io.Discard, nil)))

	s.Pause(ctx)
	if s.Status().Desired {
		t.Fatal("暂停后状态应为不运行")
	}
	if got, _ := st.Load(ctx); !got.Running {
		t.Fatal("暂停不应改设置")
	}
	if err := s.apply(ctx); err != nil || s.AppliedConfig() != nil {
		t.Fatalf("暂停时不应生成配置：%v", err)
	}
	if !s.Resume(ctx) || !s.Status().Desired || !s.Triggered() {
		t.Fatal("恢复后应按设置重新应用")
	}
	if s.Resume(ctx) {
		t.Fatal("没暂停时 Resume 返回 false")
	}

	s.Pause(ctx)
	if err := s.SetRunning(ctx, false); err != nil {
		t.Fatal(err)
	}
	if s.Resume(ctx) {
		t.Fatal("明确停止后不再算暂停")
	}
}
