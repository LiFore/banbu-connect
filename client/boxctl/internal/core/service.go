package core

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"go.yaml.in/yaml/v3"
)

// 路由盒的内核是 procd 的独立服务 boxctl-core（ADR-045）：面板重启、升级不再连带重启内核。
// 面板只负责生成配置、热重载、启停服务；内核崩溃时由 procd 重启（用磁盘上的同一份配置）。
// 面板每 500 毫秒检查一次内核：进程（procd 写的 pid 文件）和控制接口（GET /version）。
// 桌面客户端仍由面板拉起子进程（没有 UseService）。

// ServiceOptions 是独立服务的位置。
type ServiceOptions struct {
	Init    string // init 脚本，如 /etc/init.d/boxctl-core
	PIDFile string // procd 写的 pid 文件
	LogFile string // 内核的日志文件（-log-file），读 TUN 启动失败的原因
}

// ServiceHealth 是面板对内核的最近一次检查，展示用。
type ServiceHealth struct {
	Managed      string    `json:"managed"` // service：procd 独立服务；child：面板的子进程
	Online       bool      `json:"online"`
	PID          int       `json:"pid"`
	StartedAt    time.Time `json:"started_at,omitempty"`
	LatencyMS    int64     `json:"latency_ms"`
	CheckedAt    time.Time `json:"checked_at,omitempty"`
	LastError    string    `json:"last_error,omitempty"`
	Version      string    `json:"version,omitempty"`    // 正在运行的内核版本
	Bundled      string    `json:"bundled"`              // 安装包里的内核版本
	NeedsRestart bool      `json:"needs_restart"`        // 两者不同：重启内核后才用上新版本
	Restarts     int       `json:"restarts"`             // 面板运行期间内核意外退出的次数
	OfflineAt    time.Time `json:"offline_at,omitempty"` // 最近一次意外离线的时间
}

const (
	servicePeriod  = 500 * time.Millisecond
	serviceTimeout = 400 * time.Millisecond
	serviceStart   = 20 * time.Second
)

type coreService struct {
	opt ServiceOptions
}

// UseService 让内核作为独立服务运行（路由盒），在 Run 之前调用。
func (s *Supervisor) UseService(o ServiceOptions) {
	s.svc = &coreService{opt: o}
	if o.LogFile != "" {
		s.logPath = o.LogFile
	}
}

// Health 返回最近一次检查的结果。
func (s *Supervisor) Health() ServiceHealth {
	s.mu.Lock()
	defer s.mu.Unlock()
	h := s.health
	h.Bundled = MihomoVersion()
	h.NeedsRestart = h.Online && h.Version != "" && h.Version != h.Bundled
	if s.svc == nil {
		h.Managed = "child"
		if s.child != nil && s.child.Process != nil {
			h.Online, h.PID, h.StartedAt = true, s.child.Process.Pid, s.childAt
		}
	} else {
		h.Managed = "service"
	}
	return h
}

func (s *Supervisor) aliveLocked() bool {
	if s.svc != nil {
		return s.online
	}
	return s.child != nil
}

func (s *Supervisor) pidLocked() int {
	if s.svc != nil {
		return s.health.PID
	}
	if s.child != nil && s.child.Process != nil {
		return s.child.Process.Pid
	}
	return 0
}

func (s *Supervisor) startCore(ctx context.Context) error {
	if s.svc == nil {
		return s.startChild()
	}
	return s.startService(ctx)
}

func (s *Supervisor) stopCore() {
	if s.svc == nil {
		s.stopChild()
		return
	}
	s.stopService()
}

func (s *Supervisor) initCmd(ctx context.Context, action string) error {
	out, err := exec.CommandContext(ctx, s.svc.opt.Init, action).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %s：%w：%s", filepath.Base(s.svc.opt.Init), action, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// startService 启用并启动服务，等内核的控制接口能访问。
func (s *Supervisor) startService(ctx context.Context) error {
	s.mu.Lock()
	s.stopping = false
	s.mu.Unlock()
	if err := s.initCmd(ctx, "enable"); err != nil {
		return err
	}
	if err := s.initCmd(ctx, "start"); err != nil {
		return err
	}
	deadline := time.Now().Add(serviceStart)
	for time.Now().Before(deadline) {
		if s.checkService(ctx) {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(servicePeriod):
		}
	}
	reason := "内核没有在 " + serviceStart.String() + " 内启动"
	if b, err := os.ReadFile(s.logPath); err == nil {
		if last := lastLines(string(b), 3); last != "" {
			reason += "：" + last
		}
	}
	return errors.New(reason)
}

// stopService 停止并禁用服务（开机不再自动启动），等内核退出。
func (s *Supervisor) stopService() {
	s.mu.Lock()
	s.stopping = true
	s.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), stopGrace+5*time.Second)
	defer cancel()
	if err := s.initCmd(ctx, "stop"); err != nil {
		s.log.Warn("停止内核服务失败", "err", err)
	}
	if err := s.initCmd(ctx, "disable"); err != nil {
		s.log.Warn("禁用内核服务失败", "err", err)
	}
	for ctx.Err() == nil && s.checkService(ctx) {
		time.Sleep(servicePeriod)
	}
}

// watchService 每 500 毫秒检查一次内核，直到 ctx 结束。面板退出时内核继续运行。
func (s *Supervisor) watchService(ctx context.Context) {
	t := time.NewTicker(servicePeriod)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.checkService(ctx)
		}
	}
}

// checkService 检查一次内核并处理上线、离线、换了进程（procd 重启）。返回内核是否在线。
func (s *Supervisor) checkService(ctx context.Context) bool {
	pid := readPID(s.svc.opt.PIDFile)
	start := time.Now()
	version, err := s.coreVersion(ctx)
	latency := time.Since(start).Milliseconds()
	online := err == nil

	s.mu.Lock()
	was, prevPID := s.online, s.health.PID
	s.online = online
	s.health.Online, s.health.CheckedAt, s.health.LatencyMS = online, time.Now().UTC(), latency
	s.health.LastError = ""
	if err != nil {
		s.health.LastError = err.Error()
	}
	if online {
		s.health.Version = version
		if pid != prevPID || !was {
			s.health.PID, s.health.StartedAt = pid, processStart(pid)
		}
	} else {
		s.health.PID, s.health.StartedAt = 0, time.Time{}
	}
	came := online && (!was || (pid != 0 && prevPID != 0 && pid != prevPID))
	went := was && !online
	stopping := s.stopping
	if came {
		s.coreGen++
		// 接管已经在运行的内核（面板刚启动、procd 重启了内核）：配置就是磁盘上的这份。
		if b, rerr := os.ReadFile(filepath.Join(s.coreDir, configFile)); rerr == nil {
			s.applied = b
			s.probes = probesFromYAML(b)
			s.routes = listenersFromYAML(b, "route-")
			s.status.ConfigHash = ConfigHash(b)
		}
		if strings.HasPrefix(s.status.LastError, "内核离线") {
			s.status.LastError = ""
		}
	}
	if went && !stopping {
		s.status.LastError = "内核离线，procd 正在重启它"
		s.health.Restarts++
		s.health.OfflineAt = time.Now().UTC()
	}
	s.mu.Unlock()

	if came || (went && !stopping) {
		if went {
			s.log.Warn("内核离线", "err", err)
		} else {
			s.log.Info("内核在线", "pid", pid, "version", version)
		}
		// 新进程的出口要重新确认；hold 副本在新进程里默认生效，要重新下发开关。
		s.LinesChanged()
		if s.hooks.OnApplied != nil {
			go s.hooks.OnApplied(context.WithoutCancel(ctx), false)
		}
	}
	return online
}

func (s *Supervisor) coreVersion(ctx context.Context) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, serviceTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.ControllerURL()+"/version", nil)
	if err != nil {
		return "", err
	}
	s.authorize(req)
	resp, err := s.client().Do(req)
	if err != nil {
		return "", fmt.Errorf("控制接口没有响应：%w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("控制接口返回 HTTP %d", resp.StatusCode)
	}
	var v struct {
		Version string `json:"version"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&v)
	return v.Version, nil
}

// readPID 读 procd 写的 pid 文件；进程不在时返回 0。
func readPID(path string) int {
	if path == "" {
		return 0
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || pid <= 0 {
		return 0
	}
	if p, err := os.FindProcess(pid); err != nil || p.Signal(syscall.Signal(0)) != nil {
		return 0
	}
	return pid
}

// processStart 从 /proc 算出进程的启动时间（只在 Linux 上有）；读不到时用现在。
func processStart(pid int) time.Time {
	now := time.Now().UTC()
	if pid <= 0 {
		return now
	}
	stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return now
	}
	// 第二段（进程名）可能含空格，从最后一个 ')' 之后数：starttime 是第 22 段。
	i := bytes.LastIndexByte(stat, ')')
	if i < 0 {
		return now
	}
	f := strings.Fields(string(stat[i+1:]))
	if len(f) < 20 {
		return now
	}
	ticks, err := strconv.ParseInt(f[19], 10, 64)
	if err != nil {
		return now
	}
	procStat, err := os.ReadFile("/proc/stat")
	if err != nil {
		return now
	}
	for _, line := range strings.Split(string(procStat), "\n") {
		if rest, ok := strings.CutPrefix(line, "btime "); ok {
			btime, err := strconv.ParseInt(strings.TrimSpace(rest), 10, 64)
			if err != nil {
				return now
			}
			return time.Unix(btime+ticks/100, 0).UTC() // CLK_TCK 在 OpenWrt 上是 100
		}
	}
	return now
}

// probesFromYAML 从配置里读出严格组的探测入口（listeners 里的 probe-N）。
func probesFromYAML(b []byte) map[string]int { return listenersFromYAML(b, "probe-") }

// listenersFromYAML 读出名字以 prefix 开头的本机入口：代理组 → 端口。
func listenersFromYAML(b []byte, prefix string) map[string]int {
	var cfg struct {
		Listeners []struct {
			Name  string `yaml:"name"`
			Port  int    `yaml:"port"`
			Proxy string `yaml:"proxy"`
		} `yaml:"listeners"`
	}
	out := map[string]int{}
	if yaml.Unmarshal(b, &cfg) != nil {
		return out
	}
	for _, l := range cfg.Listeners {
		if strings.HasPrefix(l.Name, prefix) && l.Proxy != "" && l.Port > 0 {
			out[l.Proxy] = l.Port
		}
	}
	return out
}

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, " / ")
}
