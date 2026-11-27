package trust

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// 用 WLAN API（wlanapi.dll）读当前连接的 SSID：每 3 秒检查一次网络，不能每次都起 cmd 和 netsh 进程。
// WLAN API 用不了时（没有 WLAN AutoConfig 服务、DLL 加载失败）退回 netsh。
//
// TODO(verify:V25) Windows 11 24H2 起读取 Wi-Fi 信息需要打开定位服务；以 SYSTEM 运行时是否受影响未知。
// TODO(verify:V26) 中文 SSID 是否正确（WLAN API 返回原始字节，按 UTF-8 解释；netsh 退路先 chcp 65001）。

var (
	wlanapi                = windows.NewLazySystemDLL("wlanapi.dll")
	procWlanOpenHandle     = wlanapi.NewProc("WlanOpenHandle")
	procWlanCloseHandle    = wlanapi.NewProc("WlanCloseHandle")
	procWlanEnumInterfaces = wlanapi.NewProc("WlanEnumInterfaces")
	procWlanQueryInterface = wlanapi.NewProc("WlanQueryInterface")
	procWlanFreeMemory     = wlanapi.NewProc("WlanFreeMemory")
)

const (
	wlanClientVersion           = 2
	wlanOpcodeCurrentConnection = 7 // wlan_intf_opcode_current_connection
	errInvalidState             = syscall.Errno(5023)
	wifiListTTL                 = time.Minute // Wi-Fi 网卡列表很少变
	locationHint                = "读不到 Wi-Fi 名称：Windows 11 24H2 起需要打开系统定位服务"
)

// wlanInterfaceInfo 对应 WLAN_INTERFACE_INFO。
type wlanInterfaceInfo struct {
	GUID        windows.GUID
	Description [256]uint16
	State       uint32
}

// wlanInterfaceInfoList 对应 WLAN_INTERFACE_INFO_LIST 的表头，后面紧跟 NumberOfItems 个 wlanInterfaceInfo。
type wlanInterfaceInfoList struct {
	NumberOfItems uint32
	Index         uint32
}

// wlanConnectionAttributes 对应 WLAN_CONNECTION_ATTRIBUTES 的前半部分，后面的字段用不到。
type wlanConnectionAttributes struct {
	State       uint32
	Mode        uint32
	ProfileName [256]uint16
	SSIDLength  uint32
	SSID        [32]byte
}

type systemDetector struct {
	mu     sync.Mutex
	handle uintptr
	wifi   map[string]windows.GUID // 网卡名（与 net.Interfaces 一致）→ WLAN 网卡 GUID
	wifiAt time.Time
}

func NewSystemDetector() Detector { return &systemDetector{} }

func (d *systemDetector) Detect(ctx context.Context) Network {
	return d.DetectOn(ctx, defaultInterface())
}

// DetectOn 读指定网卡的网络信息：本机内核的 TUN 接管时，出口网卡是 TUN，要看它底下的物理网卡。
func (d *systemDetector) DetectOn(ctx context.Context, iface string) Network {
	n := Network{Interface: iface}
	if n.Interface == "" {
		return n
	}
	wifi, ssid, hint, err := d.query(n.Interface)
	if err != nil {
		return netshDetect(ctx, n)
	}
	if !wifi {
		return n
	}
	n.WiFi, n.SSID, n.Hint = true, ssid, hint
	if n.SSID == "" && n.Hint == "" {
		n.Hint = "读不到 Wi-Fi 名称"
	}
	return n
}

// query 判断网卡是不是 Wi-Fi 并读取 SSID。err 不为空表示 WLAN API 用不了。
func (d *systemDetector) query(iface string) (wifi bool, ssid, hint string, err error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.open(); err != nil {
		return false, "", "", err
	}
	if d.wifi == nil || time.Since(d.wifiAt) > wifiListTTL {
		m, err := d.wifiInterfaces()
		if err != nil {
			d.close()
			return false, "", "", err
		}
		d.wifi, d.wifiAt = m, time.Now()
	}
	guid, ok := d.wifi[iface]
	if !ok {
		return false, "", "", nil
	}
	var size uint32
	var data *wlanConnectionAttributes
	r, _, _ := procWlanQueryInterface.Call(d.handle, uintptr(unsafe.Pointer(&guid)), wlanOpcodeCurrentConnection,
		0, uintptr(unsafe.Pointer(&size)), uintptr(unsafe.Pointer(&data)), 0)
	switch e := syscall.Errno(r); {
	case e == 0:
	case e == windows.ERROR_ACCESS_DENIED:
		return true, "", locationHint, nil
	case e == errInvalidState:
		return true, "", "", nil // 网卡没有连上 Wi-Fi
	default:
		d.close()
		return false, "", "", e
	}
	defer procWlanFreeMemory.Call(uintptr(unsafe.Pointer(data)))
	if data == nil || uintptr(size) < unsafe.Sizeof(*data) {
		return true, "", "", nil
	}
	l := min(int(data.SSIDLength), len(data.SSID))
	return true, string(data.SSID[:l]), "", nil
}

func (d *systemDetector) open() error {
	if d.handle != 0 {
		return nil
	}
	if err := wlanapi.Load(); err != nil {
		return err
	}
	var negotiated uint32
	var h uintptr
	r, _, _ := procWlanOpenHandle.Call(wlanClientVersion, 0, uintptr(unsafe.Pointer(&negotiated)), uintptr(unsafe.Pointer(&h)))
	if r != 0 {
		return syscall.Errno(r)
	}
	d.handle = h
	return nil
}

func (d *systemDetector) close() {
	if d.handle != 0 {
		procWlanCloseHandle.Call(d.handle, 0)
		d.handle = 0
	}
	d.wifi = nil
}

// wifiInterfaces 列出 WLAN 网卡，按 GUID 对上网卡名（GetAdaptersAddresses 的 FriendlyName，与 Go 的 net.Interfaces 一致）。
func (d *systemDetector) wifiInterfaces() (map[string]windows.GUID, error) {
	var list *wlanInterfaceInfoList
	r, _, _ := procWlanEnumInterfaces.Call(d.handle, 0, uintptr(unsafe.Pointer(&list)))
	if r != 0 {
		return nil, syscall.Errno(r)
	}
	defer procWlanFreeMemory.Call(uintptr(unsafe.Pointer(list)))
	guids := map[string]windows.GUID{}
	if list.NumberOfItems > 0 {
		first := (*wlanInterfaceInfo)(unsafe.Add(unsafe.Pointer(list), unsafe.Sizeof(*list)))
		for _, info := range unsafe.Slice(first, list.NumberOfItems) {
			guids[strings.ToUpper(info.GUID.String())] = info.GUID
		}
	}
	out := map[string]windows.GUID{}
	if len(guids) == 0 {
		return out, nil
	}
	adapters, err := adapterAddresses()
	if err != nil {
		return nil, err
	}
	for a := adapters; a != nil; a = a.Next {
		if g, ok := guids[strings.ToUpper(windows.BytePtrToString(a.AdapterName))]; ok {
			out[windows.UTF16PtrToString(a.FriendlyName)] = g
		}
	}
	return out, nil
}

func adapterAddresses() (*windows.IpAdapterAddresses, error) {
	const flags = windows.GAA_FLAG_SKIP_UNICAST | windows.GAA_FLAG_SKIP_ANYCAST |
		windows.GAA_FLAG_SKIP_MULTICAST | windows.GAA_FLAG_SKIP_DNS_SERVER
	size := uint32(16 << 10)
	for range 3 {
		buf := make([]byte, size)
		a := (*windows.IpAdapterAddresses)(unsafe.Pointer(&buf[0]))
		err := windows.GetAdaptersAddresses(windows.AF_UNSPEC, flags, 0, a, &size)
		if err == nil {
			return a, nil
		}
		if !errors.Is(err, windows.ERROR_BUFFER_OVERFLOW) {
			return nil, err
		}
	}
	return nil, windows.ERROR_BUFFER_OVERFLOW
}

// netshDetect 是 WLAN API 用不了时的退路。netsh 的输出用控制台代码页，先切到 UTF-8，否则中文 SSID 会乱码。
func netshDetect(ctx context.Context, n Network) Network {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	root := os.Getenv("SystemRoot")
	if root == "" {
		root = `C:\Windows`
	}
	cmd := exec.CommandContext(ctx, filepath.Join(root, "System32", "cmd.exe"), "/c", "chcp 65001 >nul & netsh wlan show interfaces")
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	out, _ := cmd.Output()
	text := string(out)
	for _, w := range parseNetshInterfaces(text) {
		if w.Name != n.Interface {
			continue
		}
		n.WiFi = true
		n.SSID = w.SSID
		if n.SSID == "" {
			n.Hint = "读不到 Wi-Fi 名称"
		}
		return n
	}
	if strings.Contains(strings.ToLower(text), "location") || strings.Contains(text, "位置") {
		n.Hint = "netsh 需要打开系统定位服务才能读取 Wi-Fi 信息"
	}
	return n
}
