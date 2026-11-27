// Package auth 实现单管理员的登录、会话与密码管理（M1-06）。
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base32"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	MinPasswordLen = 10
	maxFails       = 5
	lockDuration   = 5 * time.Minute
	timeLayout     = time.RFC3339 // 固定格式 UTC，字符串比较即时间比较
)

var (
	ErrBadPassword  = errors.New("auth: wrong password")
	ErrLocked       = errors.New("auth: too many failed attempts")
	ErrNoSession    = errors.New("auth: no valid session")
	ErrWeakPassword = errors.New("auth: password does not meet policy")
)

type Session struct {
	CSRFToken  string
	MustChange bool
	ExpiresAt  time.Time
}

type Service struct {
	db *sql.DB
	// InitialPasswordPath 存放随机初始密码的文件；改密成功后删除。
	InitialPasswordPath string
	SessionTTL          time.Duration
	now                 func() time.Time

	mu    sync.Mutex
	fails map[string]*failState // 按来源 IP 计数，避免局域网内任何人把管理员锁死
}

type failState struct {
	count       int
	lockedUntil time.Time
}

func New(db *sql.DB, initialPasswordPath string) *Service {
	return &Service{
		db:                  db,
		InitialPasswordPath: initialPasswordPath,
		SessionTTL:          12 * time.Hour,
		now:                 time.Now,
		fails:               map[string]*failState{},
	}
}

// Bootstrap 在没有管理员时生成随机初始密码并写入 0600 文件。返回是否新建。
func (s *Service) Bootstrap(ctx context.Context) (bool, error) {
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM admin`).Scan(&n); err != nil {
		return false, err
	}
	if n > 0 {
		return false, nil
	}
	return true, s.resetToInitial(ctx)
}

// ResetPassword 供 SSH 上的 `boxctl reset-password` 使用：重新生成初始密码，清空所有会话。
func (s *Service) ResetPassword(ctx context.Context) error {
	return s.resetToInitial(ctx)
}

func (s *Service) resetToInitial(ctx context.Context) error {
	pw, err := randomPassword()
	if err != nil {
		return err
	}
	hash, err := HashPassword(pw)
	if err != nil {
		return err
	}
	// 先落盘密码文件：写失败则不改库，避免出现无人知道的密码
	if err := writePrivateFile(s.InitialPasswordPath, pw+"\n"); err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `INSERT INTO admin (id, password_hash, must_change, updated_at)
		VALUES (1, ?, 1, ?)
		ON CONFLICT(id) DO UPDATE SET password_hash = excluded.password_hash,
			must_change = 1, updated_at = excluded.updated_at`, hash, s.stamp(s.now())); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM sessions`); err != nil {
		return err
	}
	return tx.Commit()
}

// Login 校验密码并创建会话。返回的 token 只出现在 Cookie 中，库里只存其哈希。
func (s *Service) Login(ctx context.Context, clientIP, password string) (string, Session, error) {
	if until, locked := s.lockedUntil(clientIP); locked {
		return "", Session{}, &LockedError{Until: until}
	}
	var hash string
	var mustChange bool
	if err := s.db.QueryRowContext(ctx,
		`SELECT password_hash, must_change FROM admin WHERE id = 1`).Scan(&hash, &mustChange); err != nil {
		return "", Session{}, err
	}
	if !VerifyPassword(password, hash) {
		s.recordFail(clientIP)
		return "", Session{}, ErrBadPassword
	}
	s.clearFails(clientIP)
	return s.createSession(ctx, s.db, mustChange)
}

func (s *Service) Authenticate(ctx context.Context, token string) (Session, error) {
	if token == "" {
		return Session{}, ErrNoSession
	}
	var sess Session
	var expires string
	err := s.db.QueryRowContext(ctx, `SELECT s.csrf_token, s.expires_at, a.must_change
		FROM sessions s, admin a
		WHERE s.token_hash = ? AND s.expires_at > ? AND a.id = 1`,
		hashToken(token), s.stamp(s.now())).Scan(&sess.CSRFToken, &expires, &sess.MustChange)
	if errors.Is(err, sql.ErrNoRows) {
		return Session{}, ErrNoSession
	}
	if err != nil {
		return Session{}, err
	}
	sess.ExpiresAt, _ = time.Parse(timeLayout, expires)
	return sess, nil
}

func (s *Service) Logout(ctx context.Context, token string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE token_hash = ?`, hashToken(token))
	return err
}

// ChangePassword 校验旧密码后更新，作废所有会话（包括其它浏览器），并签发新会话。
func (s *Service) ChangePassword(ctx context.Context, clientIP, oldPassword, newPassword string) (string, Session, error) {
	if until, locked := s.lockedUntil(clientIP); locked {
		return "", Session{}, &LockedError{Until: until}
	}
	var hash string
	if err := s.db.QueryRowContext(ctx, `SELECT password_hash FROM admin WHERE id = 1`).Scan(&hash); err != nil {
		return "", Session{}, err
	}
	if !VerifyPassword(oldPassword, hash) {
		s.recordFail(clientIP)
		return "", Session{}, ErrBadPassword
	}
	if len([]rune(newPassword)) < MinPasswordLen || newPassword == oldPassword {
		return "", Session{}, ErrWeakPassword
	}
	newHash, err := HashPassword(newPassword)
	if err != nil {
		return "", Session{}, err
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", Session{}, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `UPDATE admin SET password_hash = ?, must_change = 0, updated_at = ? WHERE id = 1`,
		newHash, s.stamp(s.now())); err != nil {
		return "", Session{}, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM sessions`); err != nil {
		return "", Session{}, err
	}
	token, sess, err := s.createSession(ctx, tx, false)
	if err != nil {
		return "", Session{}, err
	}
	if err := tx.Commit(); err != nil {
		return "", Session{}, err
	}
	s.clearFails(clientIP)
	if err := os.Remove(s.InitialPasswordPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return token, sess, err
	}
	return token, sess, nil
}

type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

func (s *Service) createSession(ctx context.Context, db execer, mustChange bool) (string, Session, error) {
	token, err := randomToken()
	if err != nil {
		return "", Session{}, err
	}
	csrf, err := randomToken()
	if err != nil {
		return "", Session{}, err
	}
	now := s.now()
	sess := Session{CSRFToken: csrf, MustChange: mustChange, ExpiresAt: now.Add(s.SessionTTL).UTC().Truncate(time.Second)}
	if _, err := db.ExecContext(ctx, `DELETE FROM sessions WHERE expires_at <= ?`, s.stamp(now)); err != nil {
		return "", Session{}, err
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO sessions (token_hash, csrf_token, created_at, expires_at)
		VALUES (?, ?, ?, ?)`, hashToken(token), csrf, s.stamp(now), s.stamp(sess.ExpiresAt)); err != nil {
		return "", Session{}, err
	}
	return token, sess, nil
}

// LockedError 携带解锁时间，供 API 返回 Retry-After。
type LockedError struct{ Until time.Time }

func (e *LockedError) Error() string { return ErrLocked.Error() }
func (e *LockedError) Unwrap() error { return ErrLocked }

func (s *Service) lockedUntil(ip string) (time.Time, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f := s.fails[ip]
	if f == nil {
		return time.Time{}, false
	}
	if s.now().Before(f.lockedUntil) {
		return f.lockedUntil, true
	}
	if !f.lockedUntil.IsZero() { // 锁定期已过，重新计数
		delete(s.fails, ip)
	}
	return time.Time{}, false
}

func (s *Service) recordFail(ip string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f := s.fails[ip]
	if f == nil {
		f = &failState{}
		s.fails[ip] = f
	}
	f.count++
	if f.count >= maxFails {
		f.lockedUntil = s.now().Add(lockDuration)
	}
}

func (s *Service) clearFails(ip string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.fails, ip)
}

func (s *Service) stamp(t time.Time) string { return t.UTC().Format(timeLayout) }

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func randomToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// randomPassword 生成 20 位小写 base32（约 100 bit），便于从文件抄写。
func randomPassword() (string, error) {
	b := make([]byte, 13)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	s := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b)
	return strings.ToLower(s[:20]), nil
}

func writePrivateFile(path, content string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		return err
	}
	return os.Chmod(path, 0o600) // 文件已存在时 WriteFile 不会收紧权限
}
