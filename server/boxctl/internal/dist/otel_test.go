package dist

import (
	"context"
	"testing"
	"time"

	"boxctl/internal/otel"
)

func TestOTelUploadAndQuery(t *testing.T) {
	ctx := context.Background()
	s := newServer(t)
	store, err := otel.OpenStore(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	s.OTel = store
	d, token, _ := s.CreateDevice(ctx)
	seed(t, s)
	if _, err := s.Publish(ctx, ""); err != nil {
		t.Fatal(err)
	}
	url, fp := tlsServer(t, s)
	c, _ := NewClient(t.TempDir(), "0.1.0", nil)

	now := time.Now().UnixMilli()
	batch := otel.Batch{Logs: []otel.LogRow{{
		Hash: "h1", TsMs: now, Name: "api_request", SessionID: "s1",
		Attrs: otel.Attrs{"cost_usd": "1.5", "model": "claude-opus-5-5"},
	}}}
	if sent, err := c.ReportOTel(ctx, batch); sent || err != nil {
		t.Fatalf("未绑定不应上报：%v %v", sent, err)
	}
	if _, err := c.Bind(ctx, Binding{URL: url, Token: token, Fingerprint: fp}); err != nil {
		t.Fatal(err)
	}
	for range 2 { // 重传按 hash 去重
		if sent, err := c.ReportOTel(ctx, batch); !sent || err != nil {
			t.Fatalf("上报失败：%v %v", sent, err)
		}
	}
	sum, err := store.Summarize(ctx, d.ID, 7)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Totals.Requests != 1 || sum.Totals.CostUSD != 1.5 {
		t.Fatalf("汇总：%+v", sum.Totals)
	}
	devs, _ := store.Devices(ctx, 7)
	if len(devs) != 1 || devs[0].DeviceID != d.ID {
		t.Fatalf("按客户端：%+v", devs)
	}

	if err := s.DeleteDevice(ctx, d.ID); err != nil {
		t.Fatal(err)
	}
	if devs, _ := store.Devices(ctx, 7); len(devs) != 0 {
		t.Fatalf("应随客户端删除：%+v", devs)
	}
}
