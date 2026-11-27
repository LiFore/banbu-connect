package objstore

import (
	"context"
	"crypto/hmac"
	"crypto/md5"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// 本机存储的签名地址（相对地址，指向管理服务自己）。
const (
	LocalGetPath  = "/api/v1/blob"
	LocalPartPath = "/api/v1/blob/part"
)

// Local 把对象存在本机目录 Root 下；分片上传的临时文件在 Root/.uploads/<上传 ID>/。
type Local struct {
	Root string
	key  []byte // 签名密钥，进程内随机生成
}

// NewLocal 创建本机存储。
func NewLocal(root string) *Local {
	k := make([]byte, 32)
	_, _ = rand.Read(k)
	return &Local{Root: root, key: k}
}

func (l *Local) Kind() string { return KindLocal }

func (l *Local) path(key string) (string, error) {
	k, err := CleanKey(key)
	if err != nil {
		return "", err
	}
	return filepath.Join(l.Root, filepath.FromSlash(k)), nil
}

func (l *Local) Put(_ context.Context, key string, r io.Reader, size int64) error {
	p, err := l.path(key)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(p), ".put-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	n, err := io.Copy(tmp, r)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	if size >= 0 && n != size {
		return fmt.Errorf("写入 %d 字节，应为 %d", n, size)
	}
	return os.Rename(tmp.Name(), p)
}

func (l *Local) Open(_ context.Context, key string) (io.ReadCloser, int64, error) {
	p, err := l.path(key)
	if err != nil {
		return nil, 0, err
	}
	f, err := os.Open(p)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, 0, ErrNotFound
	}
	if err != nil {
		return nil, 0, err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, 0, err
	}
	return f, st.Size(), nil
}

func (l *Local) Stat(_ context.Context, key string) (int64, error) {
	p, err := l.path(key)
	if err != nil {
		return 0, err
	}
	st, err := os.Stat(p)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, ErrNotFound
	}
	if err != nil {
		return 0, err
	}
	return st.Size(), nil
}

func (l *Local) Delete(_ context.Context, key string) error {
	p, err := l.path(key)
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	// 顺手删掉空目录，到根目录为止。
	for d := filepath.Dir(p); d != l.Root && strings.HasPrefix(d, l.Root); d = filepath.Dir(d) {
		if os.Remove(d) != nil {
			break
		}
	}
	return nil
}

func (l *Local) sign(fields ...string) string {
	m := hmac.New(sha256.New, l.key)
	m.Write([]byte(strings.Join(fields, "\n")))
	return hex.EncodeToString(m.Sum(nil))
}

func (l *Local) SignGet(_ context.Context, key, fileName string, ttl time.Duration) (SignedURL, error) {
	k, err := CleanKey(key)
	if err != nil {
		return SignedURL{}, err
	}
	exp := strconv.FormatInt(time.Now().Add(ttl).Unix(), 10)
	q := url.Values{"k": {k}, "n": {fileName}, "e": {exp}, "s": {l.sign("GET", k, fileName, exp)}}
	return SignedURL{URL: LocalGetPath + "?" + q.Encode()}, nil
}

var uploadIDRE = regexp.MustCompile(`^[0-9a-f]{32}$`)

func (l *Local) uploadDir(uploadID string) (string, error) {
	if !uploadIDRE.MatchString(uploadID) {
		return "", errors.New("上传 ID 无效")
	}
	return filepath.Join(l.Root, ".uploads", uploadID), nil
}

func (l *Local) CreateUpload(_ context.Context, key string) (string, error) {
	if _, err := CleanKey(key); err != nil {
		return "", err
	}
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	id := hex.EncodeToString(b)
	dir, _ := l.uploadDir(id)
	return id, os.MkdirAll(dir, 0o700)
}

func (l *Local) SignPart(_ context.Context, key, uploadID string, number int, ttl time.Duration) (SignedURL, error) {
	k, err := CleanKey(key)
	if err != nil {
		return SignedURL{}, err
	}
	exp := strconv.FormatInt(time.Now().Add(ttl).Unix(), 10)
	n := strconv.Itoa(number)
	q := url.Values{"k": {k}, "u": {uploadID}, "p": {n}, "e": {exp}, "s": {l.sign("PUT", k, uploadID, n, exp)}}
	return SignedURL{URL: LocalPartPath + "?" + q.Encode()}, nil
}

func (l *Local) CompleteUpload(ctx context.Context, key, uploadID string, parts []Part) error {
	dir, err := l.uploadDir(uploadID)
	if err != nil {
		return err
	}
	readers := []io.Reader{}
	closers := []io.Closer{}
	defer func() {
		for _, c := range closers {
			c.Close()
		}
	}()
	for i, p := range parts {
		if p.Number != i+1 {
			return fmt.Errorf("分片不连续：第 %d 片编号为 %d", i+1, p.Number)
		}
		f, err := os.Open(filepath.Join(dir, strconv.Itoa(p.Number)))
		if err != nil {
			return fmt.Errorf("第 %d 片没有上传", p.Number)
		}
		closers = append(closers, f)
		h := md5.New()
		if _, err := io.Copy(h, f); err != nil {
			return err
		}
		if strings.Trim(p.ETag, `"`) != hex.EncodeToString(h.Sum(nil)) {
			return fmt.Errorf("第 %d 片校验不一致", p.Number)
		}
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			return err
		}
		readers = append(readers, f)
	}
	if err := l.Put(ctx, key, io.MultiReader(readers...), -1); err != nil {
		return err
	}
	return os.RemoveAll(dir)
}

func (l *Local) AbortUpload(_ context.Context, _ string, uploadID string) error {
	dir, err := l.uploadDir(uploadID)
	if err != nil {
		return err
	}
	return os.RemoveAll(dir)
}

// PruneUploads 删除超过 maxAge 的未完成上传。
func (l *Local) PruneUploads(maxAge time.Duration) {
	entries, err := os.ReadDir(filepath.Join(l.Root, ".uploads"))
	if err != nil {
		return
	}
	for _, e := range entries {
		if info, err := e.Info(); err == nil && time.Since(info.ModTime()) > maxAge {
			_ = os.RemoveAll(filepath.Join(l.Root, ".uploads", e.Name()))
		}
	}
}

// Routes 注册签名地址的处理：下载（GET，支持断点续传）和分片上传（PUT）。不需要登录，签名即授权。
func (l *Local) Routes(mux *http.ServeMux) {
	expired := func(e string) bool {
		t, err := strconv.ParseInt(e, 10, 64)
		return err != nil || time.Now().Unix() > t
	}
	mux.HandleFunc("GET "+LocalGetPath, func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		k, n, e := q.Get("k"), q.Get("n"), q.Get("e")
		if expired(e) || !hmac.Equal([]byte(q.Get("s")), []byte(l.sign("GET", k, n, e))) {
			http.Error(w, "下载地址无效或已过期", http.StatusForbidden)
			return
		}
		p, err := l.path(k)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		f, err := os.Open(p)
		if err != nil {
			http.Error(w, "文件不存在", http.StatusNotFound)
			return
		}
		defer f.Close()
		st, err := f.Stat()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		if n != "" {
			w.Header().Set("Content-Disposition", ContentDisposition(n))
		}
		http.ServeContent(w, r, "", st.ModTime(), f)
	})
	mux.HandleFunc("PUT "+LocalPartPath, func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		k, u, p, e := q.Get("k"), q.Get("u"), q.Get("p"), q.Get("e")
		if expired(e) || !hmac.Equal([]byte(q.Get("s")), []byte(l.sign("PUT", k, u, p, e))) {
			http.Error(w, "上传地址无效或已过期", http.StatusForbidden)
			return
		}
		n, err := strconv.Atoi(p)
		dir, derr := l.uploadDir(u)
		if err != nil || n < 1 || n > 10000 || derr != nil {
			http.Error(w, "参数无效", http.StatusBadRequest)
			return
		}
		if _, err := os.Stat(dir); err != nil {
			http.Error(w, "上传已取消或不存在", http.StatusNotFound)
			return
		}
		tmp, err := os.CreateTemp(dir, ".part-*")
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		defer os.Remove(tmp.Name())
		h := md5.New()
		_, err = io.Copy(io.MultiWriter(tmp, h), http.MaxBytesReader(w, r.Body, 5<<30))
		if cerr := tmp.Close(); err == nil {
			err = cerr
		}
		if err == nil {
			err = os.Rename(tmp.Name(), filepath.Join(dir, p))
		}
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.Header().Set("ETag", `"`+hex.EncodeToString(h.Sum(nil))+`"`)
		w.WriteHeader(http.StatusOK)
	})
}
