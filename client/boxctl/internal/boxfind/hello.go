package boxfind

import (
	"bufio"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strings"
	"unicode/utf8"
)

// 客户端自报（设备页显示接入的客户端）：客户端验证通过路由盒后，另开一个连接报自己的名字、系统和版本，
// 路由盒只拿来显示。一行：helloProto <base64url(JSON)> <HMAC-SHA256(口令, helloProto|JSON)>，
// 用同一个口令签名，局域网里没有口令的设备冒充不了别的客户端。
// 旧版路由盒不认识这一行，直接关掉连接；客户端忽略失败。

const (
	helloProto = "BANBU-HELLO1"
	// maxHelloLine 是一行请求的上限（挑战请求只有几十个字节）。
	maxHelloLine = 1024
	maxHelloText = 64 // 名字、系统、版本各自的上限（字符）
)

// Hello 是客户端自报的信息，只用于显示。
type Hello struct {
	Name    string `json:"name"`    // 电脑名
	OS      string `json:"os"`      // 系统和版本，如 macOS 15.2
	Version string `json:"version"` // Banbu Connect 版本
}

func helloSign(secret, payload string) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte(helloProto + "|" + payload))
	return hex.EncodeToString(m.Sum(nil))
}

// clean 去掉控制字符、截断过长的文字：这些值来自局域网，最后会显示在页面上。
func clean(s string) string {
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, strings.TrimSpace(s))
	if utf8.RuneCountInString(s) > maxHelloText {
		s = string([]rune(s)[:maxHelloText])
	}
	return s
}

func (h Hello) cleaned() Hello {
	return Hello{Name: clean(h.Name), OS: clean(h.OS), Version: clean(h.Version)}
}

// parseHello 核对签名并解出 Hello；rest 是 helloProto 后面的部分。
func parseHello(secret, rest string) (Hello, error) {
	enc, mac, ok := strings.Cut(rest, " ")
	if !ok {
		return Hello{}, errors.New("格式不对")
	}
	raw, err := base64.RawURLEncoding.DecodeString(enc)
	if err != nil {
		return Hello{}, err
	}
	if !hmac.Equal([]byte(mac), []byte(helloSign(secret, string(raw)))) {
		return Hello{}, errors.New("签名不对")
	}
	var h Hello
	if err := json.Unmarshal(raw, &h); err != nil {
		return Hello{}, err
	}
	return h.cleaned(), nil
}

// SendHello 向路由盒 addr（发现端口）报客户端信息。旧版路由盒不认识，返回错误，调用方忽略即可。
func SendHello(ctx context.Context, addr netip.AddrPort, secret string, h Hello) error {
	if secret == "" {
		return errors.New("没有设置路由盒口令")
	}
	raw, _ := json.Marshal(h.cleaned())
	line := fmt.Sprintf("%s %s %s\n", helloProto, base64.RawURLEncoding.EncodeToString(raw), helloSign(secret, string(raw)))
	if len(line) > maxHelloLine {
		return errors.New("客户端信息太长")
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var d net.Dialer
	c, err := d.DialContext(ctx, "tcp", addr.String())
	if err != nil {
		return err
	}
	defer c.Close()
	if dl, ok := ctx.Deadline(); ok {
		_ = c.SetDeadline(dl)
	}
	if _, err := c.Write([]byte(line)); err != nil {
		return err
	}
	resp, err := bufio.NewReader(&limitedReader{c, 64}).ReadString('\n')
	if err != nil {
		return fmt.Errorf("路由盒没有确认（版本太旧？）：%w", err)
	}
	if strings.TrimSpace(resp) != helloProto+" ok" {
		return errors.New("路由盒的回复不对")
	}
	return nil
}
