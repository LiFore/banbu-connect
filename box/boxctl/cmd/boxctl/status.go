package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"os"
	"strconv"
	"strings"
	"time"

	"boxctl/internal/box"
	"boxctl/internal/guard"
	"boxctl/internal/ipc"
)

// statusReport 是 `boxctl status` 的输出（JSON）：面板（含内置 Mihomo）和 guard 分别查询，互不影响。
// LuCI 的「服务 → Banbu Box」页每隔几秒调用一次。
type statusReport struct {
	Time       time.Time        `json:"time"`
	Panel      *box.LocalStatus `json:"panel"`
	PanelError string           `json:"panel_error,omitempty"`
	Guard      *guard.Status    `json:"guard"`
	GuardError string           `json:"guard_error,omitempty"`
	// RSS 是各进程占用的内存（字节），键为 panel、core、guard；读不到时没有对应的键。
	RSS map[string]int64 `json:"rss"`
	// CorePID 来自 procd 写的 pid 文件：内核是独立服务，面板不在时也能看到它。
	CorePID int `json:"core_pid"`
}

func runStatus(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	panelSock := fs.String("panel-sock", defaultPanelSck, "面板本机状态接口的 unix socket 路径")
	guardSock := fs.String("guard-sock", defaultGuardSck, "guard 的 unix socket 路径")
	corePID := fs.String("core-pidfile", "/var/run/boxctl/core.pid", "内核服务的 pid 文件")
	_ = fs.Parse(args)

	ctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	out := statusReport{Time: time.Now().UTC(), RSS: map[string]int64{}}

	var ps box.LocalStatus
	if err := ipc.Call(ctx, *panelSock, box.MethodStatus, nil, &ps); err != nil {
		out.PanelError = describe(err, "面板没有运行")
	} else {
		out.Panel = &ps
		addRSS(out.RSS, "panel", ps.Panel.PID)
		out.CorePID = ps.Core.Service.PID
	}
	if pid := readPIDFile(*corePID); pid > 0 {
		out.CorePID = pid
	}
	addRSS(out.RSS, "core", out.CorePID)
	var gs guard.Status
	if err := ipc.Call(ctx, *guardSock, guard.MethodStatus, nil, &gs); err != nil {
		out.GuardError = describe(err, "guard 没有运行")
	} else {
		gs.Events = firstN(gs.Events, 12)
		out.Guard = &gs
		addRSS(out.RSS, "guard", gs.PID)
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}

func readPIDFile(path string) int {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	if pid <= 0 {
		return 0
	}
	if _, err := os.Stat("/proc/" + strconv.Itoa(pid)); err != nil {
		return 0
	}
	return pid
}

func describe(err error, unreachable string) string {
	if errors.Is(err, ipc.ErrUnreachable) {
		return unreachable
	}
	return err.Error()
}

func firstN[T any](list []T, n int) []T {
	if len(list) > n {
		return list[:n]
	}
	return list
}

// addRSS 读 /proc/<pid>/status 的 VmRSS（只在 Linux 上有）。
func addRSS(m map[string]int64, key string, pid int) {
	if pid <= 0 {
		return
	}
	f, err := os.Open("/proc/" + strconv.Itoa(pid) + "/status")
	if err != nil {
		return
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if rest, ok := strings.CutPrefix(sc.Text(), "VmRSS:"); ok {
			fields := strings.Fields(rest)
			if len(fields) > 0 {
				if kb, err := strconv.ParseInt(fields[0], 10, 64); err == nil {
					m[key] = kb * 1024
				}
			}
			return
		}
	}
}
