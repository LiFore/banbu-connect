package dist

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"
	"sync"

	"boxctl/internal/objstore"
)

// 对象存储（S1.11）：配置了阿里云 OSS 并启用后，新上传的更新包和云盘文件存到 OSS；
// 否则存在数据目录。每条记录记下自己存在哪里，切换配置后旧记录仍从原处读取。

const (
	storageKey = "object_storage"
	storageAAD = "object-storage"
)

// storageSetting 是保存在 settings 表里的配置，AccessKey Secret 加密保存。
type storageSetting struct {
	Enabled      bool               `json:"enabled"`
	OSS          objstore.OSSConfig `json:"oss"`
	SecretSealed string             `json:"secret_sealed,omitempty"`
}

// StorageView 是给管理页面看的配置，不含 Secret。
type StorageView struct {
	Enabled         bool   `json:"enabled"`
	Region          string `json:"region"`
	Endpoint        string `json:"endpoint"`
	Bucket          string `json:"bucket"`
	AccessKeyID     string `json:"access_key_id"`
	HasSecret       bool   `json:"has_secret"`
	Prefix          string `json:"prefix"`
	Active          string `json:"active"` // 新文件存到哪里：local / oss
	LocalReleases   int    `json:"local_releases"`
	LocalFiles      int    `json:"local_files"`
	AccessKeySecret string `json:"access_key_secret,omitempty"` // 只用于保存
}

type storageState struct {
	mu       sync.Mutex
	local    *objstore.Local
	ossKey   string // 当前 OSS 客户端对应的配置，变化时重建
	oss      *objstore.OSS
	csp      []string // ConnectSources 的缓存，保存配置后清空
	cspOK    bool
	uploadMu sync.Mutex
	uploads  map[string]*fileUpload
}

// localStore 是本机存储，根目录是数据目录（更新包目录 s.Dir 的上一级）。
func (s *Server) localStore() *objstore.Local {
	s.storage.mu.Lock()
	defer s.storage.mu.Unlock()
	if s.storage.local == nil {
		s.storage.local = objstore.NewLocal(filepath.Dir(filepath.Clean(s.Dir)))
	}
	return s.storage.local
}

// legacyReleaseKey 是旧的本机更新包在本机存储里的路径（releases/<平台>/<版本>/<文件>）。
func (s *Server) legacyReleaseKey(r Release) string {
	return filepath.Base(filepath.Clean(s.Dir)) + "/" + r.Target + "/" + r.Version + "/" + r.FileName
}

func (s *Server) loadStorage(ctx context.Context) (storageSetting, error) {
	var st storageSetting
	if err := s.loadSetting(ctx, storageKey, &st); err != nil {
		return st, err
	}
	if st.SecretSealed != "" {
		raw, err := base64.StdEncoding.DecodeString(st.SecretSealed)
		if err != nil {
			return st, err
		}
		plain, err := s.Secrets.Open(raw, storageAAD)
		if err != nil {
			return st, fmt.Errorf("解密 OSS 密钥失败：%w", err)
		}
		st.OSS.AccessKeySecret = string(plain)
	}
	return st, nil
}

// ossDefaultOrigin 是阿里云 OSS 标准访问域名，默认 Endpoint 签出的地址都在这下面。
// 无论是否已配置都放行：页面加载后再去配置 OSS，不刷新页面也能上传。
const ossDefaultOrigin = "https://*.aliyuncs.com"

// ConnectSources 是管理页面需要额外连接的来源（CSP connect-src）：浏览器把云盘文件分片直传到 OSS。
// 每个响应都会调用，结果缓存到下次保存存储配置。
func (s *Server) ConnectSources() []string {
	s.storage.mu.Lock()
	if s.storage.cspOK {
		defer s.storage.mu.Unlock()
		return s.storage.csp
	}
	s.storage.mu.Unlock()
	srcs := []string{ossDefaultOrigin}
	o, _, err := s.ossStore(context.Background())
	if o != nil {
		// 自定义域名或 http 的 Endpoint 不在默认来源里，单独放行。
		origin := o.Origin()
		covered := strings.HasPrefix(origin, "https://") && strings.HasSuffix(origin, ".aliyuncs.com")
		if origin != "" && !covered {
			srcs = append(srcs, origin)
		}
	}
	if err == nil {
		s.storage.mu.Lock()
		s.storage.csp, s.storage.cspOK = srcs, true
		s.storage.mu.Unlock()
	}
	return srcs
}

// ossStore 返回已配置的 OSS（不论是否启用，读旧记录时也要用）；没有配置时返回 nil。
func (s *Server) ossStore(ctx context.Context) (*objstore.OSS, bool, error) {
	st, err := s.loadStorage(ctx)
	if err != nil {
		return nil, false, err
	}
	if st.OSS.Bucket == "" || st.OSS.AccessKeySecret == "" {
		return nil, st.Enabled, nil
	}
	b, _ := json.Marshal(st.OSS)
	s.storage.mu.Lock()
	defer s.storage.mu.Unlock()
	if s.storage.oss != nil && s.storage.ossKey == string(b) {
		return s.storage.oss, st.Enabled, nil
	}
	o, err := objstore.NewOSS(st.OSS)
	if err != nil {
		return nil, st.Enabled, err
	}
	s.storage.oss, s.storage.ossKey = o, string(b)
	return o, st.Enabled, nil
}

// activeStore 是新文件要存的地方，prefix 是对象路径前缀（OSS 配置的前缀，本机为空）。
func (s *Server) activeStore(ctx context.Context) (objstore.Store, string, error) {
	o, enabled, err := s.ossStore(ctx)
	if err != nil {
		return nil, "", err
	}
	if enabled {
		if o == nil {
			return nil, "", &InputError{"已启用 OSS 但配置不完整，请在「文件」页的存储设置里补全"}
		}
		return o, o.Key(""), nil
	}
	return s.localStore(), "", nil
}

// storeFor 按记录上的存储类型取存储。
func (s *Server) storeFor(ctx context.Context, kind string) (objstore.Store, error) {
	if kind != objstore.KindOSS {
		return s.localStore(), nil
	}
	o, _, err := s.ossStore(ctx)
	if err != nil {
		return nil, err
	}
	if o == nil {
		return nil, errors.New("文件存在 OSS 上，但 OSS 配置已删除")
	}
	return o, nil
}

// Storage 返回存储配置。
func (s *Server) Storage(ctx context.Context) (StorageView, error) {
	st, err := s.loadStorage(ctx)
	if err != nil {
		return StorageView{}, err
	}
	v := StorageView{Enabled: st.Enabled, Region: st.OSS.Region, Endpoint: st.OSS.Endpoint, Bucket: st.OSS.Bucket,
		AccessKeyID: st.OSS.AccessKeyID, HasSecret: st.OSS.AccessKeySecret != "", Prefix: strings.TrimSuffix(st.OSS.Prefix, "/"),
		Active: objstore.KindLocal}
	if st.Enabled {
		v.Active = objstore.KindOSS
	}
	_ = s.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM releases WHERE storage = 'local'`).Scan(&v.LocalReleases)
	_ = s.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM files WHERE storage = 'local'`).Scan(&v.LocalFiles)
	return v, nil
}

// mergeStorage 把页面提交的配置合并到已保存的配置：Secret 留空表示不改。
func (s *Server) mergeStorage(ctx context.Context, in StorageView) (storageSetting, error) {
	old, err := s.loadStorage(ctx)
	if err != nil {
		return storageSetting{}, err
	}
	cfg := objstore.OSSConfig{Region: in.Region, Endpoint: in.Endpoint, Bucket: in.Bucket, AccessKeyID: in.AccessKeyID,
		AccessKeySecret: in.AccessKeySecret, Prefix: in.Prefix}
	if strings.TrimSpace(cfg.AccessKeySecret) == "" {
		cfg.AccessKeySecret = old.OSS.AccessKeySecret
	}
	st := storageSetting{Enabled: in.Enabled, OSS: cfg}
	empty := cfg.Region == "" && cfg.Bucket == "" && cfg.AccessKeyID == ""
	if !empty || st.Enabled {
		if err := st.OSS.Validate(); err != nil {
			return st, &InputError{err.Error()}
		}
	}
	return st, nil
}

// SaveStorage 保存存储配置。启用前先测试一次，避免存进去的文件读不出来。
func (s *Server) SaveStorage(ctx context.Context, in StorageView) error {
	st, err := s.mergeStorage(ctx, in)
	if err != nil {
		return err
	}
	if st.Enabled {
		o, err := objstore.NewOSS(st.OSS)
		if err != nil {
			return &InputError{err.Error()}
		}
		if err := o.Test(ctx); err != nil {
			return &InputError{"启用前测试失败：" + err.Error()}
		}
	}
	if st.OSS.AccessKeySecret != "" {
		sealed, err := s.Secrets.Seal([]byte(st.OSS.AccessKeySecret), storageAAD)
		if err != nil {
			return err
		}
		st.SecretSealed = base64.StdEncoding.EncodeToString(sealed)
	}
	st.OSS.AccessKeySecret = ""
	if err := s.saveSetting(ctx, storageKey, st); err != nil {
		return err
	}
	s.storage.mu.Lock()
	s.storage.cspOK = false
	s.storage.mu.Unlock()
	return nil
}

// StorageTest 是测试连接的结果。CORS 只影响管理页面上传云盘文件（浏览器直传）。
type StorageTest struct {
	OK        bool   `json:"ok"`
	Error     string `json:"error,omitempty"`
	CORSReady bool   `json:"cors_ready"`
	CORSError string `json:"cors_error,omitempty"`
}

// TestStorage 用页面上填的配置（未保存）测试读写，并检查 CORS 是否允许 origin 直传。
func (s *Server) TestStorage(ctx context.Context, in StorageView, origin string) (StorageTest, error) {
	st, err := s.mergeStorage(ctx, in)
	if err != nil {
		return StorageTest{}, err
	}
	o, err := objstore.NewOSS(st.OSS)
	if err != nil {
		return StorageTest{}, &InputError{err.Error()}
	}
	if err := o.Test(ctx); err != nil {
		return StorageTest{Error: err.Error()}, nil
	}
	res := StorageTest{OK: true}
	if origin != "" {
		res.CORSReady, err = o.CORSReady(ctx, origin)
		if err != nil {
			res.CORSError = err.Error()
		}
	}
	return res, nil
}

// FixCORS 给已保存的 OSS 桶加一条允许 origin 直传的 CORS 规则。
func (s *Server) FixCORS(ctx context.Context, origin string) error {
	o, _, err := s.ossStore(ctx)
	if err != nil {
		return err
	}
	if o == nil {
		return &InputError{"请先保存 OSS 配置"}
	}
	if origin == "" {
		return &InputError{"缺少页面地址"}
	}
	if err := o.EnsureCORS(ctx, origin); err != nil {
		return &InputError{err.Error()}
	}
	return nil
}

// MigrateResult 是迁移到 OSS 的结果。
type MigrateResult struct {
	Releases int      `json:"releases"`
	Files    int      `json:"files"`
	Errors   []string `json:"errors"`
}

// MigrateToOSS 把存在本机的更新包和云盘文件上传到 OSS，成功后删除本机文件。逐个进行，失败的留在本机。
func (s *Server) MigrateToOSS(ctx context.Context) (MigrateResult, error) {
	res := MigrateResult{Errors: []string{}}
	o, enabled, err := s.ossStore(ctx)
	if err != nil {
		return res, err
	}
	if o == nil || !enabled {
		return res, &InputError{"请先配置并启用 OSS"}
	}
	local := s.localStore()
	move := func(localKey, ossKey string) error {
		rc, size, err := local.Open(ctx, localKey)
		if err != nil {
			return err
		}
		defer rc.Close()
		return o.Put(ctx, ossKey, rc, size)
	}
	rels, err := s.Releases(ctx)
	if err != nil {
		return res, err
	}
	for _, r := range rels {
		if r.Storage != objstore.KindLocal {
			continue
		}
		lk := s.releaseKey(r)
		ok := o.Key("releases/" + r.Target + "/" + r.Version + "/" + r.FileName)
		if err := move(lk, ok); err != nil {
			res.Errors = append(res.Errors, fmt.Sprintf("更新包 %s %s：%v", r.Target, r.Version, err))
			continue
		}
		if err := s.execOne(ctx, `UPDATE releases SET storage = 'oss', object_key = ? WHERE id = ?`, ok, r.ID); err != nil {
			return res, err
		}
		_ = local.Delete(ctx, lk)
		res.Releases++
	}
	files, err := s.Files(ctx, false)
	if err != nil {
		return res, err
	}
	for _, f := range files {
		if f.Storage != objstore.KindLocal {
			continue
		}
		ok := o.Key(f.ObjectKey)
		if err := move(f.ObjectKey, ok); err != nil {
			res.Errors = append(res.Errors, fmt.Sprintf("文件 %s：%v", f.Name, err))
			continue
		}
		if err := s.execOne(ctx, `UPDATE files SET storage = 'oss', object_key = ? WHERE id = ?`, ok, f.ID); err != nil {
			return res, err
		}
		_ = local.Delete(ctx, f.ObjectKey)
		res.Files++
	}
	return res, nil
}

// requestOrigin 是管理页面的来源（https://域名[:端口]），给 OSS 的 CORS 规则用。
func requestOrigin(r *http.Request) string {
	if o := r.Header.Get("Origin"); o != "" && o != "null" {
		return o
	}
	scheme := "https"
	if r.TLS == nil && r.Header.Get("X-Forwarded-Proto") != "https" {
		scheme = "http"
	}
	return scheme + "://" + r.Host
}

func (s *Server) storageAdminRoutes(mux *http.ServeMux, authed func(http.HandlerFunc) http.Handler, log *slog.Logger) {
	fail := func(w http.ResponseWriter, err error) { writeErr(w, err, log) }
	mux.Handle("GET /api/v1/dist/storage", authed(func(w http.ResponseWriter, r *http.Request) {
		v, err := s.Storage(r.Context())
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, http.StatusOK, v)
	}))
	mux.Handle("PUT /api/v1/dist/storage", authed(func(w http.ResponseWriter, r *http.Request) {
		var in StorageView
		if !decode(w, r, &in) {
			return
		}
		if err := s.SaveStorage(r.Context(), in); err != nil {
			fail(w, err)
			return
		}
		v, err := s.Storage(r.Context())
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, http.StatusOK, v)
	}))
	mux.Handle("POST /api/v1/dist/storage/test", authed(func(w http.ResponseWriter, r *http.Request) {
		var in StorageView
		if !decode(w, r, &in) {
			return
		}
		res, err := s.TestStorage(r.Context(), in, requestOrigin(r))
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, http.StatusOK, res)
	}))
	mux.Handle("POST /api/v1/dist/storage/cors", authed(func(w http.ResponseWriter, r *http.Request) {
		if err := s.FixCORS(r.Context(), requestOrigin(r)); err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	}))
	mux.Handle("POST /api/v1/dist/storage/migrate", authed(func(w http.ResponseWriter, r *http.Request) {
		res, err := s.MigrateToOSS(r.Context())
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, http.StatusOK, res)
	}))
}
