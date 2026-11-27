package desktop

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"boxctl/internal/core"
	"boxctl/internal/killswitch"
)

const (
	serviceLabel = "com.banbu.connect"
	servicePlist = "/Library/LaunchDaemons/" + serviceLabel + ".plist"
	binaryPath   = "/usr/local/libexec/banbu-connect"
)

func notifyContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}

func dataDir() string { return "/Library/Application Support/" + dirName }

func launchctl(args ...string) error {
	out, err := exec.Command("/bin/launchctl", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("launchctl %s: %w: %s", args[0], err, out)
	}
	return nil
}

// TODO(verify:V15) install、uninstall 和服务生命周期还没有在真实机器上运行过。
// TODO(verify:V18) 未公证的二进制和 App 能否直接运行。
func installService(username, tokenPath string) error {
	if os.Geteuid() != 0 {
		return errors.New("需要管理员权限，请用 sudo 运行")
	}
	if _, err := os.Stat(legacyDarwinDesktopPlist); err == nil {
		return errors.New("检测到改名前的 Boxctl 后台服务，两者写同一套系统规则，请先运行 sudo /usr/local/libexec/boxctl desktop uninstall")
	}
	if _, err := os.Stat(legacyDarwinGatePlist); err == nil {
		return errors.New("检测到旧版 Boxctl Gate 的后台服务，它的功能已经并入本客户端，请先运行 sudo /usr/local/libexec/boxctl-gate uninstall")
	}
	// 服务以 root 运行，必须从普通用户写不了的目录启动。
	if err := os.MkdirAll(filepath.Dir(binaryPath), 0o755); err != nil {
		return err
	}
	_ = launchctl("bootout", "system", servicePlist) // 先停，下面才能替换正在运行的文件
	if err := copyExecutable(binaryPath); err != nil {
		return err
	}
	data := dataDir()
	if err := os.MkdirAll(data, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(data, 0o700); err != nil {
		return err
	}
	// 随应用打包的 geo 数据：内核第一次校验不用再下载。
	if self, err := os.Executable(); err == nil {
		_ = core.SeedGeoData(filepath.Dir(self), filepath.Join(data, "core"))
	}
	token, err := ensureSecretFile(filepath.Join(data, tokenFileName))
	if err != nil {
		return err
	}
	if err := installUserToken(token, username, tokenPath); err != nil {
		return err
	}
	plist := `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>` + serviceLabel + `</string>
  <key>ProgramArguments</key>
  <array><string>` + binaryPath + `</string><string>desktop</string><string>run</string></array>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
  <key>StandardOutPath</key><string>` + launchdLog() + `</string>
  <key>StandardErrorPath</key><string>` + launchdLog() + `</string>
</dict>
</plist>
`
	if err := os.WriteFile(servicePlist, []byte(plist), 0o644); err != nil {
		return err
	}
	// 启动阶段直接退出的错误（比如数据迁移失败）只会写到 stderr：由 launchd 记到这个文件，太大时先清掉。
	if st, err := os.Stat(launchdLog()); err == nil && st.Size() > maxLogSize {
		_ = os.Remove(launchdLog())
	}
	if err := launchctl("bootstrap", "system", servicePlist); err != nil {
		return err
	}
	if err := waitServiceUp(30 * time.Second); err != nil {
		return err
	}
	fmt.Println("已安装并启动。")
	return nil
}

func launchdLog() string { return filepath.Join(dataDir(), "launchd.log") }

// waitServiceUp 等服务开始监听：launchctl bootstrap 成功只说明交给了 launchd，服务可能启动后立刻退出、被反复拉起。
// 起不来时把启动错误带回去，安装页直接显示原因。
func waitServiceUp(timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if c, err := net.DialTimeout("tcp", DefaultAddr, time.Second); err == nil {
			_ = c.Close()
			return nil
		}
		time.Sleep(500 * time.Millisecond)
	}
	msg := fmt.Sprintf("服务已安装，但 %d 秒内没有开始监听 %s", int(timeout.Seconds()), DefaultAddr)
	if tail := tailFile(launchdLog(), 8); tail != "" {
		msg += "。启动输出：\n" + tail
	}
	if tail := tailFile(filepath.Join(dataDir(), "boxctl.log"), 5); tail != "" {
		msg += "\n服务日志：\n" + tail
	}
	return errors.New(msg)
}

// tailFile 返回文件最后 n 行（只读最后 16 KB）。
func tailFile(path string, n int) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	if st, err := f.Stat(); err == nil && st.Size() > 16<<10 {
		_, _ = f.Seek(-16<<10, io.SeekEnd)
	}
	b, _ := io.ReadAll(f)
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// installUserToken 让使用客户端的用户不提权也能读取令牌。
func installUserToken(token, username, tokenPath string) error {
	if username == "" {
		username = os.Getenv("SUDO_USER")
	}
	if username == "" {
		return errors.New("无法确定用户，请加 -user 参数")
	}
	u, err := user.Lookup(username)
	if err != nil {
		return err
	}
	if tokenPath == "" {
		tokenPath = filepath.Join(u.HomeDir, "Library", "Application Support", dirName, "token")
	}
	if err := writeSecret(tokenPath, token); err != nil {
		return err
	}
	uid, err := strconv.Atoi(u.Uid)
	if err != nil {
		return err
	}
	gid, err := strconv.Atoi(u.Gid)
	if err != nil {
		return err
	}
	if err := os.Chown(filepath.Dir(tokenPath), uid, gid); err != nil {
		return err
	}
	return os.Chown(tokenPath, uid, gid)
}

func uninstallService() error {
	if os.Geteuid() != 0 {
		return errors.New("需要管理员权限，请用 sudo 运行")
	}
	_ = launchctl("bootout", "system", servicePlist)
	_ = os.Remove(servicePlist)
	if err := killswitch.NewSystemBackend().Remove(context.Background()); err != nil {
		return fmt.Errorf("撤销规则失败：%w", err)
	}
	_ = os.Remove(binaryPath)
	fmt.Println("已卸载，保护规则已撤销。数据保留在 " + dataDir())
	return nil
}

func copyExecutable(target string) error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	if self == target {
		return nil
	}
	src, err := os.Open(self)
	if err != nil {
		return err
	}
	defer src.Close()
	// 先删再建：覆盖可执行文件的内容会使它的签名失效。
	_ = os.Remove(target)
	dst, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(dst, src); err != nil {
		dst.Close()
		return err
	}
	return dst.Close()
}

func runAsService(Options) (bool, error) { return false, nil }

// stopSelf 结束服务。launchd 的 KeepAlive 会立刻拉起退出的进程，所以由 launchd 卸载：
// 它发 SIGTERM，notifyContext 收到后正常退出。下次开机按 RunAtLoad 自动启动。
// 不是由 launchd 启动的（开发时直接运行）就直接退出。
func stopSelf(quit context.CancelFunc) {
	if os.Getppid() != 1 {
		quit()
		return
	}
	cmd := exec.Command("/bin/launchctl", "bootout", "system/"+serviceLabel)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true} // 不随服务进程一起被结束
	if err := cmd.Start(); err != nil {
		quit() // 退不掉 launchd 的话，至少停掉内核；KeepAlive 会重新拉起服务
	}
}

// spawnUninstall 在独立进程里卸载服务（客户端「卸载」）：服务以 root 运行，子进程继承权限，
// 用新会话脱离 launchd 的进程组，卸载时 bootout 服务不会把它一起结束。
func spawnUninstall() error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	cmd := exec.Command(self, "desktop", "uninstall")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	return cmd.Start()
}
