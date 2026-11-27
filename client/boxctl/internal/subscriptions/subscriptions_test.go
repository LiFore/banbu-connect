package subscriptions

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"boxctl/internal/secret"
	"boxctl/internal/store"
	"boxctl/internal/sub"
)

type fixture struct {
	svc  *Service
	db   *sql.DB
	body []byte // 下一次“拉取”返回的内容
	req  sub.FetchRequest
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	db, err := store.Open(ctx, filepath.Join(dir, "boxctl.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	key, _ := secret.LoadOrCreateKey(filepath.Join(dir, "device.key"))
	box, _ := secret.New(key)

	f := &fixture{db: db}
	f.svc = New(db, box, "http://127.0.0.1:7890")
	f.svc.Fetch = func(_ context.Context, r sub.FetchRequest) (sub.FetchResult, error) {
		f.req = r
		return sub.FetchResult{Body: f.body, UserInfo: &sub.UserInfo{Total: 100, Download: 10}}, nil
	}
	sample, err := os.ReadFile("../sub/testdata/sample_sub.json")
	if err != nil {
		t.Fatal(err)
	}
	f.body = sample
	return f
}

func (f *fixture) create(t *testing.T, name string) Subscription {
	t.Helper()
	sb, err := f.svc.Create(context.Background(), Input{Name: name, URL: "https://sub.example.com/api?token=SECRET-TOKEN"})
	if err != nil {
		t.Fatal(err)
	}
	return sb
}

func (f *fixture) refresh(t *testing.T, id string, confirm bool) (Preview, Diff, error) {
	t.Helper()
	p, err := f.svc.Preview(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	d, err := f.svc.Apply(context.Background(), id, p.ID, confirm)
	return p, d, err
}

func TestCRUDAndURLMasking(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	sb := f.create(t, "jsyun")
	if sb.URL != "" || !sb.HasURL || sb.FetchVia != FetchDirect || sb.UserAgent != sub.DefaultUserAgent || !sb.Enabled {
		t.Fatalf("created %+v", sb)
	}
	if _, err := f.svc.Create(ctx, Input{Name: "jsyun", URL: "https://x.example.com"}); !errors.Is(err, ErrNameExists) {
		t.Fatalf("dup name: %v", err)
	}
	var ve *ValidationError
	for _, in := range []Input{
		{Name: "bad name!", URL: "https://x.example.com"},
		{Name: "ok", URL: "ftp://x.example.com"},
		{Name: "ok", URL: "https://x.example.com", FetchVia: "tunnel"},
		{Name: "ok", URL: "https://x.example.com", Options: &sub.Options{NameTemplate: "{region}", TimeoutSec: 30}},
	} {
		if _, err := f.svc.Create(ctx, in); !errors.As(err, &ve) {
			t.Errorf("%+v: %v", in, err)
		}
	}

	// URL 加密存储：库文件中查不到明文（M1-08）
	var ct []byte
	_ = f.db.QueryRow(`SELECT url_secret FROM subscriptions WHERE id = ?`, sb.ID).Scan(&ct)
	if bytes.Contains(ct, []byte("SECRET-TOKEN")) {
		t.Fatal("url stored in plaintext")
	}
	got, _ := f.svc.Get(ctx, sb.ID, true)
	if got.URL != "https://sub.example.com/api?token=SECRET-TOKEN" {
		t.Fatalf("reveal: %q", got.URL)
	}

	// 修改时 URL 留空则保留
	off := false
	upd, err := f.svc.Update(ctx, sb.ID, Input{Name: "jsyun2", DisplayName: "极速云", FetchVia: FetchProxy, Enabled: &off})
	if err != nil || upd.Name != "jsyun2" || upd.Enabled || upd.FetchVia != FetchProxy {
		t.Fatalf("update %+v %v", upd, err)
	}
	got, _ = f.svc.Get(ctx, sb.ID, true)
	if got.URL == "" {
		t.Fatal("url lost on update")
	}

	list, _ := f.svc.List(ctx)
	if len(list) != 1 || list[0].URL != "" {
		t.Fatalf("list %+v", list)
	}
	if err := f.svc.Delete(ctx, sb.ID, false); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Get(ctx, sb.ID, false); !errors.Is(err, ErrNotFound) {
		t.Fatalf("after delete: %v", err)
	}
}

func TestPreviewApplySample(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	sb := f.create(t, "jsyun")

	p, d, err := f.refresh(t, sb.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	if f.req.ProxyURL != "" || f.req.UserAgent != sub.DefaultUserAgent {
		t.Fatalf("fetch req %+v", f.req)
	}
	if p.Report.Kept != 107 || len(d.Added) != 107 || len(d.Removed) != 0 {
		t.Fatalf("kept=%d added=%d", p.Report.Kept, len(d.Added))
	}
	got, _ := f.svc.Get(ctx, sb.ID, false)
	if got.NodeCount != 107 || got.UserInfo == nil || got.UserInfo.Total != 100 || got.LastFetchAt == "" {
		t.Fatalf("after apply %+v", got)
	}

	// 节点凭据加密存储
	var secretCT []byte
	_ = f.db.QueryRow(`SELECT secret FROM proxy_nodes LIMIT 1`).Scan(&secretCT)
	var specCount int
	_ = f.db.QueryRow(`SELECT COUNT(*) FROM proxy_nodes WHERE spec LIKE '%fake-password%'`).Scan(&specCount)
	if bytes.Contains(secretCT, []byte("fake-password")) || specCount != 0 {
		t.Fatal("node secret stored in plaintext")
	}

	// 同内容再次更新：无变化
	_, d, err = f.refresh(t, sb.ID, false)
	if err != nil || d.Unchanged != 107 || len(d.Added)+len(d.Removed)+len(d.Changed) != 0 {
		t.Fatalf("second refresh %+v %v", d, err)
	}

	// 经代理拉取
	if _, err := f.svc.Update(ctx, sb.ID, Input{Name: "jsyun", FetchVia: FetchProxy}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Preview(ctx, sb.ID); err != nil || f.req.ProxyURL != "http://127.0.0.1:7890" {
		t.Fatalf("proxy fetch: %v %q", err, f.req.ProxyURL)
	}
}

// M2-08：机场只改节点名时 ID 不变，组的引用不受影响。
func TestRenameKeepsIDs(t *testing.T) {
	f := newFixture(t)
	sb := f.create(t, "jsyun")
	if _, _, err := f.refresh(t, sb.ID, false); err != nil {
		t.Fatal(err)
	}
	var before []string
	rows, _ := f.db.Query(`SELECT id FROM proxy_nodes ORDER BY id`)
	for rows.Next() {
		var id string
		_ = rows.Scan(&id)
		before = append(before, id)
	}
	rows.Close()

	f.body = bytes.ReplaceAll(f.body, []byte("_0"), []byte(" 节点")) // 改名：香港_01 → 香港 节点1
	_, d, err := f.refresh(t, sb.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Added) != 0 || len(d.Removed) != 0 {
		t.Fatalf("rename caused add/remove: +%d -%d", len(d.Added), len(d.Removed))
	}
	var after []string
	rows, _ = f.db.Query(`SELECT id FROM proxy_nodes ORDER BY id`)
	for rows.Next() {
		var id string
		_ = rows.Scan(&id)
		after = append(after, id)
	}
	rows.Close()
	if strings.Join(before, ",") != strings.Join(after, ",") {
		t.Fatal("ids changed")
	}
}

// M2-10：被删除节点若被代理组显式引用，需确认后才应用。
func TestRemovalImpactsRequireConfirm(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	sb := f.create(t, "jsyun")
	if _, _, err := f.refresh(t, sb.ID, false); err != nil {
		t.Fatal(err)
	}
	var victim string
	_ = f.db.QueryRow(`SELECT id FROM proxy_nodes WHERE display_name = '香港 - 01 - jsyun'`).Scan(&victim)
	if _, err := f.db.Exec(`INSERT INTO proxy_groups (id, name, type, members) VALUES ('g1', 'HK 手动', 'select', ?)`,
		`{"explicit":["`+victim+`","DIRECT"]}`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.Exec(`UPDATE proxy_nodes SET enabled = 0, tags = '["keep"]' WHERE id != ?`, victim); err != nil {
		t.Fatal(err)
	}

	// 新订阅内容去掉了第一个香港节点（🇭🇰 香港1）
	f.body = bytes.Replace(f.body, []byte(`"🇭🇰 香港1"`), []byte(`"剩余流量 被过滤"`), 1)
	p, err := f.svc.Preview(ctx, sb.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Impacts) != 1 || p.Impacts[0].RefName != "HK 手动" || p.Impacts[0].Kind != "group" {
		t.Fatalf("impacts %+v", p.Impacts)
	}
	_, err = f.svc.Apply(ctx, sb.ID, p.ID, false)
	var re *ReferencesError
	if !errors.As(err, &re) {
		t.Fatalf("want ReferencesError, got %v", err)
	}
	d, err := f.svc.Apply(ctx, sb.ID, p.ID, true)
	if err != nil || len(d.Removed) != 1 || d.Removed[0].ID != victim {
		t.Fatalf("confirm apply %+v %v", d, err)
	}
	// 用户设置的启用状态与标签在更新后保留
	var disabled int
	_ = f.db.QueryRow(`SELECT COUNT(*) FROM proxy_nodes WHERE enabled = 0 AND tags = '["keep"]'`).Scan(&disabled)
	if disabled != 106 {
		t.Fatalf("user fields not preserved: %d", disabled)
	}
	// 预览已用完
	if _, err := f.svc.Apply(ctx, sb.ID, p.ID, true); !errors.Is(err, ErrPreviewExpired) {
		t.Fatalf("reuse preview: %v", err)
	}
}

// 跨订阅重复：第二个订阅中与第一个相同的节点被跳过并标注来源。
func TestCrossSubscriptionDuplicates(t *testing.T) {
	f := newFixture(t)
	a := f.create(t, "a")
	b := f.create(t, "b")
	if _, _, err := f.refresh(t, a.ID, false); err != nil {
		t.Fatal(err)
	}
	p, d, err := f.refresh(t, b.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Conflicts) != 107 || p.Conflicts[0].OwnerSubscription != "a" || len(d.Added) != 0 {
		t.Fatalf("conflicts=%d added=%d", len(p.Conflicts), len(d.Added))
	}
}

func TestFetchErrorRecorded(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	sb := f.create(t, "x")
	f.body = []byte("<html>请登录</html>")
	if _, err := f.svc.Preview(ctx, sb.ID); err == nil {
		t.Fatal("want parse error")
	}
	got, _ := f.svc.Get(ctx, sb.ID, false)
	if got.LastError == "" || strings.Contains(got.LastError, "SECRET") {
		t.Fatalf("last_error %q", got.LastError)
	}
}

func TestDeleteWithReferences(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	sb := f.create(t, "jsyun")
	if _, _, err := f.refresh(t, sb.ID, false); err != nil {
		t.Fatal(err)
	}
	var nid string
	_ = f.db.QueryRow(`SELECT id FROM proxy_nodes LIMIT 1`).Scan(&nid)
	_, _ = f.db.Exec(`INSERT INTO rules (id, section, spec, target, note) VALUES ('r1', 'custom', '{}', ?, '测试规则')`, nid)

	var re *ReferencesError
	if err := f.svc.Delete(ctx, sb.ID, false); !errors.As(err, &re) || re.Impacts[0].Kind != "rule" {
		t.Fatalf("want rule impact, got %v", err)
	}
	if err := f.svc.Delete(ctx, sb.ID, true); err != nil {
		t.Fatal(err)
	}
	var n int
	_ = f.db.QueryRow(`SELECT COUNT(*) FROM proxy_nodes`).Scan(&n)
	if n != 0 {
		t.Fatalf("nodes left: %d", n)
	}
}
