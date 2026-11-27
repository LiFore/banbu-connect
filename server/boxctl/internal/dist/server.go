package dist

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"boxctl/internal/groups"
	"boxctl/internal/nodes"
	"boxctl/internal/objstore"
	"boxctl/internal/otel"
	"boxctl/internal/secret"
	"boxctl/internal/subscriptions"
)

// Server 是管理服务端：用自己的节点和代理组生成配置并发布，管理客户端和更新包。
type Server struct {
	DB      *sql.DB
	Secrets *secret.Box
	Nodes   *nodes.Service
	Groups  *groups.Service
	// Dir 保存更新包文件。
	Dir string
	// FetchMail 读取 Claude 登录邮件；为空时用 Gmail API（测试时替换）。
	FetchMail MailFetcher
	// ListMail 读取 Claude 账号最近的邮件原文；为空时用 Gmail API（测试时替换）。
	ListMail MailLister
	// Google 是 Google 的接口地址；为空时用正式地址（测试时替换）。
	Google GoogleAPI
	// FetchProxy 是管理服务下载远程规则集（预览）时用的代理；为空时直连（仍读 HTTP_PROXY 等环境变量）。
	FetchProxy string
	// OTel 保存客户端上报的 Claude Code 遥测；为空时不接收。
	OTel       *otel.Store
	otelPruned atomic.Int64

	loginMu sync.Mutex
	logins  map[string]*claudeLogin // 进行中和刚结束的 Claude 登录，只在内存里

	gmailMu      sync.Mutex
	gmailPending map[int64]*gmailPending // 进行中的 Gmail 授权（PKCE），按账号
	gmailTokens  map[int64]cachedToken   // Gmail 访问令牌缓存，按账号

	geminiMu     sync.Mutex
	geminiWake   chan struct{} // Gemini 账号、凭据、分配变化时关闭，唤醒等待同步的客户端
	geminiPruned atomic.Int64  // 上次清理额度采样的时间（Unix 秒）

	testMu    sync.Mutex
	authTests map[string]*AuthTest // 最近的鉴权测试，按「类型:账号」，只在内存里（ADR-061）

	refreshMu sync.Mutex
	refresh   refreshBook // Claude / Codex 令牌集中续期（ADR-071），只在内存里

	storage storageState // 对象存储与云盘上传（S1.11）
	// Subs 是订阅服务：用量提醒读订阅的流量和到期（S1.12）；为空时不检查订阅。
	Subs       *subscriptions.Service
	subUpdates subUpdates

	connPruned atomic.Int64 // 上次清理代理连接统计的时间（Unix 秒）
	liveMu     sync.Mutex
	live       map[int64]*liveState // 管理页面正在看的客户端实时连接（BXC-F0-030）
}

const (
	profileAAD     = "dist-profile"
	egressKey      = "dist_strict_egress"
	maxReleaseSize = 512 << 20
)

// ErrNotFound 表示记录不存在。
var ErrNotFound = errors.New("记录不存在")

// InputError 是用户输入错误，API 返回 400。
type InputError struct{ Msg string }

func (e *InputError) Error() string { return e.Msg }

// ---- 草稿与发布 ----

// Draft 是按服务端当前数据生成、尚未发布的配置：顶层是「默认」方案筛选后的内容，其他方案放进 Variants。
func (s *Server) Draft(ctx context.Context) (Profile, error) {
	ns, err := s.Nodes.Export(ctx)
	if err != nil {
		return Profile{}, err
	}
	gs, err := s.Groups.Export(ctx)
	if err != nil {
		return Profile{}, err
	}
	def, err := s.defaultProfile(ctx)
	if err != nil {
		return Profile{}, err
	}
	egress, err := s.StrictEgress(ctx)
	if err != nil {
		return Profile{}, err
	}
	boxSecret, err := s.BoxSecret(ctx)
	if err != nil {
		return Profile{}, err
	}
	sc := scope(ns, gs, def.ProfileContent)
	rules, sets := scopeRules(def.Rules, def.RuleSets, sc.Excluded)
	variants, err := s.ruleVariants(ctx, ns, gs)
	if err != nil {
		return Profile{}, err
	}
	return Profile{Nodes: sc.Nodes, Groups: sc.Groups, Selections: sc.Selections, Rules: rules, StrictEgress: egress, RuleSets: sets,
		BoxSecret: boxSecret, ProfileVersion: def.Version, Variants: variants}, nil
}

// Publish 发布当前草稿为新版本。
func (s *Server) Publish(ctx context.Context, note string) (Profile, error) {
	p, err := s.Draft(ctx)
	if err != nil {
		return Profile{}, err
	}
	if len(p.Nodes) == 0 {
		return Profile{}, &InputError{"还没有启用的节点，不能发布"}
	}
	var last sql.NullInt64
	if err := s.DB.QueryRowContext(ctx, `SELECT MAX(version) FROM profiles`).Scan(&last); err != nil {
		return Profile{}, err
	}
	p.Version = last.Int64 + 1
	p.PublishedAt = time.Now().UTC().Truncate(time.Second)
	p.Note = strings.TrimSpace(note)
	body, err := json.Marshal(p)
	if err != nil {
		return Profile{}, err
	}
	// 配置里有节点凭据，和节点表一样加密保存。
	sealed, err := s.Secrets.Seal(body, profileAAD)
	if err != nil {
		return Profile{}, err
	}
	_, err = s.DB.ExecContext(ctx, `INSERT INTO profiles (version, note, body, created_at) VALUES (?, ?, ?, ?)`,
		p.Version, p.Note, sealed, p.PublishedAt.Format(time.RFC3339))
	return p, err
}

// Current 返回最新发布的版本。
func (s *Server) Current(ctx context.Context) (Profile, bool, error) {
	var sealed []byte
	err := s.DB.QueryRowContext(ctx, `SELECT body FROM profiles ORDER BY version DESC LIMIT 1`).Scan(&sealed)
	if errors.Is(err, sql.ErrNoRows) {
		return Profile{}, false, nil
	}
	if err != nil {
		return Profile{}, false, err
	}
	body, err := s.Secrets.Open(sealed, profileAAD)
	if err != nil {
		return Profile{}, false, fmt.Errorf("解密配置失败：%w", err)
	}
	var p Profile
	if err := json.Unmarshal(body, &p); err != nil {
		return Profile{}, false, err
	}
	return p, true, nil
}

// ProfileMeta 是发布历史的一项。
type ProfileMeta struct {
	Version   int64  `json:"version"`
	Note      string `json:"note"`
	CreatedAt string `json:"created_at"`
	Devices   int    `json:"devices"` // 已同步到这个版本的客户端
}

func (s *Server) History(ctx context.Context) ([]ProfileMeta, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT p.version, p.note, p.created_at,
		(SELECT COUNT(*) FROM devices d WHERE d.revoked = 0 AND d.profile_version = p.version)
		FROM profiles p ORDER BY p.version DESC LIMIT 50`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ProfileMeta{}
	for rows.Next() {
		var m ProfileMeta
		if err := rows.Scan(&m.Version, &m.Note, &m.CreatedAt, &m.Devices); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// ---- 线路与规则（发布前编辑，存在 settings 表） ----

// 规则分段（M5-03）：自定义 / 服务 / 直连 / 代理 / 兜底。段的边界用标记行 `#@segment:名称` 保存在规则列表里，
// 下发时去掉（客户端的生成器本来也跳过 # 开头的行）。MATCH 只能放在兜底段，且只能有一条。
const segmentPrefix = "#@segment:"

var segments = []string{"custom", "service", "direct", "proxy", "fallback"}

// NormalizeRules 去掉空行和普通注释，保留分段标记，检查段的顺序和 MATCH 的位置。
func NormalizeRules(rules []string) ([]string, error) {
	clean := []string{}
	seg := -1 // 还没有标记时视为自定义段之前（旧数据没有分段）
	matches := 0
	for _, r := range rules {
		r = strings.TrimSpace(r)
		if name, ok := strings.CutPrefix(r, segmentPrefix); ok {
			i := slices.Index(segments, name)
			if i < 0 {
				return nil, &InputError{"未知的规则分段：" + name}
			}
			if i <= seg {
				return nil, &InputError{"规则分段的顺序须是 自定义、服务、直连、代理、兜底"}
			}
			seg = i
			clean = append(clean, r)
			continue
		}
		if r == "" || strings.HasPrefix(r, "#") {
			continue
		}
		if strings.Count(r, ",") < 1 {
			return nil, &InputError{"规则格式不对：" + r}
		}
		if strings.HasPrefix(strings.ToUpper(r), "MATCH,") {
			matches++
			if matches > 1 {
				return nil, &InputError{"MATCH 只能有一条"}
			}
			if seg >= 0 && segments[seg] != "fallback" {
				return nil, &InputError{"MATCH 只能放在兜底段"}
			}
		} else if seg >= 0 && segments[seg] == "fallback" {
			return nil, &InputError{"兜底段只能放 MATCH：" + r}
		}
		clean = append(clean, r)
	}
	return clean, nil
}

// stripSegments 去掉分段标记，得到下发给客户端的规则。
func stripSegments(rules []string) []string {
	out := []string{}
	for _, r := range rules {
		if !strings.HasPrefix(r, segmentPrefix) {
			out = append(out, r)
		}
	}
	return out
}

// boxSecretKey 是路由盒发现口令的设置项（secret.Box 加密）。
const boxSecretKey = "box_secret"

// BoxSecret 返回路由盒发现口令（ADR-073），没设置时为空。
func (s *Server) BoxSecret(ctx context.Context) (string, error) {
	var stored struct {
		Sealed []byte `json:"sealed"`
	}
	if err := s.loadSetting(ctx, boxSecretKey, &stored); err != nil || len(stored.Sealed) == 0 {
		return "", err
	}
	b, err := s.Secrets.Open(stored.Sealed, boxSecretKey)
	if err != nil {
		return "", fmt.Errorf("解密路由盒口令失败：%w", err)
	}
	return string(b), nil
}

// SetBoxSecret 保存路由盒发现口令；空串表示关闭自动发现。发布后生效。
func (s *Server) SetBoxSecret(ctx context.Context, secret string) error {
	secret = strings.TrimSpace(secret)
	if secret != "" && (len(secret) < 12 || len(secret) > 128) {
		return &InputError{"路由盒口令要 12～128 个字符"}
	}
	var sealed []byte
	if secret != "" {
		var err error
		if sealed, err = s.Secrets.Seal([]byte(secret), boxSecretKey); err != nil {
			return err
		}
	}
	return s.saveSetting(ctx, boxSecretKey, map[string][]byte{"sealed": sealed})
}

func (s *Server) StrictEgress(ctx context.Context) ([]string, error) {
	out := []string{}
	return out, s.loadSetting(ctx, egressKey, &out)
}

// SetStrictEgress 保存严格线路允许的出口。每项是 IP 或网段，单个 IP 按 /32、/128 保存。
func (s *Server) SetStrictEgress(ctx context.Context, list []string) error {
	clean := []string{}
	for _, raw := range list {
		v := strings.TrimSpace(raw)
		if v == "" || strings.HasPrefix(v, "#") {
			continue
		}
		var p netip.Prefix
		var err error
		if strings.Contains(v, "/") {
			p, err = netip.ParsePrefix(v)
		} else {
			var a netip.Addr
			if a, err = netip.ParseAddr(v); err == nil {
				p = netip.PrefixFrom(a, a.BitLen())
			}
		}
		if err != nil {
			return &InputError{"出口 IP 或网段无效：" + v}
		}
		if p = p.Masked(); !slices.Contains(clean, p.String()) {
			clean = append(clean, p.String())
		}
	}
	return s.saveSetting(ctx, egressKey, clean)
}

func (s *Server) loadSetting(ctx context.Context, key string, out any) error {
	var raw string
	err := s.DB.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = ?`, key).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	return json.Unmarshal([]byte(raw), out)
}

func (s *Server) saveSetting(ctx context.Context, key string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = s.DB.ExecContext(ctx, `INSERT INTO settings (key, value) VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = strftime('%Y-%m-%dT%H:%M:%SZ','now')`, key, string(b))
	return err
}

// ---- 客户端 ----

type Device struct {
	ID             int64  `json:"id"`
	Name           string `json:"name"`
	CreatedAt      string `json:"created_at"`
	LastSeen       string `json:"last_seen"`
	LastIP         string `json:"last_ip"`
	AppVersion     string `json:"app_version"`
	Platform       string `json:"platform"`
	OSVersion      string `json:"os_version"` // 客户端上报的系统版本，如 macOS 26.1、Windows 11 Pro 24H2
	ProfileVersion int64  `json:"profile_version"`
	Revoked        bool   `json:"revoked"`
	DiagAt         string `json:"diag_at"` // 最近一次上报诊断数据的时间
	// ReportedName 是客户端最近一次上报的电脑名；NameCustom 表示名称是管理员改的，不再被上报的电脑名覆盖。
	ReportedName string `json:"reported_name"`
	NameCustom   bool   `json:"name_custom"`
	// UserID 是设备所属的用户（users.id），0 表示不属于任何用户。
	UserID int64 `json:"user_id"`
}

const selectDevice = `SELECT id, name, created_at, last_seen, last_ip, app_version, platform, os_version, profile_version, revoked, diag_at,
	reported_name, name_custom, user_id FROM devices`

func scanDevice(sc interface{ Scan(...any) error }) (Device, error) {
	var d Device
	err := sc.Scan(&d.ID, &d.Name, &d.CreatedAt, &d.LastSeen, &d.LastIP, &d.AppVersion, &d.Platform, &d.OSVersion, &d.ProfileVersion, &d.Revoked, &d.DiagAt,
		&d.ReportedName, &d.NameCustom, &d.UserID)
	return d, err
}

// maxDeviceName 是客户端名称的最大长度（字符数）。
const maxDeviceName = 64

// RenameDevice 修改客户端名称。改过名的客户端不再被上报的电脑名覆盖；name 为空时恢复自动命名：
// 用客户端最近一次上报的电脑名，还没上报过时用「待连接 #id」。
func (s *Server) RenameDevice(ctx context.Context, id int64, name string) (Device, error) {
	name = strings.TrimSpace(name)
	if utf8.RuneCountInString(name) > maxDeviceName {
		return Device{}, &InputError{fmt.Sprintf("名称最多 %d 个字", maxDeviceName)}
	}
	var err error
	if name == "" {
		err = s.execOne(ctx, `UPDATE devices SET name_custom = 0,
			name = CASE WHEN reported_name = '' THEN '待连接 #' || id ELSE reported_name END WHERE id = ?`, id)
	} else {
		err = s.execOne(ctx, `UPDATE devices SET name = ?, name_custom = 1 WHERE id = ?`, name, id)
	}
	if err != nil {
		return Device{}, err
	}
	return scanDevice(s.DB.QueryRowContext(ctx, selectDevice+` WHERE id = ?`, id))
}

func (s *Server) Devices(ctx context.Context) ([]Device, error) {
	rows, err := s.DB.QueryContext(ctx, selectDevice+` ORDER BY revoked, id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Device{}
	for rows.Next() {
		d, err := scanDevice(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

func hashToken(t string) string {
	sum := sha256.Sum256([]byte(t))
	return hex.EncodeToString(sum[:])
}

// CreateDevice 新建客户端，返回只显示这一次的令牌。名称由客户端连接时上报的设备名覆盖，
// 在那之前显示「待连接 #id」。
func (s *Server) CreateDevice(ctx context.Context) (Device, string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return Device{}, "", err
	}
	token := "bc_" + hex.EncodeToString(buf)
	res, err := s.DB.ExecContext(ctx, `INSERT INTO devices (name, token_hash) VALUES ('', ?)`, hashToken(token))
	if err != nil {
		return Device{}, "", err
	}
	id, _ := res.LastInsertId()
	if _, err := s.DB.ExecContext(ctx, `UPDATE devices SET name = ? WHERE id = ?`, fmt.Sprintf("待连接 #%d", id), id); err != nil {
		return Device{}, "", err
	}
	d, err := scanDevice(s.DB.QueryRowContext(ctx, selectDevice+` WHERE id = ?`, id))
	return d, token, err
}

func (s *Server) RevokeDevice(ctx context.Context, id int64) error {
	return s.execOne(ctx, `UPDATE devices SET revoked = 1 WHERE id = ?`, id)
}

func (s *Server) DeleteDevice(ctx context.Context, id int64) error {
	if err := s.execOne(ctx, `DELETE FROM devices WHERE id = ?`, id); err != nil {
		return err
	}
	// Claude Code 遥测在单独的库里，没有外键，一并删除。
	if s.OTel != nil {
		return s.OTel.DeleteDevice(ctx, id)
	}
	return nil
}

func (s *Server) execOne(ctx context.Context, q string, args ...any) error {
	res, err := s.DB.ExecContext(ctx, q, args...)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// Authenticate 按令牌找客户端；已吊销的视为无效。
func (s *Server) Authenticate(ctx context.Context, token string) (Device, error) {
	d, err := scanDevice(s.DB.QueryRowContext(ctx, selectDevice+` WHERE token_hash = ? AND revoked = 0`, hashToken(token)))
	if errors.Is(err, sql.ErrNoRows) {
		return Device{}, ErrNotFound
	}
	return d, err
}

// ClientInfo 是客户端每次请求时上报的信息，空字段不覆盖已有的值。
type ClientInfo struct {
	AppVersion string
	Platform   string
	// Hostname、OSVersion 由新版客户端上报。Hostname 记为 reported_name，管理员没改过名时也作为名称；
	// 旧版客户端不上报，名称保持不变。
	Hostname  string
	OSVersion string
}

// Touch 记录客户端最近一次联系。
func (s *Server) Touch(ctx context.Context, id int64, ip string, info ClientInfo, profileVersion int64) error {
	_, err := s.DB.ExecContext(ctx, `UPDATE devices SET last_seen = ?, last_ip = ?,
		app_version = CASE WHEN ? = '' THEN app_version ELSE ? END,
		platform = CASE WHEN ? = '' THEN platform ELSE ? END,
		name = CASE WHEN ? = '' OR name_custom = 1 THEN name ELSE ? END,
		reported_name = CASE WHEN ? = '' THEN reported_name ELSE ? END,
		os_version = CASE WHEN ? = '' THEN os_version ELSE ? END,
		profile_version = CASE WHEN ? = 0 THEN profile_version ELSE ? END WHERE id = ?`,
		time.Now().UTC().Format(time.RFC3339), ip, info.AppVersion, info.AppVersion, info.Platform, info.Platform,
		info.Hostname, info.Hostname, info.Hostname, info.Hostname, info.OSVersion, info.OSVersion, profileVersion, profileVersion, id)
	return err
}

// ---- 更新包 ----

type Release struct {
	ID        int64  `json:"id"`
	Version   string `json:"version"`
	Target    string `json:"target"`
	Notes     string `json:"notes"`
	FileName  string `json:"file_name"`
	Signature string `json:"signature"`
	Size      int64  `json:"size"`
	Published bool   `json:"published"`
	CreatedAt string `json:"created_at"`
	// Devices 是灰度名单：只有这些客户端能收到；空表示全部客户端。
	Devices []int64 `json:"devices"`
	// Storage 是文件存在哪里（local / oss），ObjectKey 是 OSS 上的对象路径（本机的旧记录为空）。
	Storage   string `json:"storage"`
	ObjectKey string `json:"-"`
	// Mode 是更新方式（normal / push / silent），MinVersion 是最低版本（见 update_policy.go）。
	Mode       string `json:"mode"`
	MinVersion string `json:"min_version"`
}

// For 表示这个更新包是否发给这台客户端。
func (r Release) For(device int64) bool {
	return len(r.Devices) == 0 || slices.Contains(r.Devices, device)
}

// Targets 是支持的平台，与 Tauri updater 的 {{target}}-{{arch}} 对应。
// lite- 开头的是 Banbu Connect Lite（client 分支，只做账号管理）：它查更新时平台带这个前缀，和完整版的更新包分开。
var Targets = []string{"darwin-aarch64", "darwin-x86_64", "windows-x86_64",
	"lite-darwin-aarch64", "lite-darwin-x86_64", "lite-windows-x86_64"}

var (
	versionRE  = regexp.MustCompile(`^\d+\.\d+\.\d+(?:-[0-9A-Za-z.]+)?$`)
	fileNameRE = regexp.MustCompile(`[^A-Za-z0-9._-]+`)
)

const selectRelease = `SELECT id, version, target, notes, file_name, signature, size, published, created_at, devices, mode, min_version, storage, object_key FROM releases`

func scanRelease(sc interface{ Scan(...any) error }) (Release, error) {
	var r Release
	var devices string
	if err := sc.Scan(&r.ID, &r.Version, &r.Target, &r.Notes, &r.FileName, &r.Signature, &r.Size, &r.Published, &r.CreatedAt, &devices,
		&r.Mode, &r.MinVersion, &r.Storage, &r.ObjectKey); err != nil {
		return r, err
	}
	r.Devices = []int64{}
	_ = json.Unmarshal([]byte(devices), &r.Devices)
	return r, nil
}

func (s *Server) Releases(ctx context.Context) ([]Release, error) {
	rows, err := s.DB.QueryContext(ctx, selectRelease+` ORDER BY id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Release{}
	for rows.Next() {
		r, err := scanRelease(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// SignatureInfo 是 tauri signer 写在签名「trusted comment」里的信息。
type SignatureInfo struct {
	Version string // 构建时的应用版本；旧版 tauri cli 不写，为空
	File    string // 签名时的文件名
}

// ParseSignature 解析 .sig 文件内容（base64 编码的 minisign 签名）。
func ParseSignature(sig string) (SignatureInfo, error) {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(sig))
	if err != nil {
		return SignatureInfo{}, &InputError{"签名不是 .sig 文件的内容（应为一段 base64 文本）"}
	}
	var info SignatureInfo
	found := false
	for _, line := range strings.Split(string(raw), "\n") {
		comment, ok := strings.CutPrefix(strings.TrimSpace(line), "trusted comment:")
		if !ok {
			continue
		}
		found = true
		for _, f := range strings.Split(strings.TrimSpace(comment), "\t") {
			if v, ok := strings.CutPrefix(f, "version:"); ok {
				info.Version = strings.TrimPrefix(strings.TrimSpace(v), "v")
			}
			if v, ok := strings.CutPrefix(f, "file:"); ok {
				info.File = strings.TrimSpace(v)
			}
		}
	}
	if !found {
		return SignatureInfo{}, &InputError{"签名不是 tauri signer 生成的 .sig 内容"}
	}
	return info, nil
}

// checkSignature 在上传时核对签名和填写的版本、文件：Tauri 安装前会校验签名里的版本，
// 对不上时客户端报「The update was signed for version …」，这里提前拦下。
func checkSignature(sig, version, fileName string) error {
	info, err := ParseSignature(sig)
	if err != nil {
		return err
	}
	if info.Version != "" && info.Version != version {
		return &InputError{fmt.Sprintf("签名里的版本是 %s，与填写的 %s 不一致：这个安装包是按 %s 构建的，客户端会拒绝安装。"+
			"请把 tauri.conf.json 的 version 改成 %s 后重新构建，或把版本号填成 %s", info.Version, version, info.Version, version, info.Version)}
	}
	if info.File != "" {
		signed := fileNameRE.ReplaceAllString(filepath.Base(info.File), "_")
		if signed != fileName {
			return &InputError{fmt.Sprintf("签名对应的文件是 %s，上传的是 %s：请上传同一个文件和它的 .sig", info.File, fileName)}
		}
	}
	return nil
}

// AddRelease 保存上传的更新包。signature 是 tauri signer 生成的 .sig 文件内容。
func (s *Server) AddRelease(ctx context.Context, version, target, notes, fileName, signature string, body io.Reader) (Release, error) {
	version, target, signature = strings.TrimSpace(version), strings.TrimSpace(target), strings.TrimSpace(signature)
	if !versionRE.MatchString(version) {
		return Release{}, &InputError{"版本号须为 x.y.z"}
	}
	known := false
	for _, t := range Targets {
		known = known || t == target
	}
	if !known {
		return Release{}, &InputError{"平台须为 " + strings.Join(Targets, "、")}
	}
	if signature == "" {
		return Release{}, &InputError{"缺少签名（.sig 文件）"}
	}
	fileName = fileNameRE.ReplaceAllString(filepath.Base(fileName), "_")
	if fileName == "" || fileName == "." {
		return Release{}, &InputError{"文件名无效"}
	}
	if err := checkSignature(signature, version, fileName); err != nil {
		return Release{}, err
	}
	dir := filepath.Join(s.Dir, target, version)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return Release{}, err
	}
	store, prefix, err := s.activeStore(ctx)
	if err != nil {
		return Release{}, err
	}
	tmp, err := os.CreateTemp(dir, ".upload-*")
	if err != nil {
		return Release{}, err
	}
	defer os.Remove(tmp.Name())
	n, err := io.Copy(tmp, io.LimitReader(body, maxReleaseSize+1))
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return Release{}, err
	}
	if n > maxReleaseSize {
		return Release{}, &InputError{"文件超过 512 MB"}
	}
	if n == 0 {
		return Release{}, &InputError{"文件是空的"}
	}
	old, oldErr := scanRelease(s.DB.QueryRowContext(ctx, selectRelease+` WHERE version = ? AND target = ?`, version, target))
	storage, objectKey := objstore.KindLocal, ""
	if store.Kind() == objstore.KindOSS {
		// 存到 OSS：临时文件上传后删除，本机不留副本。
		storage, objectKey = objstore.KindOSS, prefix+"releases/"+target+"/"+version+"/"+fileName
		f, err := os.Open(tmp.Name())
		if err != nil {
			return Release{}, err
		}
		err = store.Put(ctx, objectKey, f, n)
		f.Close()
		if err != nil {
			return Release{}, fmt.Errorf("上传到 OSS 失败：%w", err)
		}
	} else if err := os.Rename(tmp.Name(), filepath.Join(dir, fileName)); err != nil {
		return Release{}, err
	}
	_, err = s.DB.ExecContext(ctx, `INSERT INTO releases (version, target, notes, file_name, signature, size, storage, object_key)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT(version, target) DO UPDATE SET notes = excluded.notes,
		file_name = excluded.file_name, signature = excluded.signature, size = excluded.size, published = 0,
		storage = excluded.storage, object_key = excluded.object_key`,
		version, target, strings.TrimSpace(notes), fileName, signature, n, storage, objectKey)
	if err != nil {
		return Release{}, err
	}
	// 同一版本重新上传：旧文件换了位置（文件名或存储变了）时删掉。
	if oldErr == nil && (old.Storage != storage || s.releaseKey(old) != s.releaseKey(Release{Target: target, Version: version, FileName: fileName, ObjectKey: objectKey})) {
		s.removeReleaseFile(ctx, old)
	}
	return scanRelease(s.DB.QueryRowContext(ctx, selectRelease+` WHERE version = ? AND target = ?`, version, target))
}

// releaseKey 是更新包在它所在存储里的路径。
func (s *Server) releaseKey(r Release) string {
	if r.ObjectKey != "" {
		return r.ObjectKey
	}
	return s.legacyReleaseKey(r)
}

func (s *Server) removeReleaseFile(ctx context.Context, r Release) {
	store, err := s.storeFor(ctx, r.Storage)
	if err == nil {
		err = store.Delete(ctx, s.releaseKey(r))
	}
	if err != nil {
		slog.Warn("delete release file failed", "id", r.ID, "storage", r.Storage, "err", err)
	}
}

func (s *Server) SetReleasePublished(ctx context.Context, id int64, published bool) error {
	return s.execOne(ctx, `UPDATE releases SET published = ? WHERE id = ?`, published, id)
}

// SetReleaseDevices 设置灰度名单；空表示全部客户端。
func (s *Server) SetReleaseDevices(ctx context.Context, id int64, devices []int64) error {
	ids := []int64{}
	for _, d := range devices {
		if d > 0 && !slices.Contains(ids, d) {
			ids = append(ids, d)
		}
	}
	b, _ := json.Marshal(ids)
	return s.execOne(ctx, `UPDATE releases SET devices = ? WHERE id = ?`, string(b), id)
}

func (s *Server) DeleteRelease(ctx context.Context, id int64) error {
	r, err := scanRelease(s.DB.QueryRowContext(ctx, selectRelease+` WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if err := s.execOne(ctx, `DELETE FROM releases WHERE id = ?`, id); err != nil {
		return err
	}
	s.removeReleaseFile(ctx, r)
	return nil
}

// LatestRelease 返回某个平台发给这台客户端的、已发布的最高版本（灰度名单之外的客户端看不到灰度中的版本）。
func (s *Server) LatestRelease(ctx context.Context, target string, device int64) (Release, bool, error) {
	rows, err := s.DB.QueryContext(ctx, selectRelease+` WHERE target = ? AND published = 1`, target)
	if err != nil {
		return Release{}, false, err
	}
	defer rows.Close()
	var best Release
	found := false
	for rows.Next() {
		r, err := scanRelease(rows)
		if err != nil {
			return Release{}, false, err
		}
		if r.For(device) && (!found || CompareVersions(r.Version, best.Version) > 0) {
			best, found = r, true
		}
	}
	return best, found, rows.Err()
}

// OpenRelease 找到发给这台客户端的、已发布的更新包，返回它所在的存储和路径。
func (s *Server) OpenRelease(ctx context.Context, id, device int64) (Release, objstore.Store, string, error) {
	r, err := scanRelease(s.DB.QueryRowContext(ctx, selectRelease+` WHERE id = ? AND published = 1`, id))
	if errors.Is(err, sql.ErrNoRows) || (err == nil && !r.For(device)) {
		return Release{}, nil, "", ErrNotFound
	}
	if err != nil {
		return Release{}, nil, "", err
	}
	store, err := s.storeFor(ctx, r.Storage)
	if err != nil {
		return Release{}, nil, "", err
	}
	return r, store, s.releaseKey(r), nil
}

// CompareVersions 比较 x.y.z（预发布后缀低于正式版）。
func CompareVersions(a, b string) int {
	pa, sa, _ := strings.Cut(a, "-")
	pb, sb, _ := strings.Cut(b, "-")
	na, nb := strings.Split(pa, "."), strings.Split(pb, ".")
	for i := 0; i < 3; i++ {
		x, y := num(na, i), num(nb, i)
		if x != y {
			if x > y {
				return 1
			}
			return -1
		}
	}
	switch {
	case sa == sb:
		return 0
	case sa == "":
		return 1
	case sb == "":
		return -1
	case sa > sb:
		return 1
	default:
		return -1
	}
}

func num(parts []string, i int) int {
	if i >= len(parts) {
		return 0
	}
	n := 0
	for _, c := range parts[i] {
		if c < '0' || c > '9' {
			break
		}
		n = n*10 + int(c-'0')
	}
	return n
}
