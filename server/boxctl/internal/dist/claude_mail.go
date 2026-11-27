package dist

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"html"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/http"
	"net/mail"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// ---- 查看 Claude 账号的邮件 ----
//
// 客户端可以查看账号 Gmail 里 Anthropic 发来的邮件（登录邮件、验证码、通知），不用打开 Gmail。
// 只取发件人是 anthropic.com（含子域名）的邮件，Gmail 里的其他邮件不会离开管理服务。
// 只返回纯文本：HTML 转成文字，不在客户端渲染邮件里的 HTML。
// 不强制走一键登录流程：自己手动登录的用户也能在这里看到登录邮件里的 magic link，复制或让托盘在浏览器打开。
// 注意 magic link 等同于登录凭据：被允许使用这个账号的客户端，在 7 天内都能看到。

const (
	claudeMailSender = "anthropic.com"
	codexMailSender  = "openai.com" // 登录验证码来自 noreply@tm.openai.com、otp@tm1.openai.com
	mailListMax      = 10
	mailListDays     = 7
	mailTextMax      = 20000 // 每封正文最多返回的字符数
)

// ClaudeMail 是一封邮件，正文为纯文本。
type ClaudeMail struct {
	ID      string    `json:"id"`
	From    string    `json:"from"`
	Subject string    `json:"subject"`
	Date    time.Time `json:"date"`
	Text    string    `json:"text"`
	Links   []string  `json:"links"` // 正文里的 magic link，单独列出方便打开和复制
	Codes   []string  `json:"codes"` // Codex 账号：正文里的 6 位验证码（BXC-F0-045）
}

// codeRE 匹配 OpenAI 登录邮件里的 6 位验证码（前后不能紧挨数字）。
var codeRE = regexp.MustCompile(`(?:^|[^0-9])([0-9]{6})(?:[^0-9]|$)`)

// mailSender 是账号类型对应的发件人域名。
func mailSender(kind string) string {
	if kind == KindCodex {
		return codexMailSender
	}
	return claudeMailSender
}

// MailLister 返回账号最近 sender（域名）发来的邮件原文（RFC 822），新的在前。测试时替换。
type MailLister func(ctx context.Context, accountID int64, sender string) ([]RawMail, error)

// RawMail 是一封邮件的原文和 Gmail 记录的到达时间。
type RawMail struct {
	ID         string
	Raw        []byte
	ReceivedAt time.Time
}

func (s *Server) mailLister() MailLister {
	if s.ListMail != nil {
		return s.ListMail
	}
	return s.listAccountMail
}

// listAccountMail 按账号的收信方式取邮件：设置了邮箱密码时用 IMAP，否则用 Gmail API。
func (s *Server) listAccountMail(ctx context.Context, accountID int64, sender string) ([]RawMail, error) {
	l, ok, err := s.mailboxLogin(ctx, accountID)
	if err != nil {
		return nil, err
	}
	if ok {
		return s.listImapRaw(ctx, l, sender, time.Now().AddDate(0, 0, -mailListDays))
	}
	return s.listGmailRaw(ctx, accountID, sender)
}

// ClaudeMails 返回账号最近 7 天 Anthropic 发来的邮件（最多 10 封）。客户端须被允许使用这个账号。
func (s *Server) ClaudeMails(ctx context.Context, deviceID, accountID int64) ([]ClaudeMail, error) {
	a, err := s.claudeAccount(ctx, accountID)
	if err != nil {
		return nil, err
	}
	if !allowed(a, deviceID) {
		return nil, ErrNotFound
	}
	if a.MailSource == "" {
		return nil, &InputError{"这个账号还没有在管理服务上设置收信（邮箱密码或 Gmail 授权）"}
	}
	raws, err := s.mailLister()(ctx, accountID, mailSender(a.Kind))
	if err != nil {
		return nil, err
	}
	out := []ClaudeMail{}
	for _, r := range raws {
		if m, ok := parseClaudeMail(r, a.Kind); ok {
			out = append(out, m)
		}
	}
	slices.SortFunc(out, func(a, b ClaudeMail) int { return b.Date.Compare(a.Date) })
	return out, nil
}

func (s *Server) listGmailRaw(ctx context.Context, accountID int64, sender string) ([]RawMail, error) {
	token, err := s.gmailAccessToken(ctx, accountID)
	if err != nil {
		return nil, err
	}
	q := url.Values{"q": {fmt.Sprintf("from:%s newer_than:%dd", sender, mailListDays)}, "maxResults": {strconv.Itoa(mailListMax)}}
	var list struct {
		Messages []struct {
			ID string `json:"id"`
		} `json:"messages"`
	}
	if err := s.gmailGet(ctx, token, "/messages?"+q.Encode(), &list); err != nil {
		return nil, err
	}
	var out []RawMail
	for _, m := range list.Messages {
		var msg struct {
			Raw          string `json:"raw"`
			InternalDate string `json:"internalDate"` // 毫秒
		}
		if err := s.gmailGet(ctx, token, "/messages/"+url.PathEscape(m.ID)+"?format=raw", &msg); err != nil {
			return nil, err
		}
		raw, err := base64.URLEncoding.DecodeString(padBase64(msg.Raw))
		if err != nil {
			continue
		}
		ms, _ := strconv.ParseInt(msg.InternalDate, 10, 64)
		out = append(out, RawMail{ID: m.ID, Raw: raw, ReceivedAt: time.UnixMilli(ms).UTC()})
	}
	return out, nil
}

// fromDomain 判断发件人是不是 sender 或它的子域名。
func fromDomain(addr, sender string) bool {
	_, domain, ok := strings.Cut(strings.ToLower(addr), "@")
	return ok && (domain == sender || strings.HasSuffix(domain, "."+sender))
}

// parseClaudeMail 解析一封邮件；发件人不是账号类型对应的域名的丢弃（Gmail 的 from: 是模糊匹配）。
func parseClaudeMail(r RawMail, kind string) (ClaudeMail, bool) {
	msg, err := mail.ReadMessage(bytes.NewReader(r.Raw))
	if err != nil {
		return ClaudeMail{}, false
	}
	from, err := mail.ParseAddress(msg.Header.Get("From"))
	if err != nil || !fromDomain(from.Address, mailSender(kind)) {
		return ClaudeMail{}, false
	}
	dec := new(mime.WordDecoder)
	subject, err := dec.DecodeHeader(msg.Header.Get("Subject"))
	if err != nil {
		subject = msg.Header.Get("Subject")
	}
	date := r.ReceivedAt
	if d, err := msg.Header.Date(); err == nil && date.IsZero() {
		date = d.UTC()
	}
	plain, htm := mailText(msg.Header.Get("Content-Type"), msg.Header.Get("Content-Transfer-Encoding"), msg.Body, 0)
	text := plain
	if strings.TrimSpace(text) == "" {
		text = htmlToText(htm)
	}
	text = strings.ReplaceAll(text, "&amp;", "&")
	links, codes := []string{}, []string{}
	if kind == KindCodex {
		for _, m := range codeRE.FindAllStringSubmatch(subject+"\n"+text, -1) {
			if !slices.Contains(codes, m[1]) {
				codes = append(codes, m[1])
			}
		}
	} else {
		for _, l := range magicLinkRE.FindAllString(text+"\n"+strings.ReplaceAll(htm, "&amp;", "&"), -1) {
			if !slices.Contains(links, l) {
				links = append(links, l)
			}
		}
	}
	text = strings.TrimSpace(text)
	if utf8.RuneCountInString(text) > mailTextMax {
		text = string([]rune(text)[:mailTextMax]) + "\n…"
	}
	name := from.Address
	if from.Name != "" {
		name = from.Name + " <" + from.Address + ">"
	}
	return ClaudeMail{ID: r.ID, From: name, Subject: subject, Date: date, Text: text, Links: links, Codes: codes}, true
}

// mailText 取第一段 text/plain 和第一段 text/html（已解码、转成 UTF-8）。
func mailText(contentType, encoding string, body io.Reader, depth int) (plain, htm string) {
	mediaType, params, err := mime.ParseMediaType(contentType)
	if err != nil {
		mediaType = "text/plain"
	}
	if strings.HasPrefix(mediaType, "multipart/") {
		if depth > 5 || params["boundary"] == "" {
			return "", ""
		}
		mr := multipart.NewReader(body, params["boundary"])
		for {
			p, err := mr.NextPart()
			if err != nil {
				return plain, htm
			}
			pp, ph := mailText(p.Header.Get("Content-Type"), p.Header.Get("Content-Transfer-Encoding"), p, depth+1)
			if plain == "" {
				plain = pp
			}
			if htm == "" {
				htm = ph
			}
		}
	}
	if mediaType != "text/plain" && mediaType != "text/html" {
		return "", ""
	}
	var rd io.Reader = body
	switch strings.ToLower(strings.TrimSpace(encoding)) {
	case "quoted-printable":
		rd = quotedprintable.NewReader(body)
	case "base64":
		rd = base64.NewDecoder(base64.StdEncoding, newlineStripper{body})
	}
	b, err := io.ReadAll(io.LimitReader(rd, maxMailMessage))
	if err != nil && len(b) == 0 {
		return "", ""
	}
	s := string(b)
	if !utf8.ValidString(s) {
		s = strings.ToValidUTF8(s, "�") // Anthropic 的邮件是 UTF-8；其他字符集不转换
	}
	if mediaType == "text/html" {
		return "", s
	}
	return s, ""
}

var (
	htmlDropRE  = regexp.MustCompile(`(?is)<(style|script|head)[^>]*>.*?</(style|script|head)>`)
	htmlBreakRE = regexp.MustCompile(`(?i)<br\s*/?>|</(p|div|tr|li|h[1-6]|table)>`)
	htmlLinkRE  = regexp.MustCompile(`(?is)<a\s[^>]*href=["']([^"']+)["'][^>]*>(.*?)</a>`)
	htmlTagRE   = regexp.MustCompile(`(?s)<[^>]*>`)
	blankRE     = regexp.MustCompile(`\n[ \t\r]*(\n[ \t\r]*)+`)
	spacesRE    = regexp.MustCompile(`[ \t]+`)
)

// htmlToText 把 HTML 邮件转成纯文本：保留段落换行，链接写成「文字（地址）」。
func htmlToText(s string) string {
	s = htmlDropRE.ReplaceAllString(s, "")
	s = htmlLinkRE.ReplaceAllStringFunc(s, func(m string) string {
		sub := htmlLinkRE.FindStringSubmatch(m)
		href := html.UnescapeString(sub[1])
		label := strings.TrimSpace(html.UnescapeString(htmlTagRE.ReplaceAllString(sub[2], "")))
		if label == "" || label == href {
			return " " + href + " "
		}
		return label + "（" + href + "）"
	})
	s = htmlBreakRE.ReplaceAllString(s, "\n")
	s = htmlTagRE.ReplaceAllString(s, "")
	s = html.UnescapeString(s)
	s = spacesRE.ReplaceAllString(s, " ")
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = strings.TrimSpace(l)
	}
	return blankRE.ReplaceAllString(strings.Join(lines, "\n"), "\n\n")
}

func (s *Server) claudeMailRoutes(mux *http.ServeMux, device func(func(http.ResponseWriter, *http.Request, Device)) http.Handler, fail func(http.ResponseWriter, error)) {
	mux.Handle("GET /api/v1/client/claude-accounts/{id}/mails", device(func(w http.ResponseWriter, r *http.Request, d Device) {
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil {
			fail(w, ErrNotFound)
			return
		}
		list, err := s.ClaudeMails(r.Context(), d.ID, id)
		if err != nil {
			fail(w, err)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, http.StatusOK, list)
	}))
}

// ClaudeMails 读取账号最近的 Anthropic 邮件（客户端）。
func (c *Client) ClaudeMails(ctx context.Context, accountID int64) ([]ClaudeMail, error) {
	var out []ClaudeMail
	err := c.doJSON(ctx, http.MethodGet, fmt.Sprintf("/api/v1/client/claude-accounts/%d/mails", accountID), nil, &out, requestTimeout+20*time.Second)
	if out == nil {
		out = []ClaudeMail{}
	}
	return out, err
}
