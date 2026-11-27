package guard

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestPrepareAndRestoreSystem(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	u := &FakeUCI{Values: map[string]string{
		"firewall.@defaults[0].flow_offloading": "1",
		"dhcp.lan.ra":                           "server",
		"dhcp.lan.dhcpv6":                       "server",
		// flow_offloading_hw 未设置：等同于关闭，不改
	}}
	changed, err := prepareSystem(ctx, u, dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(changed) != 3 {
		t.Fatalf("changed = %v", changed)
	}
	if u.Values["firewall.@defaults[0].flow_offloading"] != "0" || u.Values["dhcp.lan.ra"] != "disabled" || u.Values["dhcp.lan.dhcpv6"] != "disabled" {
		t.Fatalf("values = %v", u.Values)
	}
	if _, ok := u.Values["firewall.@defaults[0].flow_offloading_hw"]; ok {
		t.Fatal("未设置的项不应写入")
	}
	if !slices.Equal(u.Reloaded, []string{"firewall", "odhcpd"}) {
		t.Fatalf("reloaded = %v", u.Reloaded)
	}

	// 已符合时不再改、不重载。
	u.Reloaded = nil
	changed, err = prepareSystem(ctx, u, dir)
	if err != nil || len(changed) != 0 || len(u.Reloaded) != 0 {
		t.Fatalf("second prepare: changed=%v reloaded=%v err=%v", changed, u.Reloaded, err)
	}

	// 用户在 LuCI 里重新打开流量卸载：改回去，但原值仍是第一次记下的。
	u.Values["firewall.@defaults[0].flow_offloading"] = "1"
	if changed, _ = prepareSystem(ctx, u, dir); len(changed) != 1 {
		t.Fatalf("re-prepare changed = %v", changed)
	}

	if err := restoreSystem(ctx, u, dir); err != nil {
		t.Fatal(err)
	}
	if u.Values["firewall.@defaults[0].flow_offloading"] != "1" || u.Values["dhcp.lan.ra"] != "server" || u.Values["dhcp.lan.dhcpv6"] != "server" {
		t.Fatalf("restored = %v", u.Values)
	}
	if _, err := os.Stat(filepath.Join(dir, sysprepFile)); !os.IsNotExist(err) {
		t.Fatal("恢复后应删除原值文件")
	}
}

func TestRestoreDeletesKeysThatDidNotExist(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	// 未设置时 odhcpd 的 RA 也是关闭的，但这里模拟默认值为开启的旧版本：Unset 不符合时会写入。
	u := &FakeUCI{Values: map[string]string{"firewall.@defaults[0].flow_offloading_hw": "1"}}
	if _, err := prepareSystem(ctx, u, dir); err != nil {
		t.Fatal(err)
	}
	u.Values["firewall.@defaults[0].flow_offloading_hw"] = "0"
	if err := restoreSystem(ctx, u, dir); err != nil {
		t.Fatal(err)
	}
	if u.Values["firewall.@defaults[0].flow_offloading_hw"] != "1" {
		t.Fatalf("values = %v", u.Values)
	}
}

func TestEngineChecksSystemPeriodically(t *testing.T) {
	ctx := context.Background()
	sys := &FakeSystem{}
	u := &FakeUCI{Values: map[string]string{}}
	e := NewEngine(sys, t.TempDir(), "br-lan", true, slog.New(slog.NewTextHandler(io.Discard, nil)))
	e.SetUCI(u)
	e.Start(ctx)
	u.Values["firewall.@defaults[0].flow_offloading"] = "1"
	for i := 0; i < sysprepEvery; i++ {
		e.Check(ctx)
	}
	if u.Values["firewall.@defaults[0].flow_offloading"] != "0" {
		t.Fatal("自检应改回流量卸载")
	}
	found := false
	for _, ev := range e.Status().Events {
		found = found || strings.Contains(ev.Message, "关闭软件流量卸载")
	}
	if !found {
		t.Fatal("应记录修改系统设置的事件")
	}
}
