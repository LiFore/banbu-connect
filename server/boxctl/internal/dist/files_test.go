package dist

import (
	"bytes"
	"context"
	"crypto/tls"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"boxctl/internal/objstore"
)

// uploadVia 按上传计划把 data 分片 PUT 到签名地址（模拟浏览器直传），返回各片的 ETag。
func uploadVia(t *testing.T, base string, plan UploadPlan, data []byte) []objstore.Part {
	t.Helper()
	hc := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}}
	parts := []objstore.Part{}
	for i, p := range plan.Parts {
		start := int64(i) * plan.PartSize
		end := min(start+plan.PartSize, int64(len(data)))
		req, _ := http.NewRequest(http.MethodPut, base+p.URL, bytes.NewReader(data[start:end]))
		resp, err := hc.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("第 %d 片上传失败：%d", i+1, resp.StatusCode)
		}
		parts = append(parts, objstore.Part{Number: i + 1, ETag: resp.Header.Get("ETag")})
	}
	return parts
}

func TestFilesLocalUploadAndClientDownload(t *testing.T) {
	ctx := context.Background()
	s := newServer(t)
	_, token, _ := s.CreateDevice(ctx)
	base, fp := tlsServer(t, s)
	c, _ := NewClient(t.TempDir(), "dev", nil)
	seed(t, s)
	if _, err := s.Publish(ctx, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Bind(ctx, Binding{URL: base, Token: token, Fingerprint: fp}); err != nil {
		t.Fatal(err)
	}

	// 超过一片（16 MB）的文件，验证分片合并。
	data := bytes.Repeat([]byte("0123456789abcdef"), (17<<20)/16)
	plan, err := s.StartUpload(ctx, "../安装 指南.zip", int64(len(data)), 0)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Storage != objstore.KindLocal || len(plan.Parts) != 2 {
		t.Fatalf("应为本机存储、2 片：%+v", plan)
	}
	parts := uploadVia(t, base, plan, data)
	if _, err := s.CompleteUpload(ctx, plan.ID, UploadDone{Parts: parts[:1], Published: true}); err == nil {
		t.Fatal("分片数量不对应失败")
	}
	// 失败后上传作废，要重新开始。
	plan, _ = s.StartUpload(ctx, "../安装 指南.zip", int64(len(data)), 0)
	parts = uploadVia(t, base, plan, data)
	big, err := s.CompleteUpload(ctx, plan.ID, UploadDone{Parts: parts, Description: "**先读**", Published: true})
	if err != nil {
		t.Fatal(err)
	}
	if big.FileName != "安装 指南.zip" || big.Name != "安装 指南.zip" || big.Size != int64(len(data)) {
		t.Fatalf("文件信息不对：%+v", big)
	}

	md := []byte("# 说明\n\n内容")
	plan, _ = s.StartUpload(ctx, "readme.md", int64(len(md)), 0)
	doc, err := s.CompleteUpload(ctx, plan.ID, UploadDone{Parts: uploadVia(t, base, plan, md), Name: "使用说明", Published: true})
	if err != nil {
		t.Fatal(err)
	}
	plan, _ = s.StartUpload(ctx, "draft.txt", 3, 0)
	draft, err := s.CompleteUpload(ctx, plan.ID, UploadDone{Parts: uploadVia(t, base, plan, []byte("abc"))})
	if err != nil {
		t.Fatal(err)
	}

	// 客户端只看得到已发布的；新上传的排在前面。
	list, err := c.Files(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || list[0].ID != doc.ID || !list[0].Markdown || list[1].ID != big.ID {
		t.Fatalf("客户端列表不对：%+v", list)
	}
	// 置顶后排到最前，重排后按新顺序。
	pinned := true
	if _, err := s.UpdateFile(ctx, big.ID, FileEdit{Pinned: &pinned}); err != nil {
		t.Fatal(err)
	}
	pub := true
	_, _ = s.UpdateFile(ctx, draft.ID, FileEdit{Published: &pub})
	if err := s.ReorderFiles(ctx, []int64{draft.ID, doc.ID}); err != nil {
		t.Fatal(err)
	}
	list, _ = c.Files(ctx)
	if got := []int64{list[0].ID, list[1].ID, list[2].ID}; got[0] != big.ID || got[1] != draft.ID || got[2] != doc.ID {
		t.Fatalf("排序不对：%v", got)
	}

	// 下载（跳转到本机存储的签名地址）和阅读 Markdown。
	resp, err := c.OpenBlob(ctx, "/api/v1/client/files/"+itoa(big.ID)+"/download")
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !bytes.Equal(got, data) {
		t.Fatalf("下载内容不一致：%d 字节", len(got))
	}
	if !strings.Contains(resp.Header.Get("Content-Disposition"), "filename*=UTF-8''%E5%AE%89%E8%A3%85") {
		t.Fatalf("下载文件名不对：%q", resp.Header.Get("Content-Disposition"))
	}
	if text, err := c.FileContent(ctx, doc.ID); err != nil || text != string(md) {
		t.Fatalf("Markdown 内容不对：%q %v", text, err)
	}
	if _, err := c.FileContent(ctx, big.ID); err == nil {
		t.Fatal("非 Markdown 文件不能直接阅读")
	}

	// 替换文件内容：保留标题和说明。
	plan, _ = s.StartUpload(ctx, "readme-v2.md", 2, doc.ID)
	doc2, err := s.CompleteUpload(ctx, plan.ID, UploadDone{Parts: uploadVia(t, base, plan, []byte("v2"))})
	if err != nil {
		t.Fatal(err)
	}
	if doc2.ID != doc.ID || doc2.Name != "使用说明" || doc2.FileName != "readme-v2.md" || doc2.Size != 2 {
		t.Fatalf("替换后信息不对：%+v", doc2)
	}
	if _, err := s.localStore().Stat(ctx, doc.ObjectKey); err == nil {
		t.Fatal("替换后旧文件应删除")
	}

	// 下架、删除后客户端不能下载。
	unpub := false
	_, _ = s.UpdateFile(ctx, draft.ID, FileEdit{Published: &unpub})
	if _, err := c.OpenBlob(ctx, "/api/v1/client/files/"+itoa(draft.ID)+"/download"); err == nil {
		t.Fatal("下架后不能下载")
	}
	if err := s.DeleteFile(ctx, big.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.localStore().Stat(ctx, big.ObjectKey); err == nil {
		t.Fatal("删除后存储里的文件也应删除")
	}
}

func TestOldClientGetsReleaseStreamed(t *testing.T) {
	// 旧版客户端不带 redirect=1，管理服务直接转发文件内容，不回跳转。
	ctx := context.Background()
	s := newServer(t)
	_, token, _ := s.CreateDevice(ctx)
	r, err := s.AddRelease(ctx, "0.3.0", "darwin-aarch64", "", "a.tar.gz", fakeSig("0.3.0", "a.tar.gz"), strings.NewReader("pkg"))
	if err != nil {
		t.Fatal(err)
	}
	_ = s.SetReleasePublished(ctx, r.ID, true)
	mux := http.NewServeMux()
	s.ClientRoutes(mux, slog.New(slog.DiscardHandler))
	for _, q := range []string{"", "?redirect=1"} {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/client/releases/"+itoa(r.ID)+"/download"+q, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if q == "" && (rec.Code != http.StatusOK || rec.Body.String() != "pkg") {
			t.Fatalf("旧版客户端应直接拿到内容：%d %q", rec.Code, rec.Body.String())
		}
		if q != "" && (rec.Code != http.StatusFound || !strings.HasPrefix(rec.Header().Get("Location"), objstore.LocalGetPath+"?")) {
			t.Fatalf("新版客户端应拿到跳转：%d %q", rec.Code, rec.Header().Get("Location"))
		}
	}
}

func TestStorageConfigKeepsSecret(t *testing.T) {
	ctx := context.Background()
	s := newServer(t)
	if err := s.SaveStorage(ctx, StorageView{Region: "oss-cn-hangzhou", Bucket: "b", AccessKeyID: "id", AccessKeySecret: "sec", Prefix: "/banbu/"}); err != nil {
		t.Fatal(err)
	}
	v, _ := s.Storage(ctx)
	if !v.HasSecret || v.Region != "cn-hangzhou" || v.Prefix != "banbu" || v.Active != objstore.KindLocal || v.AccessKeySecret != "" {
		t.Fatalf("配置不对：%+v", v)
	}
	var raw storageSetting
	_ = s.loadSetting(ctx, storageKey, &raw)
	if raw.OSS.AccessKeySecret != "" || raw.SecretSealed == "" {
		t.Fatal("Secret 应加密保存")
	}
	// Secret 留空表示不改。
	if err := s.SaveStorage(ctx, StorageView{Region: "cn-hangzhou", Bucket: "b2", AccessKeyID: "id"}); err != nil {
		t.Fatal(err)
	}
	st, _ := s.loadStorage(ctx)
	if st.OSS.AccessKeySecret != "sec" || st.OSS.Bucket != "b2" || st.OSS.Prefix != "banbu/" && st.OSS.Prefix != "" {
		t.Fatalf("Secret 应保留：%+v", st.OSS)
	}
	// 未启用时新文件仍存本机。
	if store, _, err := s.activeStore(ctx); err != nil || store.Kind() != objstore.KindLocal {
		t.Fatalf("未启用 OSS 时应存本机：%v", err)
	}
	if err := s.SaveStorage(ctx, StorageView{Enabled: true}); err == nil {
		t.Fatal("配置不完整时不能启用")
	}
}

// 浏览器把分片直传到 OSS：管理页面的 CSP connect-src 要放行签名地址的来源，否则 XHR 只报网络错误。
func TestConnectSourcesFollowStorage(t *testing.T) {
	ctx := context.Background()
	s := newServer(t)
	if got := s.ConnectSources(); !slices.Equal(got, []string{ossDefaultOrigin}) {
		t.Fatalf("未配置 OSS 时也放行标准域名：%v", got)
	}
	if err := s.SaveStorage(ctx, StorageView{Region: "cn-hangzhou", Bucket: "banbu-files", AccessKeyID: "id", AccessKeySecret: "sec"}); err != nil {
		t.Fatal(err)
	}
	if got := s.ConnectSources(); !slices.Equal(got, []string{ossDefaultOrigin}) {
		t.Fatalf("标准 Endpoint 已被通配覆盖：%v", got)
	}
	// 保存后缓存失效，自定义 Endpoint 的来源单独放行。
	if err := s.SaveStorage(ctx, StorageView{Region: "cn-hangzhou", Endpoint: "http://files.example.com", Bucket: "banbu-files", AccessKeyID: "id"}); err != nil {
		t.Fatal(err)
	}
	got := s.ConnectSources()
	if len(got) != 2 || got[0] != ossDefaultOrigin || !strings.HasPrefix(got[1], "http://") || !strings.HasSuffix(got[1], "files.example.com") {
		t.Fatalf("自定义 Endpoint 应放行：%v", got)
	}
}

func itoa(n int64) string {
	b := []byte{}
	if n == 0 {
		return "0"
	}
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
