package guard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// guard 的规则要成立，系统上有几项设置必须是确定的值（M9-06、M9-08）：
//
//   - 流量卸载（软件、硬件）：连接被卸载进 flowtable 后，后续的包不再经过 forward 链，
//     guard 的丢弃规则对它们无效。连接建立时目标地址还不在严格集合里（客户端缓存了解析结果），
//     之后就会一直直连。
//   - 局域网的 IPv6 地址下发（RA、DHCPv6）：IPv6 转发已经一律丢弃，客户端拿到 IPv6 地址后
//     会优先走 IPv6、再超时回落，表现为访问变慢；不下发地址时客户端直接用 IPv4。
//
// 不需要清理 conntrack：guard 的丢弃规则不看连接状态，已建立的连接下一个包就会被丢弃；
// 关闭流量卸载后防火墙重载会重建 flowtable，已经卸载的连接也回到 forward 链。
//
// guard 启动时和之后每分钟检查一次，被改回去（例如在 LuCI 里重新打开流量卸载）就再改过来并记事件。
// 第一次修改前把原值存到数据目录，卸载时恢复。

type uciSetting struct {
	Key     string // uci 路径
	Want    string
	Unset   string // 未设置时的等效值：与 Want 相同时未设置也算符合
	Service string // 修改后要重载的服务
	Why     string
}

var requiredSettings = []uciSetting{
	{"firewall.@defaults[0].flow_offloading", "0", "0", "firewall", "关闭软件流量卸载"},
	{"firewall.@defaults[0].flow_offloading_hw", "0", "0", "firewall", "关闭硬件流量卸载"},
	{"dhcp.lan.ra", "disabled", "disabled", "odhcpd", "局域网不再通告 IPv6 路由"},
	{"dhcp.lan.dhcpv6", "disabled", "disabled", "odhcpd", "局域网不再分配 DHCPv6 地址"},
}

const sysprepFile = "guard-sysprep.json"

// UCI 读写 OpenWrt 的 uci 配置。
type UCI interface {
	Get(ctx context.Context, key string) (value string, ok bool)
	Set(ctx context.Context, key, value string) error
	Delete(ctx context.Context, key string) error
	Commit(ctx context.Context, config string) error
	// Reload 让服务读取新配置：firewall 重载，odhcpd 重启。
	Reload(ctx context.Context, service string) error
}

// original 是修改前的值。
type original struct {
	Value  string `json:"value"`
	Exists bool   `json:"exists"`
}

// prepareSystem 把必需的设置改成期望值，返回做了的修改（用于记事件）。
func prepareSystem(ctx context.Context, u UCI, dataDir string) ([]string, error) {
	path := filepath.Join(dataDir, sysprepFile)
	saved := loadOriginals(path)
	var changed []string
	var services []string
	var errs []error
	for _, s := range requiredSettings {
		cur, ok := u.Get(ctx, s.Key)
		if cur == s.Want || !ok && s.Unset == s.Want {
			continue
		}
		if _, done := saved[s.Key]; !done {
			saved[s.Key] = original{Value: cur, Exists: ok}
			// 先存原值再改：改到一半断电，卸载时仍能恢复。
			if err := saveOriginals(path, saved); err != nil {
				errs = append(errs, err)
				continue
			}
		}
		if err := u.Set(ctx, s.Key, s.Want); err != nil {
			errs = append(errs, fmt.Errorf("%s：%w", s.Why, err))
			continue
		}
		changed = append(changed, s.Why)
		if !slices.Contains(services, s.Service) {
			services = append(services, s.Service)
		}
	}
	errs = append(errs, applyServices(ctx, u, services)...)
	return changed, errors.Join(errs...)
}

// restoreSystem 恢复 prepareSystem 改过的设置（卸载时调用）。
func restoreSystem(ctx context.Context, u UCI, dataDir string) error {
	path := filepath.Join(dataDir, sysprepFile)
	saved := loadOriginals(path)
	if len(saved) == 0 {
		return nil
	}
	var services []string
	var errs []error
	for _, s := range requiredSettings {
		o, ok := saved[s.Key]
		if !ok {
			continue
		}
		var err error
		if o.Exists {
			err = u.Set(ctx, s.Key, o.Value)
		} else {
			err = u.Delete(ctx, s.Key)
		}
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if !slices.Contains(services, s.Service) {
			services = append(services, s.Service)
		}
	}
	errs = append(errs, applyServices(ctx, u, services)...)
	if len(errs) == 0 {
		_ = os.Remove(path)
	}
	return errors.Join(errs...)
}

func applyServices(ctx context.Context, u UCI, services []string) []error {
	var errs []error
	for _, svc := range services {
		config := svc
		if svc == "odhcpd" {
			config = "dhcp"
		}
		if err := u.Commit(ctx, config); err != nil {
			errs = append(errs, err)
			continue
		}
		if err := u.Reload(ctx, svc); err != nil {
			errs = append(errs, err)
		}
	}
	return errs
}

func loadOriginals(path string) map[string]original {
	out := map[string]original{}
	if b, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(b, &out)
	}
	return out
}

func saveOriginals(path string, m map[string]original) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// execUCI 调用 uci 命令和服务的 init 脚本。
type execUCI struct{}

func (execUCI) Get(ctx context.Context, key string) (string, bool) {
	out, err := run(ctx, "", "uci", "-q", "get", key)
	if err != nil {
		return "", false
	}
	return strings.TrimSpace(out), true
}

func (execUCI) Set(ctx context.Context, key, value string) error {
	_, err := run(ctx, "", "uci", "set", key+"="+value)
	return err
}

func (execUCI) Delete(ctx context.Context, key string) error {
	_, err := run(ctx, "", "uci", "-q", "delete", key)
	return err
}

func (execUCI) Commit(ctx context.Context, config string) error {
	_, err := run(ctx, "", "uci", "commit", config)
	return err
}

// TODO(verify:R4) iStoreOS 实机确认：firewall reload 后 flowtable 重建、已卸载的连接回到 forward 链；
// odhcpd 重启后客户端不再获得 IPv6 地址。
func (execUCI) Reload(ctx context.Context, service string) error {
	action := "reload"
	if service == "odhcpd" {
		action = "restart"
	}
	_, err := run(ctx, "", "/etc/init.d/"+service, action)
	return err
}

// FakeUCI 在内存里记录 uci 配置，用于开发和测试。
type FakeUCI struct {
	Values   map[string]string
	Reloaded []string
}

func (f *FakeUCI) Get(_ context.Context, key string) (string, bool) {
	v, ok := f.Values[key]
	return v, ok
}

func (f *FakeUCI) Set(_ context.Context, key, value string) error {
	if f.Values == nil {
		f.Values = map[string]string{}
	}
	f.Values[key] = value
	return nil
}

func (f *FakeUCI) Delete(_ context.Context, key string) error {
	delete(f.Values, key)
	return nil
}

func (f *FakeUCI) Commit(context.Context, string) error { return nil }

func (f *FakeUCI) Reload(_ context.Context, service string) error {
	f.Reloaded = append(f.Reloaded, service)
	return nil
}
