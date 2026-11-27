package trust

import (
	"bufio"
	"context"
	"crypto/subtle"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// Gate 是出口闸门：只监听本机的 SOCKS5 入口，内核把受保护服务的连接交给它（每个严格组一个出站，
// 用户名就是组名）。每个新连接先问 Evaluator.GateAllow：
//
//   - 允许：转给内核的 DIRECT 入口（HTTP CONNECT），经物理网卡交给路由盒；
//   - 不允许或任何出错：转给这个组的探测入口，也就是经代理组出去。
//
// 闸门自己不直接连外网：直连由内核发出，内核会绑定物理网卡，绕开自己的 TUN。只转发 TCP；
// 受保护服务的 UDP（QUIC）在规则里已经拒绝。
type Gate struct {
	ev     *Evaluator
	secret string
	// ports 返回内核 DIRECT 入口和严格组 → 探测入口的端口；内核没在运行时 direct 为 0。
	ports func() (direct int, groups map[string]int)
	log   *slog.Logger

	// live 是经闸门直连、还开着的连接（按严格组）。放行只在建立连接时确认一次，之后判断变了
	// （出口不符、离开受信网络）要把它们断开，否则长连接（HTTP/2、WebSocket）会继续经直连出去。
	mu   sync.Mutex
	live map[string]map[*liveConn]struct{}
}

type liveConn struct{ client, up net.Conn }

func NewGate(ev *Evaluator, secret string, ports func() (int, map[string]int), log *slog.Logger) *Gate {
	g := &Gate{ev: ev, secret: secret, ports: ports, log: log, live: map[string]map[*liveConn]struct{}{}}
	if ev != nil {
		ev.gateRevoke = g.Revoke
	}
	return g
}

// Revoke 断开某个严格组经闸门直连的连接；group 为空时断开全部。返回断开的条数。
func (g *Gate) Revoke(group string) int {
	g.mu.Lock()
	var list []*liveConn
	for name, set := range g.live {
		if group != "" && name != group {
			continue
		}
		for c := range set {
			list = append(list, c)
		}
		delete(g.live, name)
	}
	g.mu.Unlock()
	for _, c := range list {
		_ = c.client.Close()
		_ = c.up.Close()
	}
	if len(list) > 0 && g.log != nil {
		g.log.Warn("闸门直连不再放行，已断开经直连的连接", "group", group, "count", len(list))
	}
	return len(list)
}

func (g *Gate) track(group string, c *liveConn) func() {
	g.mu.Lock()
	set := g.live[group]
	if set == nil {
		set = map[*liveConn]struct{}{}
		g.live[group] = set
	}
	set[c] = struct{}{}
	g.mu.Unlock()
	return func() {
		g.mu.Lock()
		delete(g.live[group], c)
		g.mu.Unlock()
	}
}

const gateHandshakeTimeout = 10 * time.Second

// Serve 在 addr 上监听，直到 ctx 结束。
func (g *Gate) Serve(ctx context.Context, addr string) error {
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
				if ctx.Err() != nil {
					return
				}
				time.Sleep(100 * time.Millisecond)
				continue
			}
			go g.handle(ctx, c)
		}
	}()
	return nil
}

func (g *Gate) handle(ctx context.Context, c net.Conn) {
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(gateHandshakeTimeout))
	r := bufio.NewReader(c)
	group, err := g.auth(r, c)
	if err != nil {
		g.debug("闸门握手失败", err)
		return
	}
	target, err := readConnect(r)
	if err != nil {
		_, _ = c.Write(socksReply(0x07))
		g.debug("闸门请求无效", err)
		return
	}

	direct, groups := g.ports()
	allowed, reason := g.ev.GateAllow(ctx, group)
	port := groups[group]
	viaDirect := allowed && direct > 0
	if viaDirect {
		port = direct
	} else if port == 0 {
		// 没有这个组的入口（内核没在运行或配置还没生效）：拒绝，绝不改成本机直连。
		_, _ = c.Write(socksReply(0x01))
		g.debug("闸门没有可用的出口", fmt.Errorf("%s：%s", group, reason))
		return
	}
	up, err := httpConnect(ctx, port, target)
	if err != nil {
		_, _ = c.Write(socksReply(0x05))
		g.debug("闸门连接上游失败", err)
		return
	}
	defer up.Close()
	if _, err := c.Write(socksReply(0x00)); err != nil {
		return
	}
	_ = c.SetDeadline(time.Time{})
	if viaDirect {
		defer g.track(group, &liveConn{client: c, up: up})()
		// 握手期间判断可能已经变了（Revoke 时这条连接还没登记）：登记后再确认一次。
		if ok, _ := g.ev.gateStillAllowed(group); !ok {
			return
		}
	}
	pipe(c, r, up)
}

func (g *Gate) debug(msg string, err error) {
	if g.log != nil {
		g.log.Debug(msg, "err", err, "source", "gate")
	}
}

// auth 只接受用户名密码认证：用户名是严格组名，密码是闸门的口令，防止本机其他程序借它直连。
func (g *Gate) auth(r *bufio.Reader, w io.Writer) (string, error) {
	head := make([]byte, 2)
	if _, err := io.ReadFull(r, head); err != nil {
		return "", err
	}
	if head[0] != 5 {
		return "", errors.New("不是 SOCKS5")
	}
	methods := make([]byte, head[1])
	if _, err := io.ReadFull(r, methods); err != nil {
		return "", err
	}
	if !containsByte(methods, 0x02) {
		_, _ = w.Write([]byte{5, 0xff})
		return "", errors.New("客户端不支持用户名密码认证")
	}
	if _, err := w.Write([]byte{5, 0x02}); err != nil {
		return "", err
	}
	ver, err := r.ReadByte()
	if err != nil || ver != 1 {
		return "", errors.New("认证请求无效")
	}
	user, err := readShort(r)
	if err != nil {
		return "", err
	}
	pass, err := readShort(r)
	if err != nil {
		return "", err
	}
	if subtle.ConstantTimeCompare([]byte(pass), []byte(g.secret)) != 1 || user == "" {
		_, _ = w.Write([]byte{1, 1})
		return "", errors.New("口令不对")
	}
	if _, err := w.Write([]byte{1, 0}); err != nil {
		return "", err
	}
	return user, nil
}

func readShort(r *bufio.Reader) (string, error) {
	n, err := r.ReadByte()
	if err != nil {
		return "", err
	}
	b := make([]byte, n)
	if _, err := io.ReadFull(r, b); err != nil {
		return "", err
	}
	return string(b), nil
}

// readConnect 读 CONNECT 请求，返回 host:port。
func readConnect(r *bufio.Reader) (string, error) {
	head := make([]byte, 4)
	if _, err := io.ReadFull(r, head); err != nil {
		return "", err
	}
	if head[0] != 5 || head[1] != 1 {
		return "", fmt.Errorf("只支持 CONNECT（命令 %d）", head[1])
	}
	var host string
	switch head[3] {
	case 1:
		b := make([]byte, 4)
		if _, err := io.ReadFull(r, b); err != nil {
			return "", err
		}
		host = net.IP(b).String()
	case 3:
		h, err := readShort(r)
		if err != nil {
			return "", err
		}
		host = h
	case 4:
		b := make([]byte, 16)
		if _, err := io.ReadFull(r, b); err != nil {
			return "", err
		}
		host = net.IP(b).String()
	default:
		return "", fmt.Errorf("地址类型 %d 无效", head[3])
	}
	pb := make([]byte, 2)
	if _, err := io.ReadFull(r, pb); err != nil {
		return "", err
	}
	return net.JoinHostPort(host, strconv.Itoa(int(binary.BigEndian.Uint16(pb)))), nil
}

func socksReply(code byte) []byte { return []byte{5, code, 0, 1, 0, 0, 0, 0, 0, 0} }

// httpConnect 经内核本机的 HTTP 入口建立隧道。
func httpConnect(ctx context.Context, port int, target string) (net.Conn, error) {
	d := net.Dialer{Timeout: 5 * time.Second}
	up, err := d.DialContext(ctx, "tcp", "127.0.0.1:"+strconv.Itoa(port))
	if err != nil {
		return nil, err
	}
	_ = up.SetDeadline(time.Now().Add(15 * time.Second))
	if _, err := fmt.Fprintf(up, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", target, target); err != nil {
		up.Close()
		return nil, err
	}
	br := bufio.NewReader(up)
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodConnect})
	if err != nil {
		up.Close()
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		up.Close()
		return nil, fmt.Errorf("上游拒绝 %s：HTTP %d", target, resp.StatusCode)
	}
	_ = up.SetDeadline(time.Time{})
	if br.Buffered() > 0 {
		return &bufferedConn{Conn: up, r: br}, nil
	}
	return up, nil
}

type bufferedConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *bufferedConn) Read(p []byte) (int, error) { return c.r.Read(p) }

// pipe 双向转发，一边读完就半关闭另一边的写，两边都结束后返回。cr 是客户端一侧已经带缓冲的读取器。
func pipe(client net.Conn, cr io.Reader, up net.Conn) {
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); _, _ = io.Copy(up, cr); closeWrite(up) }()
	go func() { defer wg.Done(); _, _ = io.Copy(client, up); closeWrite(client) }()
	wg.Wait()
}

func closeWrite(c net.Conn) {
	if b, ok := c.(*bufferedConn); ok {
		c = b.Conn
	}
	if t, ok := c.(interface{ CloseWrite() error }); ok {
		_ = t.CloseWrite()
		return
	}
	_ = c.Close()
}

func containsByte(b []byte, v byte) bool {
	for _, x := range b {
		if x == v {
			return true
		}
	}
	return false
}
