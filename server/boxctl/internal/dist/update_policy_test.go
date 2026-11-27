package dist

import (
	"context"
	"strings"
	"testing"
)

func TestUpdatePolicy(t *testing.T) {
	ctx := context.Background()
	s := newServer(t)
	d, token, _ := s.CreateDevice(ctx)
	r1, _ := s.AddRelease(ctx, "0.3.0", "darwin-aarch64", "", "a.tar.gz", fakeSig("0.3.0", "a.tar.gz"), strings.NewReader("a"))
	r2, _ := s.AddRelease(ctx, "0.4.0", "darwin-aarch64", "", "b.tar.gz", fakeSig("0.4.0", "b.tar.gz"), strings.NewReader("b"))
	_ = s.SetReleasePublished(ctx, r1.ID, true)
	_ = s.SetReleasePublished(ctx, r2.ID, true)

	if err := s.SetReleasePolicy(ctx, r2.ID, ReleasePolicy{Mode: "auto"}); err == nil {
		t.Fatal("unknown mode should fail")
	}
	if err := s.SetReleasePolicy(ctx, r1.ID, ReleasePolicy{Mode: UpdatePush, MinVersion: "0.4.0"}); err == nil {
		t.Fatal("min version above the release version should fail")
	}
	// 0.3.0 要求最低 0.3.0；最新的 0.4.0 是静默更新。
	if err := s.SetReleasePolicy(ctx, r1.ID, ReleasePolicy{Mode: UpdateNormal, MinVersion: "v0.3.0"}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetReleasePolicy(ctx, r2.ID, ReleasePolicy{Mode: UpdateSilent}); err != nil {
		t.Fatal(err)
	}

	u, fp := tlsServer(t, s)
	seed(t, s)
	if _, err := s.Publish(ctx, ""); err != nil {
		t.Fatal(err)
	}
	c, _ := NewClient(t.TempDir(), "dev", nil)
	if _, err := c.Bind(ctx, Binding{URL: u, Token: token, Fingerprint: fp}); err != nil {
		t.Fatal(err)
	}
	// 低于最低版本：强制。
	info, err := c.CheckUpdate(ctx, "darwin", "aarch64", "0.2.9")
	if err != nil || info == nil || info.Version != "0.4.0" || info.Mode != UpdateForce || info.MinVersion != "0.3.0" {
		t.Fatalf("below min = %+v %v", info, err)
	}
	// 不低于最低版本：按最新包的方式。
	if info, _ = c.CheckUpdate(ctx, "darwin", "aarch64", "0.3.0"); info == nil || info.Mode != UpdateSilent {
		t.Fatalf("at min = %+v", info)
	}

	// 进度上报与管理页面查看。
	if err := c.ReportUpdateProgress(ctx, UpdateProgressReport{Version: "0.4.0", Mode: UpdateSilent, Phase: "bogus"}); err == nil {
		t.Fatal("unknown phase should fail")
	}
	if err := c.ReportUpdateProgress(ctx, UpdateProgressReport{Version: "0.4.0", Mode: UpdateSilent, Phase: "failed", Error: "签名校验失败"}); err != nil {
		t.Fatal(err)
	}
	list, err := s.DeviceUpdates(ctx)
	if err != nil || len(list) != 1 || list[0].DeviceID != d.ID || list[0].Phase != "failed" || list[0].Error != "签名校验失败" || list[0].Mode != UpdateSilent {
		t.Fatalf("device updates = %+v %v", list, err)
	}
}

// Lite 版（lite- 前缀）和完整版的更新包互不干扰：各自只拿到自己平台的最新版本。
func TestLiteReleasesSeparate(t *testing.T) {
	ctx := context.Background()
	s := newServer(t)
	d, _, _ := s.CreateDevice(ctx)
	full, err := s.AddRelease(ctx, "0.3.0", "darwin-aarch64", "", "full.tar.gz", fakeSig("0.3.0", "full.tar.gz"), strings.NewReader("f"))
	if err != nil {
		t.Fatal(err)
	}
	lite, err := s.AddRelease(ctx, "0.5.0", "lite-darwin-aarch64", "", "lite.tar.gz", fakeSig("0.5.0", "lite.tar.gz"), strings.NewReader("l"))
	if err != nil {
		t.Fatal(err)
	}
	_ = s.SetReleasePublished(ctx, full.ID, true)
	_ = s.SetReleasePublished(ctx, lite.ID, true)
	if r, ok, err := s.LatestRelease(ctx, "darwin-aarch64", d.ID); err != nil || !ok || r.ID != full.ID {
		t.Fatalf("完整版应拿到 0.3.0：%+v %v %v", r, ok, err)
	}
	if r, ok, err := s.LatestRelease(ctx, "lite-darwin-aarch64", d.ID); err != nil || !ok || r.ID != lite.ID {
		t.Fatalf("Lite 版应拿到 0.5.0：%+v %v %v", r, ok, err)
	}
	if _, err := s.AddRelease(ctx, "0.5.0", "lite-linux-x86_64", "", "x.tar.gz", fakeSig("0.5.0", "x.tar.gz"), strings.NewReader("x")); err == nil {
		t.Fatal("未知平台应被拒绝")
	}
}
