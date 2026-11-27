package core

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// confirmFixture 是一个「正在运行」的 Supervisor：内核控制接口由 httptest 代替，记录热重载。
type confirmFixture struct {
	s       *Supervisor
	reloads atomic.Int32
	healthy atomic.Bool
	mu      sync.Mutex
	events  []ConfirmEvent
}

func newConfirmFixture(t *testing.T) *confirmFixture {
	t.Helper()
	old := [3]time.Duration{confirmFirst, confirmEvery, confirmWindow}
	confirmFirst, confirmEvery, confirmWindow = 10*time.Millisecond, 10*time.Millisecond, 80*time.Millisecond
	t.Cleanup(func() { confirmFirst, confirmEvery, confirmWindow = old[0], old[1], old[2] })

	f := &confirmFixture{}
	ctl := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut && r.URL.Path == "/configs" {
			f.reloads.Add(1)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(ctl.Close)
	hooks := Hooks{
		Confirm: func(context.Context) error {
			if f.healthy.Load() {
				return nil
			}
			return errors.New("出口不通")
		},
		OnConfirm: func(ev ConfirmEvent) {
			f.mu.Lock()
			f.events = append(f.events, ev)
			f.mu.Unlock()
		},
	}
	f.s = NewSupervisor(Data{}, Store{}, hooks, "", t.TempDir(), "x", slog.New(slog.NewTextHandler(io.Discard, nil)))
	f.s.controller = strings.TrimPrefix(ctl.URL, "http://")
	f.s.child = &exec.Cmd{} // 只用来表示内核在运行
	return f
}

func (f *confirmFixture) last() ConfirmEvent {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.events) == 0 {
		return ConfirmEvent{}
	}
	return f.events[len(f.events)-1]
}

// applied 模拟 apply 成功后的状态，返回本次的 gen。
func (f *confirmFixture) applied(yaml string) int {
	f.s.mu.Lock()
	defer f.s.mu.Unlock()
	f.s.applied = []byte(yaml)
	f.s.confirmGen++
	return f.s.confirmGen
}

func TestConfirmRollsBackChangeInducedFailure(t *testing.T) {
	f := newConfirmFixture(t)
	ctx := context.Background()
	f.s.lastGood = []byte("good: 1\n")

	gen := f.applied("bad: 1\n")
	f.s.confirm(ctx, gen, []byte("bad: 1\n"), true)

	if ev := f.last(); ev.State != ConfirmRolledBack || ev.Hash != ConfigHash([]byte("bad: 1\n")) {
		t.Fatalf("应回滚：%+v", ev)
	}
	if f.reloads.Load() != 1 {
		t.Fatalf("回滚应热重载一次，实际 %d", f.reloads.Load())
	}
	b, _ := os.ReadFile(filepath.Join(f.s.coreDir, configFile))
	if string(b) != "good: 1\n" || string(f.s.applied) != "good: 1\n" {
		t.Fatalf("磁盘和内存都应换回上一版：%q %q", b, f.s.applied)
	}
	st := f.s.Status()
	if st.Confirm != ConfirmRolledBack || !strings.Contains(st.LastError, "已回滚") {
		t.Fatalf("状态：%+v", st)
	}
	if f.s.rejected != ConfigHash([]byte("bad: 1\n")) {
		t.Fatal("被回滚的配置应被拒绝再次应用")
	}
}

func TestConfirmDoesNotRollBackUpstreamFailure(t *testing.T) {
	f := newConfirmFixture(t)
	f.s.lastGood = []byte("good: 1\n")
	gen := f.applied("new: 1\n")
	// 变更前出口就不通：上游故障，不回滚（M8-07）。
	f.s.confirm(context.Background(), gen, []byte("new: 1\n"), false)
	if ev := f.last(); ev.State != ConfirmUpstream {
		t.Fatalf("应判为上游故障：%+v", ev)
	}
	if f.reloads.Load() != 0 || string(f.s.applied) != "new: 1\n" {
		t.Fatal("不应回滚")
	}
}

func TestConfirmSuccessBecomesRollbackTarget(t *testing.T) {
	f := newConfirmFixture(t)
	f.healthy.Store(true)
	gen := f.applied("v2: 1\n")
	f.s.confirm(context.Background(), gen, []byte("v2: 1\n"), true)
	if ev := f.last(); ev.State != ConfirmOK {
		t.Fatalf("应确认：%+v", ev)
	}
	if string(f.s.lastGood) != "v2: 1\n" {
		t.Fatal("确认过的配置应成为回滚目标")
	}
	b, err := os.ReadFile(filepath.Join(f.s.coreDir, lastGoodFile))
	if err != nil || string(b) != "v2: 1\n" {
		t.Fatalf("应持久化：%q %v", b, err)
	}
}

func TestConfirmStaleTaskDoesNothing(t *testing.T) {
	f := newConfirmFixture(t)
	f.s.lastGood = []byte("good: 1\n")
	gen := f.applied("bad: 1\n")
	f.applied("newer: 1\n") // 确认期间又应用了新配置
	f.s.confirm(context.Background(), gen, []byte("bad: 1\n"), true)
	if f.reloads.Load() != 0 || len(f.events) != 0 {
		t.Fatal("过时的确认任务不应回滚")
	}
}

func TestConfirmWithoutRollbackTarget(t *testing.T) {
	f := newConfirmFixture(t)
	gen := f.applied("first: 1\n")
	f.s.confirm(context.Background(), gen, []byte("first: 1\n"), true)
	if ev := f.last(); ev.State != ConfirmFailed {
		t.Fatalf("没有可回滚的配置：%+v", ev)
	}
}
