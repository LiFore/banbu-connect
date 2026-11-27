package dist

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// ---- 自建邮箱收信（IMAP，如 BillionMail）----
//
// 账号用自己域名的邮箱注册时（邮箱服务器是自建的 BillionMail、Mailcow 等），管理服务直接用 IMAP 登录这个邮箱读登录邮件，
// 不需要 Gmail 那样的 OAuth 授权。IMAP 服务器统一设置（settings 表 mail_imap_server），每个账号只存登录名和密码（加密）。
// 只读：用 EXAMINE 打开文件夹、BODY.PEEK 取正文，不改已读状态，也不删邮件。
// 这里只实现用到的几条命令（LOGIN / EXAMINE / UID SEARCH / UID FETCH / LOGOUT），不引入完整的 IMAP 库，
// 免得路由盒上的二进制变大。

const (
	mailServerKey   = "mail_imap_server"
	mailPasswordAAD = "claude_accounts.mail_password:"
	imapTimeout     = 30 * time.Second
)

// MailServer 是自建邮箱的 IMAP 服务器。Folders 是要搜的文件夹，逗号分隔，空表示 INBOX 和 Junk
// （BillionMail 的 Rspamd 可能把登录邮件判成垃圾邮件）；不存在的文件夹跳过。
type MailServer struct {
	Host       string `json:"host"`
	Port       int    `json:"port"`
	Security   string `json:"security"` // tls（993）、starttls（143）、none（只用于内网测试）
	SkipVerify bool   `json:"skip_verify"`
	Folders    string `json:"folders"`
}

func (m MailServer) folders() []string {
	var out []string
	for f := range strings.SplitSeq(m.Folders, ",") {
		if f = strings.TrimSpace(f); f != "" && !slices.Contains(out, f) {
			out = append(out, f)
		}
	}
	if len(out) == 0 {
		out = []string{"INBOX", "Junk"}
	}
	return out
}

func (s *Server) MailServer(ctx context.Context) (MailServer, error) {
	var m MailServer
	err := s.loadSetting(ctx, mailServerKey, &m)
	return m, err
}

// SetMailServer 保存 IMAP 服务器。Host 为空表示不用自建邮箱。
func (s *Server) SetMailServer(ctx context.Context, m MailServer) (MailServer, error) {
	m.Host = strings.TrimSpace(m.Host)
	m.Folders = strings.Join(MailServer{Folders: m.Folders}.folders(), ",")
	if m.Security == "" {
		m.Security = "tls"
	}
	if m.Security != "tls" && m.Security != "starttls" && m.Security != "none" {
		return m, &InputError{"加密方式只能是 tls、starttls 或 none"}
	}
	if m.Port == 0 {
		m.Port = 993
		if m.Security != "tls" {
			m.Port = 143
		}
	}
	if m.Port < 1 || m.Port > 65535 {
		return m, &InputError{"端口不对"}
	}
	if m.Host != "" && (strings.ContainsAny(m.Host, " /:") && net.ParseIP(m.Host) == nil) {
		return m, &InputError{"服务器只填主机名或 IP，不带协议和端口"}
	}
	if err := s.saveSetting(ctx, mailServerKey, m); err != nil {
		return m, err
	}
	return m, nil
}

// imapLogin 是一个账号的 IMAP 登录信息。
type imapLogin struct {
	server   MailServer
	user     string
	password string
}

// mailboxLogin 读出账号的 IMAP 登录信息；没设置 IMAP 密码时 ok 为 false。
func (s *Server) mailboxLogin(ctx context.Context, accountID int64) (imapLogin, bool, error) {
	var email, user string
	var sealed []byte
	if err := s.DB.QueryRowContext(ctx, `SELECT email, mail_user, mail_password FROM claude_accounts WHERE id = ?`, accountID).
		Scan(&email, &user, &sealed); err != nil {
		return imapLogin{}, false, err
	}
	if len(sealed) == 0 {
		return imapLogin{}, false, nil
	}
	pw, err := s.Secrets.Open(sealed, mailPasswordAAD+strconv.FormatInt(accountID, 10))
	if err != nil {
		return imapLogin{}, false, fmt.Errorf("解密邮箱密码失败：%w", err)
	}
	srv, err := s.MailServer(ctx)
	if err != nil {
		return imapLogin{}, false, err
	}
	if srv.Host == "" {
		return imapLogin{}, false, &MailAuthError{"还没有设置自建邮箱的 IMAP 服务器（AI 账号 → 设置 → 自建邮箱）"}
	}
	if user == "" {
		user = email
	}
	return imapLogin{server: srv, user: user, password: string(pw)}, true, nil
}

// SetMailbox 保存账号的 IMAP 登录名和密码；password 为空表示只改登录名。
func (s *Server) setMailbox(ctx context.Context, id int64, user, password string) error {
	user = strings.TrimSpace(user)
	if strings.ContainsAny(user+password, "\r\n\"\\") || !isASCII(user+password) {
		return &InputError{"邮箱登录名和密码只能是 ASCII 字符，且不能含引号、反斜杠和换行"}
	}
	if _, err := s.DB.ExecContext(ctx, `UPDATE claude_accounts SET mail_user = ? WHERE id = ?`, user, id); err != nil {
		return err
	}
	if password == "" {
		return nil
	}
	sealed, err := s.Secrets.Seal([]byte(password), mailPasswordAAD+strconv.FormatInt(id, 10))
	if err != nil {
		return err
	}
	_, err = s.DB.ExecContext(ctx, `UPDATE claude_accounts SET mail_password = ?, last_error = '' WHERE id = ?`, sealed, id)
	return err
}

// ClearMailbox 删除账号的 IMAP 密码（之后改用 Gmail 授权，没有时不能读邮件）。
func (s *Server) ClearMailbox(ctx context.Context, id int64) (ClaudeAccount, error) {
	res, err := s.DB.ExecContext(ctx, `UPDATE claude_accounts SET mail_user = '', mail_password = x'' WHERE id = ?`, id)
	if err != nil {
		return ClaudeAccount{}, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ClaudeAccount{}, ErrNotFound
	}
	s.geminiChanged()
	return s.claudeAccount(ctx, id)
}

func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < 0x20 || s[i] > 0x7e {
			return false
		}
	}
	return true
}

// listImapRaw 返回 sender 最近 mailListDays 天发来的邮件（最多 mailListMax 封），新的在前。
func (s *Server) listImapRaw(ctx context.Context, l imapLogin, sender string, since time.Time) ([]RawMail, error) {
	c, err := dialIMAP(ctx, l)
	if err != nil {
		return nil, err
	}
	defer c.close()
	var out []RawMail
	for _, folder := range l.server.folders() {
		if _, err := c.cmd("EXAMINE " + imapQuote(folder)); err != nil {
			var no *imapNoError
			if errors.As(err, &no) {
				continue // 文件夹不存在
			}
			return nil, err
		}
		// SINCE 只按日期（服务器时区），往前多算一天，到达时间在下面按 INTERNALDATE 精确过滤。
		day := since.Add(-24 * time.Hour).Format("2-Jan-2006")
		resp, err := c.cmd(fmt.Sprintf("UID SEARCH SINCE %s FROM %s", day, imapQuote(sender)))
		if err != nil {
			return nil, err
		}
		uids := searchUIDs(resp)
		if len(uids) > mailListMax {
			uids = uids[len(uids)-mailListMax:]
		}
		if len(uids) == 0 {
			continue
		}
		set := make([]string, len(uids))
		for i, u := range uids {
			set[i] = strconv.FormatUint(uint64(u), 10)
		}
		resp, err = c.cmd("UID FETCH " + strings.Join(set, ",") + " (UID INTERNALDATE BODY.PEEK[])")
		if err != nil {
			return nil, err
		}
		for _, r := range resp {
			m, ok := fetchedMail(r)
			if !ok || (!m.ReceivedAt.IsZero() && m.ReceivedAt.Before(since)) {
				continue
			}
			m.ID = folder + ":" + m.ID
			out = append(out, m)
		}
	}
	_, _ = c.cmd("LOGOUT")
	slices.SortFunc(out, func(a, b RawMail) int { return b.ReceivedAt.Compare(a.ReceivedAt) })
	if len(out) > mailListMax {
		out = out[:mailListMax]
	}
	return out, nil
}

// testMailbox 用 IMAP 登录一次，检查服务器和密码。
func testMailbox(ctx context.Context, l imapLogin) error {
	c, err := dialIMAP(ctx, l)
	if err != nil {
		return err
	}
	defer c.close()
	if _, err := c.cmd("EXAMINE INBOX"); err != nil {
		return err
	}
	_, _ = c.cmd("LOGOUT")
	return nil
}

// ---- 最小 IMAP 客户端 ----

type imapConn struct {
	conn net.Conn
	r    *bufio.Reader
	tag  int
}

// imapResp 是一条非标记的响应：Text 是去掉字面量后的文本，Literals 是按顺序出现的字面量。
type imapResp struct {
	Text     string
	Literals [][]byte
}

// imapNoError 是服务器回了 NO / BAD。
type imapNoError struct{ msg string }

func (e *imapNoError) Error() string { return e.msg }

func dialIMAP(ctx context.Context, l imapLogin) (*imapConn, error) {
	ctx, cancel := context.WithTimeout(ctx, imapTimeout)
	defer cancel()
	addr := net.JoinHostPort(l.server.Host, strconv.Itoa(l.server.Port))
	tlsConf := &tls.Config{ServerName: l.server.Host, InsecureSkipVerify: l.server.SkipVerify, MinVersion: tls.VersionTLS12} //nolint:gosec // 自签名证书时由管理员选择跳过校验
	var conn net.Conn
	var err error
	if l.server.Security == "tls" {
		conn, err = (&tls.Dialer{Config: tlsConf}).DialContext(ctx, "tcp", addr)
	} else {
		conn, err = (&net.Dialer{}).DialContext(ctx, "tcp", addr)
	}
	if err != nil {
		return nil, fmt.Errorf("连接邮箱服务器 %s：%w", addr, err)
	}
	c := &imapConn{conn: conn, r: bufio.NewReader(conn)}
	_ = conn.SetDeadline(time.Now().Add(imapTimeout))
	if _, err := c.readResp(); err != nil { // 问候语
		c.close()
		return nil, fmt.Errorf("邮箱服务器没有响应：%w", err)
	}
	if l.server.Security == "starttls" {
		if _, err := c.cmd("STARTTLS"); err != nil {
			c.close()
			return nil, fmt.Errorf("STARTTLS：%w", err)
		}
		tc := tls.Client(conn, tlsConf)
		if err := tc.HandshakeContext(ctx); err != nil {
			c.close()
			return nil, fmt.Errorf("STARTTLS：%w", err)
		}
		c.conn, c.r = tc, bufio.NewReader(tc)
		_ = tc.SetDeadline(time.Now().Add(imapTimeout))
	}
	if _, err := c.cmd("LOGIN " + imapQuote(l.user) + " " + imapQuote(l.password)); err != nil {
		c.close()
		var no *imapNoError
		if errors.As(err, &no) {
			return nil, &MailAuthError{"邮箱登录失败，检查登录名和密码（" + no.msg + "）"}
		}
		return nil, err
	}
	return c, nil
}

func (c *imapConn) close() { _ = c.conn.Close() }

func imapQuote(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}

// cmd 发送一条命令，返回它的非标记响应；标记响应不是 OK 时返回 *imapNoError。
func (c *imapConn) cmd(command string) ([]imapResp, error) {
	c.tag++
	tag := "b" + strconv.Itoa(c.tag)
	if _, err := io.WriteString(c.conn, tag+" "+command+"\r\n"); err != nil {
		return nil, err
	}
	var out []imapResp
	for {
		r, err := c.readResp()
		if err != nil {
			return out, err
		}
		if rest, ok := strings.CutPrefix(r.Text, tag+" "); ok {
			status, msg, _ := strings.Cut(rest, " ")
			if strings.EqualFold(status, "OK") {
				return out, nil
			}
			name, _, _ := strings.Cut(command, " ")
			if name == "LOGIN" {
				command = "LOGIN"
			}
			return out, &imapNoError{fmt.Sprintf("%s：%s %s", command, status, msg)}
		}
		out = append(out, r)
	}
}

// literalRE 匹配行尾的字面量长度 {123}。
var literalRE = regexp.MustCompile(`\{(\d+)\}$`)

// readResp 读一条完整的响应（含行内的字面量）。超过 maxMailMessage 的字面量读掉丢弃。
func (c *imapConn) readResp() (imapResp, error) {
	var r imapResp
	var text strings.Builder
	for {
		line, err := c.r.ReadString('\n')
		if err != nil {
			return r, err
		}
		if text.Len()+len(line) > 1<<20 {
			return r, errors.New("IMAP 响应行太长")
		}
		line = strings.TrimRight(line, "\r\n")
		m := literalRE.FindStringSubmatch(line)
		if m == nil {
			text.WriteString(line)
			r.Text = text.String()
			return r, nil
		}
		n, _ := strconv.ParseInt(m[1], 10, 64)
		text.WriteString(line[:len(line)-len(m[0])])
		text.WriteString("{}")
		if n > maxMailMessage {
			if _, err := io.CopyN(io.Discard, c.r, n); err != nil {
				return r, err
			}
			r.Literals = append(r.Literals, nil)
			continue
		}
		buf := make([]byte, n)
		if _, err := io.ReadFull(c.r, buf); err != nil {
			return r, err
		}
		r.Literals = append(r.Literals, buf)
	}
}

// searchUIDs 取 UID SEARCH 的结果，从小到大。
func searchUIDs(resp []imapResp) []uint32 {
	var out []uint32
	for _, r := range resp {
		rest, ok := strings.CutPrefix(r.Text, "* SEARCH")
		if !ok {
			continue
		}
		for f := range strings.FieldsSeq(rest) {
			if n, err := strconv.ParseUint(f, 10, 32); err == nil {
				out = append(out, uint32(n))
			}
		}
	}
	slices.Sort(out)
	return out
}

var (
	fetchUIDRE  = regexp.MustCompile(`\bUID (\d+)`)
	fetchDateRE = regexp.MustCompile(`\bINTERNALDATE "([^"]+)"`)
)

// fetchedMail 从一条 FETCH 响应里取 UID、到达时间和邮件原文。
func fetchedMail(r imapResp) (RawMail, bool) {
	if !strings.HasPrefix(r.Text, "* ") || !strings.Contains(r.Text, " FETCH ") || len(r.Literals) == 0 || r.Literals[0] == nil {
		return RawMail{}, false
	}
	m := RawMail{Raw: r.Literals[0]}
	if u := fetchUIDRE.FindStringSubmatch(r.Text); u != nil {
		m.ID = u[1]
	}
	if d := fetchDateRE.FindStringSubmatch(r.Text); d != nil {
		if t, err := time.Parse("_2-Jan-2006 15:04:05 -0700", d[1]); err == nil {
			m.ReceivedAt = t.UTC()
		}
	}
	return m, m.ID != ""
}
