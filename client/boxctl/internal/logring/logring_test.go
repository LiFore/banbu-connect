package logring

import (
	"io"
	"log/slog"
	"strings"
	"testing"
)

func TestRingAndRedact(t *testing.T) {
	r := New(3)
	log := slog.New(NewHandler(slog.NewTextHandler(io.Discard, nil), r, "panel"))
	log.Info("拉取订阅", "url", "https://sub.example.com/api?token=SECRET1&x=1")
	log.Warn("节点", "cfg", "password: SECRET2 server: 1.2.3.4")
	log.Error("代理", "u", "http://user:SECRET3@1.2.3.4:80")
	log.Info("第四条")
	all := r.Entries("")
	if len(all) != 3 || all[2].Message != "第四条" {
		t.Fatalf("环形缓冲：%+v", all)
	}
	joined := ""
	for _, e := range all {
		joined += e.Message + "\n"
	}
	for _, s := range []string{"SECRET2", "SECRET3"} {
		if strings.Contains(joined, s) {
			t.Fatalf("未脱敏 %s：%s", s, joined)
		}
	}
	if Redact("https://a/b?token=SECRET1&x=1") != "https://a/b?token=***&x=1" {
		t.Fatal(Redact("https://a/b?token=SECRET1&x=1"))
	}
	if w := r.Entries("WARN"); len(w) != 2 {
		t.Fatalf("按级别筛选：%+v", w)
	}
}
