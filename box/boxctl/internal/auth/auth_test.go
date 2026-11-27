package auth

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"boxctl/internal/store"
)

func init() { hashIterations = 1000 } // 测试提速

type fixture struct {
	svc   *Service
	clock time.Time
	pwd   string // 初始密码
}

func newFixture(t *testing.T) *fixture {
	ctx := context.Background()
	dir := t.TempDir()
	db, err := store.Open(ctx, filepath.Join(dir, "boxctl.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	f := &fixture{clock: time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)}
	f.svc = New(db, filepath.Join(dir, "initial-password"))
	f.svc.now = func() time.Time { return f.clock }

	created, err := f.svc.Bootstrap(ctx)
	if err != nil || !created {
		t.Fatalf("bootstrap: %v %v", created, err)
	}
	b, err := os.ReadFile(f.svc.InitialPasswordPath)
	if err != nil {
		t.Fatal(err)
	}
	f.pwd = strings.TrimSpace(string(b))
	return f
}

func TestBootstrapOnce(t *testing.T) {
	f := newFixture(t)
	if len(f.pwd) != 20 {
		t.Fatalf("initial password %q", f.pwd)
	}
	created, err := f.svc.Bootstrap(context.Background())
	if err != nil || created {
		t.Fatalf("second bootstrap: %v %v", created, err)
	}
}

func TestLoginAndMustChange(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	token, sess, err := f.svc.Login(ctx, "10.0.0.2", f.pwd)
	if err != nil {
		t.Fatal(err)
	}
	if !sess.MustChange || sess.CSRFToken == "" {
		t.Fatalf("session %+v", sess)
	}
	got, err := f.svc.Authenticate(ctx, token)
	if err != nil || got.CSRFToken != sess.CSRFToken {
		t.Fatalf("authenticate: %+v %v", got, err)
	}

	// 弱密码、与旧密码相同均拒绝
	if _, _, err := f.svc.ChangePassword(ctx, "10.0.0.2", f.pwd, "short"); !errors.Is(err, ErrWeakPassword) {
		t.Fatalf("weak: %v", err)
	}
	if _, _, err := f.svc.ChangePassword(ctx, "10.0.0.2", f.pwd, f.pwd); !errors.Is(err, ErrWeakPassword) {
		t.Fatalf("same: %v", err)
	}

	newToken, newSess, err := f.svc.ChangePassword(ctx, "10.0.0.2", f.pwd, "correct horse battery")
	if err != nil {
		t.Fatal(err)
	}
	if newSess.MustChange {
		t.Fatal("must_change still set")
	}
	// 旧会话作废、初始密码文件删除
	if _, err := f.svc.Authenticate(ctx, token); !errors.Is(err, ErrNoSession) {
		t.Fatalf("old session: %v", err)
	}
	if _, err := f.svc.Authenticate(ctx, newToken); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(f.svc.InitialPasswordPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("initial password file: %v", err)
	}
	if _, _, err := f.svc.Login(ctx, "10.0.0.2", f.pwd); !errors.Is(err, ErrBadPassword) {
		t.Fatalf("old password: %v", err)
	}
}

func TestSessionExpiresAndLogout(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	token, _, err := f.svc.Login(ctx, "10.0.0.2", f.pwd)
	if err != nil {
		t.Fatal(err)
	}
	f.clock = f.clock.Add(f.svc.SessionTTL + time.Second)
	if _, err := f.svc.Authenticate(ctx, token); !errors.Is(err, ErrNoSession) {
		t.Fatalf("expired: %v", err)
	}

	token, _, _ = f.svc.Login(ctx, "10.0.0.2", f.pwd)
	if err := f.svc.Logout(ctx, token); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Authenticate(ctx, token); !errors.Is(err, ErrNoSession) {
		t.Fatalf("after logout: %v", err)
	}
}

func TestLockoutPerIP(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	for i := 0; i < maxFails; i++ {
		if _, _, err := f.svc.Login(ctx, "10.0.0.66", "wrong"); !errors.Is(err, ErrBadPassword) {
			t.Fatalf("attempt %d: %v", i, err)
		}
	}
	// 被锁定后正确密码也拒绝
	var locked *LockedError
	if _, _, err := f.svc.Login(ctx, "10.0.0.66", f.pwd); !errors.As(err, &locked) {
		t.Fatalf("want locked, got %v", err)
	}
	if !locked.Until.Equal(f.clock.Add(lockDuration)) {
		t.Fatalf("until %v", locked.Until)
	}
	// 其它 IP 不受影响
	if _, _, err := f.svc.Login(ctx, "10.0.0.2", f.pwd); err != nil {
		t.Fatalf("other ip: %v", err)
	}
	// 锁定期过后恢复
	f.clock = f.clock.Add(lockDuration + time.Second)
	if _, _, err := f.svc.Login(ctx, "10.0.0.66", f.pwd); err != nil {
		t.Fatalf("after lock: %v", err)
	}
}

func TestResetPassword(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	token, _, _ := f.svc.Login(ctx, "10.0.0.2", f.pwd)
	if _, _, err := f.svc.ChangePassword(ctx, "10.0.0.2", f.pwd, "correct horse battery"); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.ResetPassword(ctx); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(f.svc.InitialPasswordPath)
	_, sess, err := f.svc.Login(ctx, "10.0.0.2", strings.TrimSpace(string(b)))
	if err != nil || !sess.MustChange {
		t.Fatalf("login after reset: %+v %v", sess, err)
	}
	if _, err := f.svc.Authenticate(ctx, token); !errors.Is(err, ErrNoSession) {
		t.Fatal("sessions not cleared")
	}
}

func TestPasswordHashFormat(t *testing.T) {
	h, err := HashPassword("pw")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(h, "pbkdf2-sha256$1000$") || !VerifyPassword("pw", h) || VerifyPassword("pw2", h) {
		t.Fatalf("hash %q", h)
	}
	for _, bad := range []string{"", "x$1$a$b", "pbkdf2-sha256$0$AA$AA", "pbkdf2-sha256$1$!!$AA"} {
		if VerifyPassword("pw", bad) {
			t.Errorf("accepted %q", bad)
		}
	}
}
