package dist

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
	"unicode/utf8"
)

// 诊断数据：客户端把出口探测结果和系统事件随同步上报，管理员在「客户端」页面排查连接问题。
//
// 收集范围以「排查连接问题」为限，不收集使用情况：
//   - 收集：出口探测结果（IP、地区、耗时、错误）、Kill Switch 状态变化、内核和 TUN 故障、
//     网络类型变化（网卡、是不是 Wi-Fi、是否受信、能否读到名称），以及上报时的状态快照。
//   - 不收集：访问的域名和连接、流量、Wi-Fi 名称、开关机和休眠、服务或应用的启动退出、
//     开关代理、切换模式和线路这类操作。
//
// 客户端只在绑定了管理服务、且没有在「服务端」页面关闭诊断上报时收集和上报。

const (
	maxDiagRecords    = 500 // 一次上报的事件上限
	maxDiagFields     = 16  // 每条事件的字段数上限
	maxDiagText       = 500 // 消息、字段值的长度上限（字符）
	maxDiagKey        = 32  // 类型、级别、字段名的长度上限
	maxDiagSnapshot   = 64 << 10
	diagKeepDays      = 30   // 服务端保留天数
	diagKeepPerClient = 5000 // 每台客户端最多保留的事件数
)

// DiagRecord 是一条诊断事件。
type DiagRecord struct {
	Seq     int64             `json:"seq"` // 客户端生成，单调递增（纳秒时间戳），服务端据此去重
	Time    time.Time         `json:"time"`
	Kind    string            `json:"kind"`  // egress、strict_egress、network、killswitch、health、core
	Level   string            `json:"level"` // info、warn、error
	Message string            `json:"message"`
	Fields  map[string]string `json:"fields,omitempty"`
}

// DiagEgress 是一次出口探测的结果。
type DiagEgress struct {
	OK        bool      `json:"ok"`
	IP        string    `json:"ip,omitempty"`
	Country   string    `json:"country,omitempty"`
	Error     string    `json:"error,omitempty"`
	LatencyMS int64     `json:"latency_ms,omitempty"`
	CheckedAt time.Time `json:"checked_at"`
}

// DiagSnapshot 是上报时的状态。原因文字里的 Wi-Fi 名称已去掉。
type DiagSnapshot struct {
	Time        time.Time `json:"time"`
	KillSwitch  string    `json:"killswitch"` // killswitch 的状态
	Path        string    `json:"path"`
	Reason      string    `json:"reason,omitempty"`
	DNSOK       bool      `json:"dns_ok"`
	TUNOK       bool      `json:"tun_ok"`
	CoreRunning bool      `json:"core_running"`
	CoreError   string    `json:"core_error,omitempty"`
	TUNError    string    `json:"tun_error,omitempty"`
	Network     DiagNet   `json:"network"`
	// Egress 是受信 Wi-Fi 上最近一次出口探测；不在受信 Wi-Fi 上时为空。
	Egress *DiagEgress  `json:"egress,omitempty"`
	Strict []DiagStrict `json:"strict,omitempty"`
}

// DiagNet 描述当前网络，不含 Wi-Fi 名称。
type DiagNet struct {
	Interface    string `json:"interface"`
	WiFi         bool   `json:"wifi"`
	Trusted      bool   `json:"trusted"`
	SSIDReadable bool   `json:"ssid_readable"`
	Hint         string `json:"hint,omitempty"`
}

// DiagStrict 是一个严格组的出口。
type DiagStrict struct {
	Group   string     `json:"group"`
	Allowed bool       `json:"allowed"`
	Egress  DiagEgress `json:"egress"`
}

// DiagReport 是一次上报。
type DiagReport struct {
	Snapshot DiagSnapshot `json:"snapshot"`
	Records  []DiagRecord `json:"records"`
}

// ---- 客户端 ----

// Report 上报诊断数据。没有绑定时什么也不做，返回 false。
func (c *Client) Report(ctx context.Context, r DiagReport) (bool, error) {
	c.mu.Lock()
	b := c.binding
	c.mu.Unlock()
	if b == nil {
		return false, nil
	}
	body, err := json.Marshal(r)
	if err != nil {
		return false, err
	}
	resp, err := c.doMethod(ctx, *b, http.MethodPost, "/api/v1/client/diagnostics", bytes.NewReader(body), func(req *http.Request) {
		req.Header.Set("Content-Type", "application/json")
	})
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false, responseError(resp)
	}
	return true, nil
}

// ---- 服务端 ----

func validKey(s string) bool {
	if s == "" || len(s) > maxDiagKey {
		return false
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '_') {
			return false
		}
	}
	return true
}

func clip(s string) string {
	if utf8.RuneCountInString(s) <= maxDiagText {
		return s
	}
	return string([]rune(s)[:maxDiagText]) + "…"
}

func (r DiagReport) validate() error {
	if len(r.Records) > maxDiagRecords {
		return &InputError{fmt.Sprintf("一次最多上报 %d 条事件", maxDiagRecords)}
	}
	for i, rec := range r.Records {
		if rec.Seq <= 0 || rec.Time.IsZero() || !validKey(rec.Kind) || !validKey(rec.Level) {
			return &InputError{fmt.Sprintf("第 %d 条事件无效", i+1)}
		}
		if len(rec.Fields) > maxDiagFields {
			return &InputError{fmt.Sprintf("第 %d 条事件的字段过多", i+1)}
		}
		for k := range rec.Fields {
			if !validKey(k) {
				return &InputError{fmt.Sprintf("第 %d 条事件的字段名 %q 无效", i+1, k)}
			}
		}
	}
	return nil
}

// StoredDiagRecord 是服务端保存的一条事件。
type StoredDiagRecord struct {
	DiagRecord
	ID         int64  `json:"id"`
	ReceivedAt string `json:"received_at"`
}

// DeviceDiagnostics 是管理页面看到的诊断数据。
type DeviceDiagnostics struct {
	Snapshot   *DiagSnapshot      `json:"snapshot"`
	SnapshotAt string             `json:"snapshot_at"`
	Records    []StoredDiagRecord `json:"records"`
}
