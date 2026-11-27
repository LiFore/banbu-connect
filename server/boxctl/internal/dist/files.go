package dist

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"boxctl/internal/objstore"
)

// 云盘（S1.11）：管理端上传文件（浏览器分片直传到存储），写简短说明、排序、置顶；所有绑定的客户端可见已发布的文件。

const (
	maxDescription  = 2000    // 说明是简短的 Markdown
	maxMarkdownRead = 2 << 20 // 客户端直接阅读的 Markdown 文件上限
	uploadTTL       = 24 * time.Hour
)

// File 是云盘里的一个文件。
type File struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	FileName    string `json:"file_name"`
	Storage     string `json:"storage"`
	ObjectKey   string `json:"-"`
	Size        int64  `json:"size"`
	Description string `json:"description"`
	Pinned      bool   `json:"pinned"`
	Sort        int64  `json:"sort"`
	Published   bool   `json:"published"`
	CreatedAt   string `json:"created_at"`
	UpdatedAt   string `json:"updated_at"`
	Markdown    bool   `json:"markdown"` // .md 文件，客户端可以直接阅读
}

const selectFile = `SELECT id, name, file_name, storage, object_key, size, description, pinned, sort, published, created_at, updated_at FROM files`

// 置顶的在前，组内按 sort，sort 相同时新的在前。
const fileOrder = ` ORDER BY pinned DESC, sort ASC, id DESC`

func scanFile(sc interface{ Scan(...any) error }) (File, error) {
	var f File
	err := sc.Scan(&f.ID, &f.Name, &f.FileName, &f.Storage, &f.ObjectKey, &f.Size, &f.Description, &f.Pinned, &f.Sort, &f.Published,
		&f.CreatedAt, &f.UpdatedAt)
	f.Markdown = isMarkdown(f.FileName)
	return f, err
}

func isMarkdown(name string) bool {
	switch strings.ToLower(path.Ext(name)) {
	case ".md", ".markdown":
		return true
	}
	return false
}

// Files 列出文件；onlyPublished 时只列已发布的（给客户端）。
func (s *Server) Files(ctx context.Context, onlyPublished bool) ([]File, error) {
	q := selectFile
	if onlyPublished {
		q += ` WHERE published = 1`
	}
	rows, err := s.DB.QueryContext(ctx, q+fileOrder)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []File{}
	for rows.Next() {
		f, err := scanFile(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

func (s *Server) file(ctx context.Context, id int64) (File, error) {
	f, err := scanFile(s.DB.QueryRowContext(ctx, selectFile+` WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return f, ErrNotFound
	}
	return f, err
}

// cleanFileName 去掉路径和控制字符，保留中文等字符（下载时按 UTF-8 文件名保存）。
func cleanFileName(name string) string {
	name = path.Base(strings.ReplaceAll(strings.TrimSpace(name), "\\", "/"))
	name = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f || strings.ContainsRune(`<>:"/\|?*`, r) {
			return '_'
		}
		return r
	}, name)
	if name == "." || name == ".." || name == "/" {
		return ""
	}
	if utf8.RuneCountInString(name) > 200 {
		name = string([]rune(name)[:200])
	}
	return name
}

// FileEdit 是可编辑的字段，nil 表示不改。
type FileEdit struct {
	Name        *string `json:"name"`
	FileName    *string `json:"file_name"`
	Description *string `json:"description"`
	Pinned      *bool   `json:"pinned"`
	Published   *bool   `json:"published"`
}

func (e FileEdit) check() error {
	if e.Name != nil && strings.TrimSpace(*e.Name) == "" {
		return &InputError{"标题不能为空"}
	}
	if e.Name != nil && utf8.RuneCountInString(*e.Name) > 200 {
		return &InputError{"标题不能超过 200 字"}
	}
	if e.FileName != nil && cleanFileName(*e.FileName) == "" {
		return &InputError{"文件名无效"}
	}
	if e.Description != nil && utf8.RuneCountInString(*e.Description) > maxDescription {
		return &InputError{fmt.Sprintf("说明不能超过 %d 字；长内容请上传 Markdown 文件", maxDescription)}
	}
	return nil
}

// UpdateFile 修改文件的标题、文件名、说明、置顶、发布。改置顶时排到新分组的最前。
func (s *Server) UpdateFile(ctx context.Context, id int64, e FileEdit) (File, error) {
	if err := e.check(); err != nil {
		return File{}, err
	}
	f, err := s.file(ctx, id)
	if err != nil {
		return File{}, err
	}
	if e.Name != nil {
		f.Name = strings.TrimSpace(*e.Name)
	}
	if e.FileName != nil {
		f.FileName = cleanFileName(*e.FileName)
	}
	if e.Description != nil {
		f.Description = strings.TrimSpace(*e.Description)
	}
	if e.Pinned != nil && *e.Pinned != f.Pinned {
		f.Pinned = *e.Pinned
		if f.Sort, err = s.topSort(ctx, f.Pinned); err != nil {
			return File{}, err
		}
	}
	if e.Published != nil {
		f.Published = *e.Published
	}
	err = s.execOne(ctx, `UPDATE files SET name = ?, file_name = ?, description = ?, pinned = ?, sort = ?, published = ?,
		updated_at = strftime('%Y-%m-%dT%H:%M:%SZ','now') WHERE id = ?`,
		f.Name, f.FileName, f.Description, f.Pinned, f.Sort, f.Published, id)
	if err != nil {
		return File{}, err
	}
	return s.file(ctx, id)
}

// topSort 返回排在某个分组最前的 sort 值。
func (s *Server) topSort(ctx context.Context, pinned bool) (int64, error) {
	var min sql.NullInt64
	if err := s.DB.QueryRowContext(ctx, `SELECT MIN(sort) FROM files WHERE pinned = ?`, pinned).Scan(&min); err != nil {
		return 0, err
	}
	if !min.Valid {
		return 0, nil
	}
	return min.Int64 - 1, nil
}

// ReorderFiles 按给定顺序重排（页面拖拽后提交一个分组的完整顺序）。
func (s *Server) ReorderFiles(ctx context.Context, ids []int64) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for i, id := range ids {
		if _, err := tx.ExecContext(ctx, `UPDATE files SET sort = ? WHERE id = ?`, i, id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// DeleteFile 删除记录和存储里的文件。
func (s *Server) DeleteFile(ctx context.Context, id int64) error {
	f, err := s.file(ctx, id)
	if err != nil {
		return err
	}
	if err := s.execOne(ctx, `DELETE FROM files WHERE id = ?`, id); err != nil {
		return err
	}
	s.removeObject(ctx, f.Storage, f.ObjectKey)
	return nil
}

func (s *Server) removeObject(ctx context.Context, kind, key string) {
	store, err := s.storeFor(ctx, kind)
	if err == nil {
		err = store.Delete(ctx, key)
	}
	if err != nil {
		slog.Warn("delete object failed", "storage", kind, "key", key, "err", err)
	}
}

// OpenFile 返回已发布文件所在的存储（给客户端下载）。
func (s *Server) OpenFile(ctx context.Context, id int64, onlyPublished bool) (File, objstore.Store, error) {
	f, err := s.file(ctx, id)
	if err == nil && onlyPublished && !f.Published {
		err = ErrNotFound
	}
	if err != nil {
		return File{}, nil, err
	}
	store, err := s.storeFor(ctx, f.Storage)
	return f, store, err
}

// FileContent 读出 Markdown 文件的内容（客户端直接阅读）。
func (s *Server) FileContent(ctx context.Context, id int64, onlyPublished bool) (string, error) {
	f, store, err := s.OpenFile(ctx, id, onlyPublished)
	if err != nil {
		return "", err
	}
	if !f.Markdown {
		return "", &InputError{"只能直接阅读 Markdown 文件"}
	}
	if f.Size > maxMarkdownRead {
		return "", &InputError{"文件太大，请下载后阅读"}
	}
	rc, _, err := store.Open(ctx, f.ObjectKey)
	if errors.Is(err, objstore.ErrNotFound) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", err
	}
	defer rc.Close()
	b, err := io.ReadAll(io.LimitReader(rc, maxMarkdownRead))
	return string(b), err
}

// ---- 分片上传 ----

// fileUpload 是进行中的上传，只在内存里：管理服务重启后作废，浏览器需要重新上传。
type fileUpload struct {
	ID        string
	Storage   string
	Key       string
	UploadID  string
	FileName  string
	Size      int64
	PartSize  int64
	Parts     int
	ReplaceID int64 // 替换已有文件时为它的 ID
	CreatedAt time.Time
}

// UploadPlan 是交给浏览器的上传计划：按 part_size 切片，依次 PUT 到对应地址，记下响应头 ETag。
type UploadPlan struct {
	ID       string               `json:"id"`
	Storage  string               `json:"storage"`
	PartSize int64                `json:"part_size"`
	Parts    []objstore.SignedURL `json:"parts"`
}

func randomID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// StartUpload 开始上传一个文件（replaceID > 0 时替换已有文件的内容，保留标题、说明和顺序）。
func (s *Server) StartUpload(ctx context.Context, fileName string, size, replaceID int64) (UploadPlan, error) {
	name := cleanFileName(fileName)
	if name == "" {
		return UploadPlan{}, &InputError{"文件名无效"}
	}
	if size <= 0 {
		return UploadPlan{}, &InputError{"文件是空的"}
	}
	if replaceID > 0 {
		if _, err := s.file(ctx, replaceID); err != nil {
			return UploadPlan{}, err
		}
	}
	s.pruneUploads(ctx)
	store, prefix, err := s.activeStore(ctx)
	if err != nil {
		return UploadPlan{}, err
	}
	key := prefix + "files/" + time.Now().UTC().Format("200601") + "/" + randomID()[:12] + "/" + name
	uploadID, err := store.CreateUpload(ctx, key)
	if err != nil {
		return UploadPlan{}, err
	}
	partSize, n := objstore.PartSize(size)
	plan := UploadPlan{ID: randomID(), Storage: store.Kind(), PartSize: partSize, Parts: make([]objstore.SignedURL, 0, n)}
	for i := 1; i <= n; i++ {
		u, err := store.SignPart(ctx, key, uploadID, i, uploadTTL)
		if err != nil {
			_ = store.AbortUpload(ctx, key, uploadID)
			return UploadPlan{}, err
		}
		plan.Parts = append(plan.Parts, u)
	}
	s.storage.uploadMu.Lock()
	if s.storage.uploads == nil {
		s.storage.uploads = map[string]*fileUpload{}
	}
	s.storage.uploads[plan.ID] = &fileUpload{ID: plan.ID, Storage: store.Kind(), Key: key, UploadID: uploadID, FileName: name, Size: size,
		PartSize: partSize, Parts: n, ReplaceID: replaceID, CreatedAt: time.Now()}
	s.storage.uploadMu.Unlock()
	return plan, nil
}

func (s *Server) takeUpload(id string) (*fileUpload, bool) {
	s.storage.uploadMu.Lock()
	defer s.storage.uploadMu.Unlock()
	u, ok := s.storage.uploads[id]
	delete(s.storage.uploads, id)
	return u, ok
}

// pruneUploads 放弃超过有效期还没完成的上传。
func (s *Server) pruneUploads(ctx context.Context) {
	s.storage.uploadMu.Lock()
	stale := []*fileUpload{}
	for id, u := range s.storage.uploads {
		if time.Since(u.CreatedAt) > uploadTTL {
			stale = append(stale, u)
			delete(s.storage.uploads, id)
		}
	}
	s.storage.uploadMu.Unlock()
	for _, u := range stale {
		if store, err := s.storeFor(ctx, u.Storage); err == nil {
			_ = store.AbortUpload(ctx, u.Key, u.UploadID)
		}
	}
	s.localStore().PruneUploads(uploadTTL + time.Hour)
}

// UploadDone 是浏览器传完所有分片后提交的内容。
type UploadDone struct {
	Parts       []objstore.Part `json:"parts"`
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Pinned      bool            `json:"pinned"`
	Published   bool            `json:"published"`
}

// CompleteUpload 合并分片并登记文件（或替换已有文件的内容）。
func (s *Server) CompleteUpload(ctx context.Context, id string, in UploadDone) (File, error) {
	u, ok := s.takeUpload(id)
	if !ok {
		return File{}, &InputError{"上传不存在或已过期，请重新上传"}
	}
	store, err := s.storeFor(ctx, u.Storage)
	if err != nil {
		return File{}, err
	}
	abort := func(e error) (File, error) {
		_ = store.AbortUpload(ctx, u.Key, u.UploadID)
		return File{}, e
	}
	if len(in.Parts) != u.Parts {
		return abort(&InputError{fmt.Sprintf("分片数量不对：应为 %d，收到 %d", u.Parts, len(in.Parts))})
	}
	name := strings.TrimSpace(in.Name)
	if name == "" {
		name = u.FileName
	}
	desc := strings.TrimSpace(in.Description)
	if err := (FileEdit{Name: &name, Description: &desc}).check(); err != nil {
		return abort(err)
	}
	if err := store.CompleteUpload(ctx, u.Key, u.UploadID, in.Parts); err != nil {
		return abort(fmt.Errorf("合并分片失败：%w", err))
	}
	size, err := store.Stat(ctx, u.Key)
	if err != nil {
		return File{}, err
	}
	if size != u.Size {
		_ = store.Delete(ctx, u.Key)
		return File{}, &InputError{fmt.Sprintf("上传后的大小是 %d 字节，与文件的 %d 字节不一致，请重新上传", size, u.Size)}
	}
	if u.ReplaceID > 0 {
		old, err := s.file(ctx, u.ReplaceID)
		if err != nil {
			_ = store.Delete(ctx, u.Key)
			return File{}, err
		}
		if err := s.execOne(ctx, `UPDATE files SET file_name = ?, storage = ?, object_key = ?, size = ?,
			updated_at = strftime('%Y-%m-%dT%H:%M:%SZ','now') WHERE id = ?`, u.FileName, u.Storage, u.Key, size, old.ID); err != nil {
			return File{}, err
		}
		s.removeObject(ctx, old.Storage, old.ObjectKey)
		return s.file(ctx, old.ID)
	}
	sort, err := s.topSort(ctx, in.Pinned)
	if err != nil {
		return File{}, err
	}
	res, err := s.DB.ExecContext(ctx, `INSERT INTO files (name, file_name, storage, object_key, size, description, pinned, sort, published)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`, name, u.FileName, u.Storage, u.Key, size, desc, in.Pinned, sort, in.Published)
	if err != nil {
		return File{}, err
	}
	fid, _ := res.LastInsertId()
	return s.file(ctx, fid)
}

// AbortUpload 取消上传。
func (s *Server) AbortUpload(ctx context.Context, id string) error {
	u, ok := s.takeUpload(id)
	if !ok {
		return nil
	}
	store, err := s.storeFor(ctx, u.Storage)
	if err != nil {
		return err
	}
	return store.AbortUpload(ctx, u.Key, u.UploadID)
}

// ---- 接口 ----

func (s *Server) fileAdminRoutes(mux *http.ServeMux, authed func(http.HandlerFunc) http.Handler, log *slog.Logger) {
	fail := func(w http.ResponseWriter, err error) { writeErr(w, err, log) }
	withID := func(pattern string, fn func(http.ResponseWriter, *http.Request, int64)) {
		mux.Handle(pattern, authed(func(w http.ResponseWriter, r *http.Request) {
			id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
			if err != nil {
				fail(w, ErrNotFound)
				return
			}
			fn(w, r, id)
		}))
	}
	mux.Handle("GET /api/v1/dist/files", authed(func(w http.ResponseWriter, r *http.Request) {
		list, err := s.Files(r.Context(), false)
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, http.StatusOK, list)
	}))
	withID("PUT /api/v1/dist/files/{id}", func(w http.ResponseWriter, r *http.Request, id int64) {
		var in FileEdit
		if !decode(w, r, &in) {
			return
		}
		f, err := s.UpdateFile(r.Context(), id, in)
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, http.StatusOK, f)
	})
	withID("DELETE /api/v1/dist/files/{id}", func(w http.ResponseWriter, r *http.Request, id int64) {
		if err := s.DeleteFile(r.Context(), id); err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	})
	mux.Handle("PUT /api/v1/dist/files-order", authed(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			IDs []int64 `json:"ids"`
		}
		if !decode(w, r, &in) {
			return
		}
		if err := s.ReorderFiles(r.Context(), in.IDs); err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	}))
	// 管理页面预览：下载（跳转到签名地址）和 Markdown 内容。
	withID("GET /api/v1/dist/files/{id}/download", func(w http.ResponseWriter, r *http.Request, id int64) {
		f, store, err := s.OpenFile(r.Context(), id, false)
		if err != nil {
			fail(w, err)
			return
		}
		u, err := store.SignGet(r.Context(), f.ObjectKey, f.FileName, signedGetTTL)
		if err != nil {
			fail(w, err)
			return
		}
		http.Redirect(w, r, u.URL, http.StatusFound)
	})
	withID("GET /api/v1/dist/files/{id}/content", func(w http.ResponseWriter, r *http.Request, id int64) {
		text, err := s.FileContent(r.Context(), id, false)
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"content": text})
	})
	mux.Handle("POST /api/v1/dist/file-uploads", authed(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			FileName  string `json:"file_name"`
			Size      int64  `json:"size"`
			ReplaceID int64  `json:"replace_id"`
		}
		if !decode(w, r, &in) {
			return
		}
		plan, err := s.StartUpload(r.Context(), in.FileName, in.Size, in.ReplaceID)
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, http.StatusOK, plan)
	}))
	mux.Handle("POST /api/v1/dist/file-uploads/{uid}/complete", authed(func(w http.ResponseWriter, r *http.Request) {
		var in UploadDone
		if !decode(w, r, &in) {
			return
		}
		f, err := s.CompleteUpload(r.Context(), r.PathValue("uid"), in)
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, http.StatusOK, f)
	}))
	mux.Handle("DELETE /api/v1/dist/file-uploads/{uid}", authed(func(w http.ResponseWriter, r *http.Request) {
		if err := s.AbortUpload(r.Context(), r.PathValue("uid")); err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	}))
}

// fileClientRoutes：客户端列文件、下载、读 Markdown。只看得到已发布的文件。
func (s *Server) fileClientRoutes(mux *http.ServeMux, device func(func(http.ResponseWriter, *http.Request, Device)) http.Handler, log *slog.Logger) {
	parse := func(w http.ResponseWriter, r *http.Request) (int64, bool) {
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil {
			writeErr(w, ErrNotFound, log)
			return 0, false
		}
		return id, true
	}
	mux.Handle("GET /api/v1/client/files", device(func(w http.ResponseWriter, r *http.Request, _ Device) {
		list, err := s.Files(r.Context(), true)
		if err != nil {
			writeErr(w, err, log)
			return
		}
		writeJSON(w, http.StatusOK, list)
	}))
	mux.Handle("GET /api/v1/client/files/{id}/download", device(func(w http.ResponseWriter, r *http.Request, _ Device) {
		id, ok := parse(w, r)
		if !ok {
			return
		}
		f, store, err := s.OpenFile(r.Context(), id, true)
		if err != nil {
			writeErr(w, err, log)
			return
		}
		s.serveObject(w, r, store, f.ObjectKey, f.FileName, log)
	}))
	mux.Handle("GET /api/v1/client/files/{id}/content", device(func(w http.ResponseWriter, r *http.Request, _ Device) {
		id, ok := parse(w, r)
		if !ok {
			return
		}
		text, err := s.FileContent(r.Context(), id, true)
		if err != nil {
			writeErr(w, err, log)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"content": text})
	}))
}
