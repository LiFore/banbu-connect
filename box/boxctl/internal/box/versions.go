package box

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"

	"boxctl/internal/core"
	"boxctl/internal/guard"
	"boxctl/internal/textdiff"
)

// 配置版本（M8-04）：内核配置、guard 策略、dnsmasq 片段三者任一变化就记一个新版本，三者共用一个版本号。
// 内核配置存成文件（含节点凭据），查看和对比时把凭据打码。

const (
	versionsDir  = "versions"
	keepVersions = 30
	diffContext  = 3
)

// Version 是一个配置版本的摘要。
type Version struct {
	ID         int64  `json:"id"`
	ConfigHash string `json:"config_hash"`
	Status     string `json:"status"`
	Reason     string `json:"reason,omitempty"`
	CreatedAt  string `json:"created_at"`
	Current    bool   `json:"current"` // 内核正在运行这份配置
}

// VersionDetail 是一个版本的三种产物，内核配置已打码。
type VersionDetail struct {
	Version
	Mihomo      string `json:"mihomo"`
	GuardPolicy string `json:"guard_policy"`
	Dnsmasq     string `json:"dnsmasq"`
}

// VersionDiff 是两个版本之间三种产物的差异（统一格式），没有差异的为空串。
type VersionDiff struct {
	From        int64  `json:"from"` // 0 表示与空内容对比（第一个版本）
	To          int64  `json:"to"`
	Mihomo      string `json:"mihomo"`
	GuardPolicy string `json:"guard_policy"`
	Dnsmasq     string `json:"dnsmasq"`
}

// versionPolicy 是策略里属于配置的部分：阻断和内核是否在运行是运行状态，不算版本变化。
func versionPolicy(p guard.Policy) (guard.Policy, error) {
	p.Hold, p.Reason, p.ForwardAll = false, "", true
	return p.Normalize()
}

func (b *Box) versionPath(id int64) string {
	return filepath.Join(b.opts.DataDir, versionsDir, strconv.FormatInt(id, 10)+".yaml")
}

// recordVersion 在三种产物任一变化时记录新版本。yaml 是正在运行的内核配置，p 是刚算出的 guard 策略。
func (b *Box) recordVersion(ctx context.Context, yaml []byte, p guard.Policy) error {
	if len(yaml) == 0 {
		return nil // 内核没在运行：没有生效的内核配置
	}
	vp, err := versionPolicy(p)
	if err != nil {
		return err
	}
	pj, err := json.MarshalIndent(vp, "", "  ")
	if err != nil {
		return err
	}
	policy, dnsmasq, hash := string(pj)+"\n", guard.DnsmasqConf(vp), core.ConfigHash(yaml)

	b.versionMu.Lock()
	defer b.versionMu.Unlock()
	var lastHash, lastPolicy, lastDnsmasq string
	err = b.db.QueryRowContext(ctx, `SELECT config_hash, guard_policy, dnsmasq FROM config_versions ORDER BY id DESC LIMIT 1`).
		Scan(&lastHash, &lastPolicy, &lastDnsmasq)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if lastHash == hash && lastPolicy == policy && lastDnsmasq == dnsmasq {
		return nil
	}
	status, reason := string(core.ConfirmPending), ""
	if st := b.sup.Status(); st.ConfigHash == hash && st.Confirm != "" {
		status, reason = string(st.Confirm), st.ConfirmReason
	}
	// 回滚后运行的是旧版本的内核配置：沿用它原来的确认结果。
	if lastHash != hash {
		var prev string
		if b.db.QueryRowContext(ctx, `SELECT status FROM config_versions WHERE config_hash = ? ORDER BY id DESC LIMIT 1`, hash).Scan(&prev) == nil &&
			prev == string(core.ConfirmOK) && status == string(core.ConfirmPending) {
			status = prev
		}
	}
	res, err := b.db.ExecContext(ctx, `INSERT INTO config_versions (config_hash, guard_policy, dnsmasq, status, reason) VALUES (?, ?, ?, ?, ?)`,
		hash, policy, dnsmasq, status, reason)
	if err != nil {
		return err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(b.versionPath(id)), 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(b.versionPath(id), yaml, 0o600); err != nil {
		return err
	}
	return b.pruneVersions(ctx)
}

func (b *Box) pruneVersions(ctx context.Context) error {
	rows, err := b.db.QueryContext(ctx, `SELECT id FROM config_versions ORDER BY id DESC LIMIT -1 OFFSET ?`, keepVersions)
	if err != nil {
		return err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	rows.Close()
	for _, id := range ids {
		if _, err := b.db.ExecContext(ctx, `DELETE FROM config_versions WHERE id = ?`, id); err != nil {
			return err
		}
		_ = os.Remove(b.versionPath(id))
	}
	return nil
}

// onConfirm 把内核配置的确认结果写到对应的版本上。
func (b *Box) onConfirm(ev core.ConfirmEvent) {
	b.versionMu.Lock()
	defer b.versionMu.Unlock()
	_, err := b.db.ExecContext(context.Background(), `UPDATE config_versions SET status = ?, reason = ?
		WHERE id = (SELECT id FROM config_versions WHERE config_hash = ? ORDER BY id DESC LIMIT 1)`,
		string(ev.State), ev.Reason, ev.Hash)
	if err != nil {
		b.log.Warn("记录配置确认结果失败", "err", err)
	}
	if ev.State == core.ConfirmRolledBack || ev.State == core.ConfirmFailed {
		b.log.Warn("配置确认失败", "state", ev.State, "reason", ev.Reason)
	}
}

func (b *Box) listVersions(ctx context.Context) ([]Version, error) {
	rows, err := b.db.QueryContext(ctx, `SELECT id, config_hash, status, reason, created_at FROM config_versions ORDER BY id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	current := b.sup.Status().ConfigHash
	if !b.sup.Status().Running {
		current = ""
	}
	out := []Version{}
	marked := false
	for rows.Next() {
		var v Version
		if err := rows.Scan(&v.ID, &v.ConfigHash, &v.Status, &v.Reason, &v.CreatedAt); err != nil {
			return nil, err
		}
		// 同一份内核配置可能对应多个版本（只有策略变了）：最新的那个是当前版本。
		if !marked && v.ConfigHash == current {
			v.Current, marked = true, true
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func (b *Box) version(ctx context.Context, id int64) (VersionDetail, error) {
	var d VersionDetail
	err := b.db.QueryRowContext(ctx, `SELECT id, config_hash, status, reason, created_at, guard_policy, dnsmasq FROM config_versions WHERE id = ?`, id).
		Scan(&d.ID, &d.ConfigHash, &d.Status, &d.Reason, &d.CreatedAt, &d.GuardPolicy, &d.Dnsmasq)
	if err != nil {
		return d, err
	}
	yaml, err := os.ReadFile(b.versionPath(id))
	if err != nil {
		return d, fmt.Errorf("读取内核配置失败：%w", err)
	}
	d.Mihomo = MaskSecrets(string(yaml))
	return d, nil
}

const secretKeys = `password|passwd|uuid|secret|private-key|pre-shared-key|psk|auth|auth-str|auth_str|token|obfs-password|username|public-key|short-id`

// secretRE 匹配 YAML 里的凭据字段：节点密码、UUID、预共享密钥、私钥、控制接口密钥等。
// inlineSecretRE 匹配流式写法 {name: a, password: x} 里的同名字段。
var (
	secretRE       = regexp.MustCompile(`(?m)^(\s*(?:-\s+)?(?:` + secretKeys + `)\s*:\s*)(\S.*)$`)
	inlineSecretRE = regexp.MustCompile(`([{,]\s*(?:` + secretKeys + `)\s*:\s*)([^,}\n]+)`)
)

// MaskSecrets 把内核配置里的凭据打码，用于查看、对比和诊断包。
func MaskSecrets(yaml string) string {
	yaml = secretRE.ReplaceAllString(yaml, "${1}'******'")
	return inlineSecretRE.ReplaceAllString(yaml, "${1}'******'")
}

func (b *Box) diffVersions(ctx context.Context, from, to int64) (VersionDiff, error) {
	out := VersionDiff{From: from, To: to}
	b2, err := b.version(ctx, to)
	if err != nil {
		return out, err
	}
	var a VersionDetail
	if from > 0 {
		if a, err = b.version(ctx, from); err != nil {
			return out, err
		}
	}
	out.Mihomo = textdiff.Unified(a.Mihomo, b2.Mihomo, diffContext)
	out.GuardPolicy = textdiff.Unified(a.GuardPolicy, b2.GuardPolicy, diffContext)
	out.Dnsmasq = textdiff.Unified(a.Dnsmasq, b2.Dnsmasq, diffContext)
	return out, nil
}

func (b *Box) versionRoutes(mux *http.ServeMux, authed func(http.HandlerFunc) http.Handler) {
	idOf := func(w http.ResponseWriter, r *http.Request) (int64, bool) {
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil || id <= 0 {
			core.WriteJSON(w, http.StatusBadRequest, core.APIErr{Code: "bad_request", Message: "版本号无效"})
			return 0, false
		}
		return id, true
	}
	notFound := func(w http.ResponseWriter, err error) {
		if errors.Is(err, sql.ErrNoRows) || errors.Is(err, os.ErrNotExist) {
			core.WriteJSON(w, http.StatusNotFound, core.APIErr{Code: "not_found", Message: "版本不存在"})
			return
		}
		core.Fail(w, err)
	}
	mux.Handle("GET /api/v1/box/versions", authed(func(w http.ResponseWriter, r *http.Request) {
		list, err := b.listVersions(r.Context())
		if err != nil {
			core.Fail(w, err)
			return
		}
		core.WriteJSON(w, http.StatusOK, list)
	}))
	mux.Handle("GET /api/v1/box/versions/{id}", authed(func(w http.ResponseWriter, r *http.Request) {
		id, ok := idOf(w, r)
		if !ok {
			return
		}
		d, err := b.version(r.Context(), id)
		if err != nil {
			notFound(w, err)
			return
		}
		core.WriteJSON(w, http.StatusOK, d)
	}))
	// 对比：base 不写时与上一个版本对比。
	mux.Handle("GET /api/v1/box/versions/{id}/diff", authed(func(w http.ResponseWriter, r *http.Request) {
		id, ok := idOf(w, r)
		if !ok {
			return
		}
		var base int64
		if s := r.URL.Query().Get("base"); s != "" {
			v, err := strconv.ParseInt(s, 10, 64)
			if err != nil || v < 0 {
				core.WriteJSON(w, http.StatusBadRequest, core.APIErr{Code: "bad_request", Message: "对比的版本号无效"})
				return
			}
			base = v
		} else if err := b.db.QueryRowContext(r.Context(), `SELECT id FROM config_versions WHERE id < ? ORDER BY id DESC LIMIT 1`, id).Scan(&base); err != nil && !errors.Is(err, sql.ErrNoRows) {
			core.Fail(w, err)
			return
		}
		d, err := b.diffVersions(r.Context(), base, id)
		if err != nil {
			notFound(w, err)
			return
		}
		core.WriteJSON(w, http.StatusOK, d)
	}))
}
