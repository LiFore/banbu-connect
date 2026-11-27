package desktop

import (
	"testing"
	"time"

	"boxctl/internal/dist"
)

func newAutoUpdater(t *testing.T, mode string) (*appUpdater, *[]string, *time.Time) {
	t.Helper()
	c, err := dist.NewClient(t.TempDir(), "0.2.6", nil)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 23, 10, 0, 0, 0, time.UTC)
	var notes []string
	u := &appUpdater{client: c, now: func() time.Time { return now }, notify: func(title, _ string) { notes = append(notes, title) }}
	u.available = &dist.UpdateInfo{Version: "0.3.0", Mode: mode}
	return u, &notes, &now
}

func TestAutoUpdatePush(t *testing.T) {
	u, notes, _ := newAutoUpdater(t, dist.UpdatePush)
	u.auto()
	u.auto()
	if len(*notes) != 1 || u.Pending() != nil {
		t.Fatalf("push: notes %v, request %+v", *notes, u.Pending())
	}
}

func TestAutoUpdateSilent(t *testing.T) {
	u, notes, _ := newAutoUpdater(t, dist.UpdateSilent)
	u.auto()
	req := u.Pending()
	if req == nil || req.Action != "install" || !u.status().Auto || len(*notes) != 0 {
		t.Fatalf("silent install = %+v notes %v", req, *notes)
	}
	// 下载中不重复请求。
	u.setProgress(AppUpdateProgress{ID: req.ID, Phase: "downloading", Version: "0.3.0"})
	u.auto()
	if u.Pending().ID != req.ID {
		t.Fatal("should not request again while downloading")
	}
	// 装好后请外壳空闲时重启，只请求一次。
	u.setProgress(AppUpdateProgress{ID: req.ID, Phase: "installed", Version: "0.3.0"})
	u.auto()
	idle := u.Pending()
	if idle == nil || idle.Action != "restart_when_idle" {
		t.Fatalf("idle restart = %+v", idle)
	}
	u.auto()
	if u.Pending().ID != idle.ID {
		t.Fatal("idle restart requested twice")
	}
}

func TestAutoUpdateForce(t *testing.T) {
	u, notes, now := newAutoUpdater(t, dist.UpdateForce)
	u.auto()
	req := u.Pending()
	if req == nil || req.Action != "install" || len(*notes) != 1 {
		t.Fatalf("force install = %+v notes %v", req, *notes)
	}
	// 失败后一小时内不重试。
	u.setProgress(AppUpdateProgress{ID: req.ID, Phase: "failed", Version: "0.3.0"})
	*now = now.Add(10 * time.Minute)
	u.auto()
	if u.Pending().ID != req.ID {
		t.Fatal("should wait before retrying")
	}
	*now = now.Add(time.Hour)
	u.auto()
	req = u.Pending()
	if req.Action != "install" {
		t.Fatalf("retry = %+v", req)
	}
	// 装好后和静默更新一样请外壳空闲时重启（主界面打开时由页面全屏拦住并立即重启），不再倒计时。
	u.setProgress(AppUpdateProgress{ID: req.ID, Phase: "installed", Version: "0.3.0"})
	u.auto()
	if p := u.Pending(); p == nil || p.Action != "restart_when_idle" || len(*notes) != 1 {
		t.Fatalf("idle restart = %+v notes %v", p, *notes)
	}
}
