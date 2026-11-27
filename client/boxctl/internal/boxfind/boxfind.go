// Package boxfind 是客户端发现路由盒的协议（ADR-073），路由盒和桌面客户端共用。
//
// 路由盒的 dnsmasq 把 Name 解析到自己的 LAN 地址，面板在 LAN 地址的 Port 上监听。客户端换网络后：
//
//  1. 直接问物理网卡的 DNS 服务器 Name 的 A 记录（不经系统解析：本机代理的 TUN 开着时系统 DNS 会拿到 fake-ip），
//     只接受私有地址；
//  2. 连这个地址的 Port，发一个随机数；路由盒返回自己的 ID 和 HMAC-SHA256(口令, 随机数 + ID)；
//  3. 客户端用同一个口令核对。口令不在网络上传输，冒充的服务器拿不到它。
//
// 名字用 OpenWrt / iStoreOS 默认的本地域 lan（dnsmasq 本地应答），不用 .local：macOS、Windows 只用组播（mDNS）
// 查 .local，问不到 dnsmasq。早期版本用 banbu-box.home.arpa，客户端查不到新名字时再查一次旧名字（LegacyName）。
// 口令是所有路由盒和客户端共用的（用户选择的简化方案）：任何一台客户端的口令泄漏后可以冒充路由盒，
// 路由盒接管后客户端不再确认出口（ADR-082）；客户端还要在路由盒的代理入口上认证通过（VerifyProxy，ADR-084）。
package boxfind

import (
	"bufio"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	// Name 是路由盒在局域网 DNS 里的名字。
	Name = "banbu-box.lan"
	// LegacyName 是早期版本路由盒用的名字，客户端兼容还没升级的路由盒。
	LegacyName = "banbu-box.home.arpa"
	// Port 是路由盒的发现端口（TCP，只监听 LAN 地址）。
	Port = 19443

	proto   = "BANBU-BOX1"
	timeout = 2 * time.Second
)

// Box 是验证通过的路由盒。
type Box struct {
	ID   string         `json:"id"`
	Addr netip.AddrPort `json:"addr"`
}

var idRE = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

// ValidID 判断路由盒 ID 是否合法（字母、数字、点、下划线、连字符，最多 64 个字符）。
func ValidID(id string) bool { return idRE.MatchString(id) }

func sign(secret, nonce, id string) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte(proto + "|" + nonce + "|" + id))
	return hex.EncodeToString(m.Sum(nil))
}

// ---- 路由盒端 ----

// Recorder 记下来找路由盒的客户端（设备页显示用）：Seen 是一次挑战应答（旧版客户端只有这个），
// Hello 是客户端验证通过后报来的名字、系统和版本。可以为 nil。
type Recorder interface {
	Seen(ip netip.Addr)
	Hello(ip netip.Addr, h Hello)
}

// Serve 在 addr（TCP）上应答发现请求，直到 ctx 结束。secret 每次连接时读取，为空时不应答（直接关闭），
// 这样口令随管理服务下发更新后不用重启。rec 不为 nil 时记下来访的客户端。
func Serve(ctx context.Context, addr, id string, secret func() string, log *slog.Logger, rec Recorder) error {
	if !ValidID(id) {
		return fmt.Errorf("路由盒 ID 不合法：%q", id)
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	go func() {
		<-ctx.Done()
		ln.Close()
	}()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				if ctx.Err() == nil {
					log.Warn("路由盒发现端口停止", "err", err)
				}
				return
			}
			go answer(c, id, secret(), rec)
		}
	}()
	return nil
}

func answer(c net.Conn, id, secret string, rec Recorder) {
	defer c.Close()
	if secret == "" {
		return
	}
	_ = c.SetDeadline(time.Now().Add(timeout))
	line, err := bufio.NewReader(&limitedReader{c, maxHelloLine}).ReadString('\n')
	if err != nil {
		return
	}
	p, rest, ok := strings.Cut(strings.TrimSpace(line), " ")
	if !ok {
		return
	}
	var src netip.Addr
	if ap, err := netip.ParseAddrPort(c.RemoteAddr().String()); err == nil {
		src = ap.Addr().Unmap()
	}
	switch p {
	case proto:
		if len(rest) != 32 {
			return
		}
		if _, err := hex.DecodeString(rest); err != nil {
			return
		}
		fmt.Fprintf(c, "%s %s %s\n", proto, id, sign(secret, rest, id))
		if rec != nil && src.IsValid() {
			rec.Seen(src)
		}
	case helloProto:
		h, err := parseHello(secret, rest)
		if err != nil {
			return
		}
		fmt.Fprintf(c, "%s ok\n", helloProto)
		if rec != nil && src.IsValid() {
			rec.Hello(src, h)
		}
	}
}

type limitedReader struct {
	c net.Conn
	n int
}

func (r *limitedReader) Read(p []byte) (int, error) {
	if r.n <= 0 {
		return 0, errors.New("请求太长")
	}
	if len(p) > r.n {
		p = p[:r.n]
	}
	n, err := r.c.Read(p)
	r.n -= n
	return n, err
}

// ---- 客户端 ----

// ErrNotFound 表示这个网络上没有路由盒（名字解析不到）。
var ErrNotFound = errors.New("没有找到路由盒")

// Discover 问 servers（物理网卡的 DNS 服务器）Name 的地址，逐个验证，返回第一个通过的路由盒。
// 解析不到时返回 ErrNotFound；解析到了但验证不过时返回具体原因。
func Discover(ctx context.Context, servers []netip.Addr, secret string) (Box, error) {
	if secret == "" {
		return Box{}, errors.New("没有设置路由盒口令")
	}
	tr := traceFrom(ctx)
	addrs := resolve(ctx, servers, tr)
	if len(addrs) == 0 {
		return Box{}, ErrNotFound
	}
	var last error
	for _, a := range addrs {
		ap := netip.AddrPortFrom(a, Port)
		b, err := Verify(ctx, ap, secret)
		if tr != nil {
			at := Attempt{Addr: ap.String(), OK: err == nil}
			if err != nil {
				at.Error = err.Error()
			}
			tr.Attempts = append(tr.Attempts, at)
		}
		if err == nil {
			return b, nil
		}
		last = err
	}
	return Box{}, last
}

// Trace 记录一次 Discover 的过程（问了哪些 DNS、解析到什么、逐个验证的结果），展示用。
type Trace struct {
	Lookups  []Lookup  `json:"lookups"`
	Attempts []Attempt `json:"attempts"`
}

// Lookup 是向一个 DNS 服务器查 Name 的结果。Addrs 只含接受的私有 IPv4 地址，Ignored 是被丢弃的公网地址。
type Lookup struct {
	Server  string   `json:"server"`
	Name    string   `json:"name"`
	Addrs   []string `json:"addrs,omitempty"`
	Ignored []string `json:"ignored,omitempty"`
	Error   string   `json:"error,omitempty"`
}

// Attempt 是对一个地址做挑战应答的结果。
type Attempt struct {
	Addr  string `json:"addr"`
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

type traceKey struct{}

// WithTrace 让 ctx 上的 Discover 把过程记进 t。
func WithTrace(ctx context.Context, t *Trace) context.Context {
	return context.WithValue(ctx, traceKey{}, t)
}

func traceFrom(ctx context.Context) *Trace {
	t, _ := ctx.Value(traceKey{}).(*Trace)
	return t
}

// resolve 先查 Name，查不到再查 LegacyName；resolveName 依次问每个 DNS 服务器，返回第一个有结果的服务器给出的私有 IPv4 地址。
// 公网地址不接受：路由盒总在局域网里，指向公网的应答一定不是它。
func resolve(ctx context.Context, servers []netip.Addr, tr *Trace) []netip.Addr {
	for _, name := range []string{Name, LegacyName} {
		if out := resolveName(ctx, servers, name, tr); len(out) > 0 {
			return out
		}
	}
	return nil
}

func resolveName(ctx context.Context, servers []netip.Addr, name string, tr *Trace) []netip.Addr {
	for _, s := range servers {
		server := netip.AddrPortFrom(s.Unmap(), 53).String()
		r := &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, network, server)
		}}
		qctx, cancel := context.WithTimeout(ctx, timeout)
		ips, err := r.LookupNetIP(qctx, "ip4", name)
		cancel()
		lk := Lookup{Server: server, Name: name}
		if err != nil {
			lk.Error = err.Error()
			if tr != nil {
				tr.Lookups = append(tr.Lookups, lk)
			}
			continue
		}
		var out []netip.Addr
		for _, ip := range ips {
			if ip = ip.Unmap(); ip.IsPrivate() {
				out = append(out, ip)
				lk.Addrs = append(lk.Addrs, ip.String())
			} else {
				lk.Ignored = append(lk.Ignored, ip.String())
			}
		}
		if tr != nil {
			tr.Lookups = append(tr.Lookups, lk)
		}
		if len(out) > 0 {
			return out
		}
	}
	return nil
}

// Verify 连 addr 做一次挑战应答，返回路由盒的 ID。
func Verify(ctx context.Context, addr netip.AddrPort, secret string) (Box, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var d net.Dialer
	c, err := d.DialContext(ctx, "tcp", addr.String())
	if err != nil {
		return Box{}, fmt.Errorf("连不上 %s：%w", addr, err)
	}
	defer c.Close()
	if dl, ok := ctx.Deadline(); ok {
		_ = c.SetDeadline(dl)
	}
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	nonce := hex.EncodeToString(b)
	if _, err := fmt.Fprintf(c, "%s %s\n", proto, nonce); err != nil {
		return Box{}, err
	}
	line, err := bufio.NewReader(&limitedReader{c, 256}).ReadString('\n')
	if err != nil {
		return Box{}, fmt.Errorf("%s 没有应答（口令没设置或不是路由盒）", addr)
	}
	f := strings.Fields(line)
	if len(f) != 3 || f[0] != proto || !ValidID(f[1]) {
		return Box{}, fmt.Errorf("%s 的应答格式不对", addr)
	}
	if !hmac.Equal([]byte(f[2]), []byte(sign(secret, nonce, f[1]))) {
		return Box{}, fmt.Errorf("%s 的口令验证不通过", addr)
	}
	return Box{ID: f[1], Addr: addr}, nil
}

// DefaultID 是路由盒没有设置 ID 时用的：主机名里的合法字符。
func DefaultID(hostname string) string {
	var b strings.Builder
	for _, r := range hostname {
		if r < 128 && (r == '.' || r == '_' || r == '-' || r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z') {
			b.WriteRune(r)
		}
	}
	id := b.String()
	if len(id) > 64 {
		id = id[:64]
	}
	if id == "" {
		id = "box"
	}
	return id
}

// ListenAddr 拼出发现端口的监听地址。
func ListenAddr(ip string) string { return net.JoinHostPort(ip, strconv.Itoa(Port)) }
