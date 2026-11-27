package dist

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"
)

// ---- 用户 ----
//
// 用户有姓名，名下可以有多台设备（devices.user_id）。AI 账号可以分配给设备，也可以分配给用户：
// 分配给用户时，用户名下的所有设备（包括之后加入的）都能用。生效的设备 = 直接分配的设备 + 用户名下的设备。

const (
	scopeClaude = "claude" // claude_accounts：Claude 和 Codex
	scopeGemini = "gemini" // gemini_accounts

	maxUserName = 32
)

// User 是一个用户。
type User struct {
	ID        int64   `json:"id"`
	Name      string  `json:"name"`
	Note      string  `json:"note"`
	Devices   []int64 `json:"devices"` // 名下的设备（不含已吊销的）
	CreatedAt string  `json:"created_at"`
}

// UserInput 是新建或修改。Devices 为空时不改名下的设备（nil）；要清空请传空数组。
type UserInput struct {
	Name    string   `json:"name"`
	Note    string   `json:"note"`
	Devices *[]int64 `json:"devices"`
}

func (in *UserInput) normalize() error {
	in.Name = strings.TrimSpace(in.Name)
	in.Note = strings.TrimSpace(in.Note)
	if in.Name == "" {
		return &InputError{"请填写用户姓名"}
	}
	if utf8.RuneCountInString(in.Name) > maxUserName {
		return &InputError{fmt.Sprintf("姓名最多 %d 个字", maxUserName)}
	}
	if utf8.RuneCountInString(in.Note) > 200 {
		return &InputError{"备注最多 200 字"}
	}
	return nil
}

func (s *Server) Users(ctx context.Context) ([]User, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT id, name, note, created_at FROM users ORDER BY name, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []User{}
	index := map[int64]int{}
	for rows.Next() {
		var u User
		if err := rows.Scan(&u.ID, &u.Name, &u.Note, &u.CreatedAt); err != nil {
			return nil, err
		}
		u.Devices = []int64{}
		index[u.ID] = len(out)
		out = append(out, u)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	drows, err := s.DB.QueryContext(ctx, `SELECT id, user_id FROM devices WHERE user_id != 0 AND revoked = 0 ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer drows.Close()
	for drows.Next() {
		var id, uid int64
		if err := drows.Scan(&id, &uid); err != nil {
			return nil, err
		}
		if i, ok := index[uid]; ok {
			out[i].Devices = append(out[i].Devices, id)
		}
	}
	return out, drows.Err()
}

func (s *Server) user(ctx context.Context, id int64) (User, error) {
	list, err := s.Users(ctx)
	if err != nil {
		return User{}, err
	}
	for _, u := range list {
		if u.ID == id {
			return u, nil
		}
	}
	return User{}, ErrNotFound
}

func (s *Server) CreateUser(ctx context.Context, in UserInput) (User, error) {
	if err := in.normalize(); err != nil {
		return User{}, err
	}
	res, err := s.DB.ExecContext(ctx, `INSERT INTO users (name, note) VALUES (?, ?)`, in.Name, in.Note)
	if err != nil {
		return User{}, err
	}
	id, _ := res.LastInsertId()
	if in.Devices != nil {
		if err := s.SetUserDevices(ctx, id, *in.Devices); err != nil {
			_, _ = s.DB.ExecContext(ctx, `DELETE FROM users WHERE id = ?`, id)
			return User{}, err
		}
	}
	return s.user(ctx, id)
}

func (s *Server) UpdateUser(ctx context.Context, id int64, in UserInput) (User, error) {
	if err := in.normalize(); err != nil {
		return User{}, err
	}
	if err := s.execOne(ctx, `UPDATE users SET name = ?, note = ? WHERE id = ?`, in.Name, in.Note, id); err != nil {
		return User{}, err
	}
	if in.Devices != nil {
		if err := s.SetUserDevices(ctx, id, *in.Devices); err != nil {
			return User{}, err
		}
	}
	return s.user(ctx, id)
}

// DeleteUser 删除用户：名下的设备变成不属于任何用户，分配给这个用户的账号去掉这条分配
// （设备只能用直接分配给它的账号了）。
func (s *Server) DeleteUser(ctx context.Context, id int64) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `UPDATE devices SET user_id = 0 WHERE user_id = ?`, id); err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM users WHERE id = ?`, id) // ai_account_users 随外键级联删除
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.geminiChanged()
	return nil
}

// SetUserDevices 设置用户名下的设备：列表里的设备归这个用户（从原来的用户名下转过来），
// 原来在名下、不在列表里的设备变成不属于任何用户。
func (s *Server) SetUserDevices(ctx context.Context, userID int64, devices []int64) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var n int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM users WHERE id = ?`, userID).Scan(&n); err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	if _, err := tx.ExecContext(ctx, `UPDATE devices SET user_id = 0 WHERE user_id = ?`, userID); err != nil {
		return err
	}
	for _, d := range slices.Compact(slices.Sorted(slices.Values(devices))) {
		res, err := tx.ExecContext(ctx, `UPDATE devices SET user_id = ? WHERE id = ? AND revoked = 0`, userID, d)
		if err != nil {
			return err
		}
		if c, _ := res.RowsAffected(); c == 0 {
			return &InputError{fmt.Sprintf("客户端 %d 不存在或已吊销", d)}
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.geminiChanged()
	return nil
}

// SetDeviceUser 设置设备所属的用户，userID 为 0 表示不属于任何用户。
func (s *Server) SetDeviceUser(ctx context.Context, deviceID, userID int64) (Device, error) {
	if userID != 0 {
		if _, err := s.user(ctx, userID); err != nil {
			return Device{}, err
		}
	}
	if err := s.execOne(ctx, `UPDATE devices SET user_id = ? WHERE id = ?`, userID, deviceID); err != nil {
		return Device{}, err
	}
	s.geminiChanged()
	return scanDevice(s.DB.QueryRowContext(ctx, selectDevice+` WHERE id = ?`, deviceID))
}

// ---- 账号分配给用户 ----

// normalizeIDs 去掉非正数和重复，保持顺序。
func normalizeIDs(in []int64) []int64 {
	out := []int64{}
	for _, v := range in {
		if v > 0 && !slices.Contains(out, v) {
			out = append(out, v)
		}
	}
	return out
}

// setAccountUsers 替换账号分配的用户。用户必须存在。
func (s *Server) setAccountUsers(ctx context.Context, scope string, accountID int64, users []int64) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, u := range users {
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM users WHERE id = ?`, u).Scan(&n); err != nil {
			return err
		}
		if n == 0 {
			return &InputError{fmt.Sprintf("用户 %d 不存在", u)}
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM ai_account_users WHERE scope = ? AND account_id = ?`, scope, accountID); err != nil {
		return err
	}
	for _, u := range users {
		if _, err := tx.ExecContext(ctx, `INSERT INTO ai_account_users (scope, account_id, user_id) VALUES (?, ?, ?)`, scope, accountID, u); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Server) clearAccountUsers(ctx context.Context, scope string, accountID int64) {
	_, _ = s.DB.ExecContext(ctx, `DELETE FROM ai_account_users WHERE scope = ? AND account_id = ?`, scope, accountID)
}

// accountUsers 返回 scope 下每个账号分配的用户。
func (s *Server) accountUsers(ctx context.Context, scope string) (map[int64][]int64, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT account_id, user_id FROM ai_account_users WHERE scope = ? ORDER BY account_id, user_id`, scope)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64][]int64{}
	for rows.Next() {
		var a, u int64
		if err := rows.Scan(&a, &u); err != nil {
			return nil, err
		}
		out[a] = append(out[a], u)
	}
	return out, rows.Err()
}

// userDevices 返回每个用户名下的设备（不含已吊销的）。
func (s *Server) userDevices(ctx context.Context) (map[int64][]int64, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT user_id, id FROM devices WHERE user_id != 0 AND revoked = 0 ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64][]int64{}
	for rows.Next() {
		var u, d int64
		if err := rows.Scan(&u, &d); err != nil {
			return nil, err
		}
		out[u] = append(out[u], d)
	}
	return out, rows.Err()
}

// mergeDevices 是直接分配的设备加上用户名下的设备，去重、保持直接分配的在前。
func mergeDevices(direct, users []int64, byUser map[int64][]int64) []int64 {
	out := slices.Clone(direct)
	if out == nil {
		out = []int64{}
	}
	for _, u := range users {
		for _, d := range byUser[u] {
			if !slices.Contains(out, d) {
				out = append(out, d)
			}
		}
	}
	return out
}

// ---- 管理接口 ----

func (s *Server) userAdminRoutes(mux *http.ServeMux, authed func(http.HandlerFunc) http.Handler, log *slog.Logger) {
	fail := func(w http.ResponseWriter, err error) { writeErr(w, err, log) }
	pathID := func(w http.ResponseWriter, r *http.Request) (int64, bool) {
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil {
			fail(w, ErrNotFound)
			return 0, false
		}
		return id, true
	}
	mux.Handle("GET /api/v1/dist/users", authed(func(w http.ResponseWriter, r *http.Request) {
		list, err := s.Users(r.Context())
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, http.StatusOK, list)
	}))
	mux.Handle("POST /api/v1/dist/users", authed(func(w http.ResponseWriter, r *http.Request) {
		var in UserInput
		if !decode(w, r, &in) {
			return
		}
		u, err := s.CreateUser(r.Context(), in)
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, http.StatusOK, u)
	}))
	mux.Handle("PUT /api/v1/dist/users/{id}", authed(func(w http.ResponseWriter, r *http.Request) {
		id, ok := pathID(w, r)
		if !ok {
			return
		}
		var in UserInput
		if !decode(w, r, &in) {
			return
		}
		u, err := s.UpdateUser(r.Context(), id, in)
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, http.StatusOK, u)
	}))
	mux.Handle("DELETE /api/v1/dist/users/{id}", authed(func(w http.ResponseWriter, r *http.Request) {
		id, ok := pathID(w, r)
		if !ok {
			return
		}
		if err := s.DeleteUser(r.Context(), id); err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	}))
	// 设置设备所属的用户：user_id 为 0 表示不属于任何用户。
	mux.Handle("PUT /api/v1/dist/devices/{id}/user", authed(func(w http.ResponseWriter, r *http.Request) {
		id, ok := pathID(w, r)
		if !ok {
			return
		}
		var in struct {
			UserID int64 `json:"user_id"`
		}
		if !decode(w, r, &in) {
			return
		}
		d, err := s.SetDeviceUser(r.Context(), id, in.UserID)
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, http.StatusOK, d)
	}))
}
