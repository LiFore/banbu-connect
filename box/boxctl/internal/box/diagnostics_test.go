package box

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"boxctl/internal/subscriptions"
)

func TestDiagnosticsHasNoSecrets(t *testing.T) {
	b, h, _ := setup(t)
	ctx := context.Background()
	// 订阅地址带令牌：写日志、记录错误，都不应进诊断包。
	if _, err := b.subs.Create(ctx, subscriptions.Input{Name: "s", URL: "https://sub.example.com/api?token=SUBTOKEN123"}); err != nil {
		t.Fatal(err)
	}
	b.log.Warn("拉取失败", "url", "https://sub.example.com/api?token=SUBTOKEN123", "cfg", "password: NODEPASS456")
	_ = b.recordVersion(ctx, []byte("secret: CTRLSECRET789\nproxies:\n  - name: a\n    password: NODEPASS456\n"), mustPolicy(t, b))

	w := call(t, h, http.MethodGet, "/api/v1/box/diagnostics", "")
	if w.Code != http.StatusOK || !strings.HasPrefix(w.Header().Get("Content-Disposition"), "attachment") {
		t.Fatalf("diagnostics: %d", w.Code)
	}
	gz, err := gzip.NewReader(bytes.NewReader(w.Body.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gz)
	var names []string
	var all bytes.Buffer
	for {
		hd, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, hd.Name)
		_, _ = io.Copy(&all, tr)
	}
	for _, want := range []string{"boxctl-diag/status.json", "boxctl-diag/guard-ruleset.txt", "boxctl-diag/logs/panel.log", "boxctl-diag/system.txt"} {
		found := false
		for _, n := range names {
			found = found || n == want
		}
		if !found {
			t.Errorf("缺少 %s：%v", want, names)
		}
	}
	for _, secret := range []string{"SUBTOKEN123", "NODEPASS456", "CTRLSECRET789"} {
		if strings.Contains(all.String(), secret) {
			t.Errorf("诊断包里有凭据 %s", secret)
		}
	}
}
