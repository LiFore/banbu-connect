package trust

import (
	"context"
	"encoding/binary"
	"io"
	"net"
	"time"
)

// 阻断时受保护的域名交给这里解析，一律回答 REFUSED，应用立即失败而不是等超时。
// 服务不在时端口关闭，解析同样失败：阻断不依赖本进程存活。
//
// TODO(verify:V28) macOS 的 mDNSResponder 收到 REFUSED 后是否立即失败，而不是改问其他解析器。

const rcodeRefused = 5

// refuse 把查询改写成 REFUSED 应答：保留 ID、操作码、RD 和问题段，去掉其余段。
func refuse(q []byte) []byte {
	if len(q) < 12 {
		return nil
	}
	if q[2]&0x80 != 0 { // 已经是应答，不理会
		return nil
	}
	end := 12
	if binary.BigEndian.Uint16(q[4:6]) == 1 {
		// 跳过 QNAME 和 QTYPE、QCLASS；解析失败就不带问题段。
		i := 12
		for i < len(q) && q[i] != 0 {
			if q[i]&0xC0 != 0 {
				i = len(q)
				break
			}
			i += int(q[i]) + 1
		}
		if i+5 <= len(q) {
			end = i + 5
		}
	}
	r := make([]byte, end)
	copy(r, q[:end])
	r[2] = 0x80 | (q[2] & 0x79) // QR=1，保留 Opcode 和 RD
	r[3] = 0x80 | rcodeRefused  // RA=1
	qd := uint16(0)
	if end > 12 {
		qd = 1
	}
	binary.BigEndian.PutUint16(r[4:6], qd)
	binary.BigEndian.PutUint16(r[6:8], 0)
	binary.BigEndian.PutUint16(r[8:10], 0)
	binary.BigEndian.PutUint16(r[10:12], 0)
	return r
}

// ServeHold 在 addr 上同时监听 UDP 和 TCP，直到 ctx 结束。
func ServeHold(ctx context.Context, addr string) error {
	pc, err := net.ListenPacket("udp", addr)
	if err != nil {
		return err
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		pc.Close()
		return err
	}
	go func() {
		<-ctx.Done()
		pc.Close()
		ln.Close()
	}()
	go func() {
		buf := make([]byte, 1500)
		for {
			n, from, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			if r := refuse(buf[:n]); r != nil {
				_, _ = pc.WriteTo(r, from)
			}
		}
	}()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go serveTCP(c)
		}
	}()
	return nil
}

func serveTCP(c net.Conn) {
	defer c.Close()
	for {
		_ = c.SetDeadline(time.Now().Add(5 * time.Second))
		var l [2]byte
		if _, err := io.ReadFull(c, l[:]); err != nil {
			return
		}
		q := make([]byte, binary.BigEndian.Uint16(l[:]))
		if _, err := io.ReadFull(c, q); err != nil {
			return
		}
		r := refuse(q)
		if r == nil {
			return
		}
		out := make([]byte, 2+len(r))
		binary.BigEndian.PutUint16(out, uint16(len(r)))
		copy(out[2:], r)
		if _, err := c.Write(out); err != nil {
			return
		}
	}
}
