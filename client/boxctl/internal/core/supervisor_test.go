package core

import "testing"

func TestTUNFailure(t *testing.T) {
	log := `time="2026-11-09T10:00:00" level=warning msg="other"
time="2026-11-09T10:00:01" level=error msg="Start TUN listening error: first"
time="2026-11-09T10:00:02" level=error msg="Start TUN listening error: gVisor is not included in this build, rebuild with -tags with_gvisor"
time="2026-11-09T10:00:03" level=warning msg="later"
`
	want := "gVisor is not included in this build, rebuild with -tags with_gvisor"
	if got := tunFailure(log); got != want {
		t.Fatalf("got %q", got)
	}
	if got := tunFailure("level=warning msg=\"other\"\n"); got != "" {
		t.Fatalf("没有失败记录时应为空：%q", got)
	}
}
