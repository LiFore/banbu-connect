package desktop

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"

	"boxctl/internal/core"
	"boxctl/internal/killswitch"
)

const serviceName = "BanbuConnect"

func notifyContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt)
}

func dataDir() string {
	root := os.Getenv("ProgramData")
	if root == "" {
		root = `C:\ProgramData`
	}
	return filepath.Join(root, dirName)
}

func installDir() string {
	root := os.Getenv("ProgramFiles")
	if root == "" {
		root = `C:\Program Files`
	}
	return filepath.Join(root, dirName)
}

// lockDataDir 收紧目录权限：ProgramData 默认所有用户可读，而里面有凭据和令牌。
// 用 SID 而不是组名，组名在中文系统上不一样。
func lockDataDir(dir string) error {
	out, err := exec.Command("icacls", dir, "/inheritance:r",
		"/grant:r", "*S-1-5-18:(OI)(CI)F", "/grant:r", "*S-1-5-32-544:(OI)(CI)F").CombinedOutput()
	if err != nil {
		return fmt.Errorf("设置目录权限失败：%w: %s", err, out)
	}
	return nil
}

// TODO(verify:V15) install、uninstall 和服务生命周期还没有在真实机器上运行过。
// TODO(verify:V16) icacls 的效果，以及管理员账号与日常账号不同时的令牌读取。
func installService(_, tokenPath string) error {
	if m, err := mgr.Connect(); err == nil {
		legacy := map[string]string{
			legacyWindowsDesktop: `检测到改名前的 Boxctl 后台服务，两者写同一套系统规则，请先在管理员终端运行 "C:\Program Files\boxctl\boxctl.exe" desktop uninstall`,
			legacyWindowsGate:    `检测到旧版 Boxctl Gate 的后台服务，它的功能已经并入本客户端，请先在管理员终端运行 "C:\Program Files\boxctl-gate\boxctl.exe" uninstall`,
		}
		for name, msg := range legacy {
			if s, err := m.OpenService(name); err == nil {
				s.Close()
				m.Disconnect()
				return errors.New(msg)
			}
		}
		m.Disconnect()
	}
	// 已安装时是升级（应用更新后重新安装）：先停服务，才能替换正在运行的程序。
	upgrading, err := stopExisting()
	if err != nil {
		return err
	}
	// 服务以 SYSTEM 运行，必须从普通用户写不了的目录启动。
	dir := installDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("需要管理员权限：%w", err)
	}
	target := filepath.Join(dir, "banbu-connect.exe")
	if err := copyExecutable(target); err != nil {
		return err
	}
	data := dataDir()
	if err := os.MkdirAll(data, 0o700); err != nil {
		return err
	}
	if err := lockDataDir(data); err != nil {
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
	// 让当前用户不提权也能打开界面；LOCALAPPDATA 默认只有本人可读。
	if tokenPath == "" {
		tokenPath = filepath.Join(os.Getenv("LOCALAPPDATA"), dirName, "token")
	}
	if err := writeSecret(tokenPath, token); err != nil {
		return err
	}

	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("需要管理员权限：%w", err)
	}
	defer m.Disconnect()
	if upgrading {
		s, err := m.OpenService(serviceName)
		if err != nil {
			return err
		}
		defer s.Close()
		if err := s.Start(); err != nil {
			return err
		}
		fmt.Println("已更新并启动。")
		return nil
	}
	s, err := m.CreateService(serviceName, target, mgr.Config{
		DisplayName: productName,
		Description: "运行代理内核和保护服务。",
		StartType:   mgr.StartAutomatic,
	}, "desktop", "run")
	if err != nil {
		return err
	}
	defer s.Close()
	_ = s.SetRecoveryActions([]mgr.RecoveryAction{
		{Type: mgr.ServiceRestart, Delay: 5 * time.Second},
		{Type: mgr.ServiceRestart, Delay: 30 * time.Second},
	}, 86400)
	if err := s.Start(); err != nil {
		return err
	}
	fmt.Println("已安装并启动。")
	return nil
}

// stopExisting 停止已安装的服务，返回它是否存在。
func stopExisting() (bool, error) {
	m, err := mgr.Connect()
	if err != nil {
		return false, fmt.Errorf("需要管理员权限：%w", err)
	}
	defer m.Disconnect()
	s, err := m.OpenService(serviceName)
	if err != nil {
		return false, nil
	}
	defer s.Close()
	st, err := s.Query()
	if err != nil {
		return true, err
	}
	if st.State != svc.Stopped {
		if st, err = s.Control(svc.Stop); err != nil {
			return true, fmt.Errorf("停止旧服务失败：%w", err)
		}
		for i := 0; i < 60 && st.State != svc.Stopped; i++ {
			time.Sleep(500 * time.Millisecond)
			if st, err = s.Query(); err != nil {
				return true, err
			}
		}
		if st.State != svc.Stopped {
			return true, errors.New("旧服务没有在 30 秒内停止")
		}
	}
	return true, nil
}

func uninstallService() error {
	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("需要管理员权限：%w", err)
	}
	defer m.Disconnect()
	if s, err := m.OpenService(serviceName); err == nil {
		defer s.Close()
		if st, err := s.Control(svc.Stop); err == nil {
			for i := 0; i < 40 && st.State != svc.Stopped; i++ {
				time.Sleep(500 * time.Millisecond)
				if st, err = s.Query(); err != nil {
					break
				}
			}
		}
		if err := s.Delete(); err != nil {
			return err
		}
	}
	if err := killswitch.NewSystemBackend().Remove(context.Background()); err != nil {
		return fmt.Errorf("撤销规则失败：%w", err)
	}
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
	dst, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return fmt.Errorf("需要管理员权限：%w", err)
	}
	if _, err := io.Copy(dst, src); err != nil {
		dst.Close()
		return err
	}
	return dst.Close()
}

func runAsService(opts Options) (bool, error) {
	isService, err := svc.IsWindowsService()
	if err != nil {
		return true, err
	}
	if !isService {
		return false, nil
	}
	return true, svc.Run(serviceName, &serviceHandler{opts: opts})
}

type serviceHandler struct{ opts Options }

func (h *serviceHandler) Execute(_ []string, req <-chan svc.ChangeRequest, status chan<- svc.Status) (bool, uint32) {
	status <- svc.Status{State: svc.StartPending}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- Run(ctx, h.opts) }()
	status <- svc.Status{State: svc.Running, Accepts: svc.AcceptStop | svc.AcceptShutdown}
	for {
		select {
		case err := <-done:
			if err != nil {
				return true, 1 // 触发恢复动作
			}
			return false, 0
		case c := <-req:
			switch c.Cmd {
			case svc.Interrogate:
				status <- c.CurrentStatus
			case svc.Stop, svc.Shutdown:
				status <- svc.Status{State: svc.StopPending}
				cancel()
				<-done
				return false, 0
			}
		}
	}
}

// stopSelf 结束服务：Run 正常返回后服务以退出码 0 停止，不触发恢复动作；下次开机仍会自动启动。
func stopSelf(quit context.CancelFunc) { quit() }

// spawnUninstall 在独立进程里卸载服务（客户端「卸载」）：服务以 LocalSystem 运行，子进程继承权限；
// 在服务进程里直接卸载会等自己停止，卡住。DETACHED_PROCESS：服务停止后子进程继续。
func spawnUninstall() error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	cmd := exec.Command(self, "desktop", "uninstall")
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: 0x00000008 | 0x00000200} // DETACHED_PROCESS | CREATE_NEW_PROCESS_GROUP
	return cmd.Start()
}
