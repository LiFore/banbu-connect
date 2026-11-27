package killswitch

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

// 刚启动的服务没有任何通知：列表必须是 []。托盘外壳把 null 当成解析失败，会以为连不上服务
// （0.2.2 重新安装服务后安装页卡在「没有在预期时间内启动」）。
func TestFreshStatusHasNoNullLists(t *testing.T) {
	e, err := NewEngine(filepath.Join(t.TempDir(), "config.json"), &FakeBackend{}, &fakeProber{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(e.Status())
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{`"notices":null`, `"events":null`, `"rule_checks":null`, `"warnings":null`} {
		if strings.Contains(string(b), key) {
			t.Errorf("fresh status contains %s: %s", key, b)
		}
	}
}
