package subscriptions

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"
)

func TestSchedulerUpdatesDueSubscriptions(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	sb := f.create(t, "jsyun") // 默认 12 小时更新一次
	if sb.Options.UpdateHours != 12 {
		t.Fatalf("默认间隔 = %d", sb.Options.UpdateHours)
	}
	now := time.Date(2026, 11, 17, 8, 0, 0, 0, time.UTC)
	f.svc.now = func() time.Time { return now }
	attempts := map[string]time.Time{}

	// 从没拉取过：马上更新。
	if !f.svc.runDue(ctx, log, attempts) {
		t.Fatal("首次应更新并报告节点变化")
	}
	got, _ := f.svc.Get(ctx, sb.ID, false)
	if got.NodeCount == 0 || got.LastFetchAt == "" {
		t.Fatalf("应写入节点：%+v", got)
	}

	// 未到期不更新。
	now = now.Add(11 * time.Hour)
	if f.svc.runDue(ctx, log, attempts) {
		t.Fatal("未到 12 小时不应更新")
	}

	// 到期但拉取失败：保留节点，记录原因，30 分钟内不重试。
	now = now.Add(2 * time.Hour)
	good := f.body
	f.body = []byte("<html>请登录</html>")
	if f.svc.runDue(ctx, log, attempts) {
		t.Fatal("失败不报告变化")
	}
	got, _ = f.svc.Get(ctx, sb.ID, false)
	if got.LastError == "" || got.NodeCount == 0 {
		t.Fatalf("失败应保留节点并记录原因：%+v", got)
	}
	f.body = good
	now = now.Add(10 * time.Minute)
	f.svc.runDue(ctx, log, attempts)
	if got2, _ := f.svc.Get(ctx, sb.ID, false); got2.LastError == "" {
		t.Fatal("30 分钟内不应重试")
	}
	now = now.Add(25 * time.Minute)
	f.svc.runDue(ctx, log, attempts)
	if got2, _ := f.svc.Get(ctx, sb.ID, false); got2.LastError != "" {
		t.Fatalf("重试成功后清除错误：%s", got2.LastError)
	}
}

func TestSchedulerSkipsDisabledAndManual(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	a := f.create(t, "a")
	off := false
	if _, err := f.svc.Update(ctx, a.ID, Input{Name: "a", Enabled: &off}); err != nil {
		t.Fatal(err)
	}
	b := f.create(t, "b")
	opts := b.Options
	opts.UpdateHours = 0
	if _, err := f.svc.Update(ctx, b.ID, Input{Name: "b", Options: &opts}); err != nil {
		t.Fatal(err)
	}
	if f.svc.runDue(ctx, log, map[string]time.Time{}) {
		t.Fatal("停用的和不自动更新的订阅都不应更新")
	}
}
