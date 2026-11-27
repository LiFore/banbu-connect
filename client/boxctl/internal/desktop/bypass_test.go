package desktop

import (
	"path/filepath"
	"testing"

	"boxctl/internal/dist"
	"boxctl/internal/killswitch"
)

func TestTrustBypass(t *testing.T) {
	path := filepath.Join(t.TempDir(), trustBypassFile)
	b := newTrustBypass(path, nil)
	if b.Enabled() {
		t.Fatal("default off")
	}
	if _, err := b.Set(true, "wrong"); err == nil || b.Enabled() {
		t.Fatal("wrong password must not enable")
	}
	if st, err := b.Set(true, trustBypassPassword); err != nil || !st.Enabled || st.Since.IsZero() {
		t.Fatalf("enable = %+v %v", st, err)
	}
	// 重启后保持。
	if !newTrustBypass(path, nil).Enabled() {
		t.Fatal("should persist")
	}
	// 关闭不要口令。
	if _, err := b.Set(false, ""); err != nil || b.Enabled() {
		t.Fatal("disable without password")
	}
	if newTrustBypass(path, nil).Enabled() {
		t.Fatal("disable should persist")
	}
	var nilB *trustBypass
	if nilB.Enabled() {
		t.Fatal("nil is off")
	}
}

// 打开后账号操作不再因保护状态被拦，关闭后恢复检查。
func TestTrustBypassSkipsWorkspaceCheck(t *testing.T) {
	// 默认配置：保护服务没开启，Check 必然拦。Status 只读内存状态，不碰后端。
	ks, err := killswitch.NewEngine(filepath.Join(t.TempDir(), "ks.json"), nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	w := newWorkspaces(ks, nil, nil)
	w.bypass = newTrustBypass("", nil)
	if err := w.Check(dist.KindCodex); err == nil {
		t.Fatal("protection off should block")
	}
	if _, err := w.Enqueue(WorkspaceAction{Action: "login", Kind: dist.KindCodex, Email: "a@x.com"}); err == nil {
		t.Fatal("enqueue should be blocked")
	}
	_, _ = w.bypass.Set(true, trustBypassPassword)
	if err := w.Check(dist.KindCodex); err != nil {
		t.Fatalf("bypass should skip: %v", err)
	}
	if _, err := w.Enqueue(WorkspaceAction{Action: "login", Kind: dist.KindCodex, Email: "a@x.com"}); err != nil {
		t.Fatalf("enqueue with bypass: %v", err)
	}
	_, _ = w.bypass.Set(false, "")
	if err := w.Check(dist.KindCodex); err == nil {
		t.Fatal("check resumes after disable")
	}
}
