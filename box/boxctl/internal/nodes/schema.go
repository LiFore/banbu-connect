package nodes

// 协议表单 schema（M3-02）：前端据此渲染表单，只展示该协议必填与常用字段，
// 其余放“高级”折叠区。字段 Key 为 Mihomo 配置键，嵌套用 "." 连接（如 "reality-opts.public-key"）。
// 未列出的协议仍可通过分享链接 / YAML 导入，只是没有表单。

type FieldType string

const (
	FieldString   FieldType = "string"
	FieldSecret   FieldType = "secret" // 密码类，编辑时回显为掩码
	FieldInt      FieldType = "int"
	FieldBool     FieldType = "bool"
	FieldSelect   FieldType = "select"
	FieldList     FieldType = "list" // 字符串数组，如 alpn
	secretMaskStr           = "******"
)

type Field struct {
	Key      string    `json:"key"`
	Label    string    `json:"label"`
	Type     FieldType `json:"type"`
	Required bool      `json:"required,omitempty"`
	Options  []string  `json:"options,omitempty"`
	Default  any       `json:"default,omitempty"`
	Advanced bool      `json:"advanced,omitempty"`
	Help     string    `json:"help,omitempty"`
}

type ProtocolSchema struct {
	Type   string  `json:"type"`
	Label  string  `json:"label"`
	Fields []Field `json:"fields"`
}

var (
	fServer = Field{Key: "server", Label: "服务器", Type: FieldString, Required: true}
	fPort   = Field{Key: "port", Label: "端口", Type: FieldInt, Required: true}
	fUDP    = Field{Key: "udp", Label: "UDP", Type: FieldBool, Default: true}
	fSNI    = Field{Key: "sni", Label: "SNI", Type: FieldString}
	fSkip   = Field{Key: "skip-cert-verify", Label: "跳过证书校验", Type: FieldBool, Advanced: true,
		Help: "仅在自签证书时开启"}
	fFP = Field{Key: "client-fingerprint", Label: "TLS 指纹", Type: FieldSelect, Advanced: true,
		Options: []string{"", "chrome", "firefox", "safari", "ios", "edge", "random"}}
	fALPN    = Field{Key: "alpn", Label: "ALPN", Type: FieldList, Advanced: true}
	fNetwork = Field{Key: "network", Label: "传输层", Type: FieldSelect, Options: []string{"tcp", "ws", "grpc"}, Default: "tcp"}
	fWSPath  = Field{Key: "ws-opts.path", Label: "WS 路径", Type: FieldString, Help: "传输层为 ws 时填写"}
	fWSHost  = Field{Key: "ws-opts.headers.Host", Label: "WS Host", Type: FieldString, Advanced: true}
	fGRPC    = Field{Key: "grpc-opts.grpc-service-name", Label: "gRPC 服务名", Type: FieldString, Help: "传输层为 grpc 时填写"}
	fUser    = Field{Key: "username", Label: "用户名", Type: FieldSecret}
	fPass    = Field{Key: "password", Label: "密码", Type: FieldSecret}
	fPassReq = Field{Key: "password", Label: "密码", Type: FieldSecret, Required: true}
)

var Schemas = []ProtocolSchema{
	{Type: "anytls", Label: "AnyTLS", Fields: []Field{
		fServer, fPort, fPassReq, fSNI, fUDP, fSkip, fFP, fALPN,
		{Key: "idle-session-check-interval", Label: "空闲会话检查间隔（秒）", Type: FieldInt, Advanced: true},
		{Key: "idle-session-timeout", Label: "空闲会话超时（秒）", Type: FieldInt, Advanced: true},
		{Key: "min-idle-session", Label: "最少空闲会话", Type: FieldInt, Advanced: true},
	}},
	{Type: "hysteria2", Label: "Hysteria2", Fields: []Field{
		fServer, fPort, fPassReq, fSNI,
		{Key: "ports", Label: "端口跳跃", Type: FieldString, Advanced: true, Help: "如 443,8443,5000-6000"},
		{Key: "up", Label: "上行带宽", Type: FieldString, Advanced: true, Help: "如 30 Mbps"},
		{Key: "down", Label: "下行带宽", Type: FieldString, Advanced: true, Help: "如 200 Mbps"},
		{Key: "obfs", Label: "混淆", Type: FieldSelect, Options: []string{"", "salamander"}, Advanced: true},
		{Key: "obfs-password", Label: "混淆密码", Type: FieldSecret, Advanced: true},
		fSkip, fALPN,
	}},
	{Type: "vless", Label: "VLESS", Fields: []Field{
		fServer, fPort,
		{Key: "uuid", Label: "UUID", Type: FieldSecret, Required: true},
		{Key: "flow", Label: "Flow", Type: FieldSelect, Options: []string{"", "xtls-rprx-vision"}},
		{Key: "tls", Label: "TLS", Type: FieldBool, Default: true},
		{Key: "servername", Label: "SNI", Type: FieldString},
		{Key: "reality-opts.public-key", Label: "Reality 公钥", Type: FieldString, Help: "使用 Reality 时填写，需同时开启 TLS"},
		{Key: "reality-opts.short-id", Label: "Reality Short ID", Type: FieldString},
		fFP, fNetwork, fWSPath, fWSHost, fGRPC, fUDP, fSkip, fALPN,
	}},
	{Type: "trojan", Label: "Trojan", Fields: []Field{
		fServer, fPort, fPassReq, fSNI, fNetwork, fWSPath, fWSHost, fGRPC, fUDP, fSkip, fFP, fALPN,
	}},
	{Type: "ss", Label: "Shadowsocks", Fields: []Field{
		fServer, fPort,
		{Key: "cipher", Label: "加密方式", Type: FieldSelect, Required: true, Default: "2022-blake3-aes-128-gcm", Options: []string{
			"2022-blake3-aes-128-gcm", "2022-blake3-aes-256-gcm", "2022-blake3-chacha20-poly1305",
			"aes-128-gcm", "aes-256-gcm", "chacha20-ietf-poly1305", "xchacha20-ietf-poly1305",
		}},
		fPassReq, fUDP,
		{Key: "udp-over-tcp", Label: "UDP over TCP", Type: FieldBool, Advanced: true},
	}},
	{Type: "http", Label: "HTTP 代理", Fields: []Field{
		fServer, fPort, fUser, fPass,
		{Key: "tls", Label: "HTTPS（TLS）", Type: FieldBool},
		fSNI, fSkip,
	}},
	{Type: "socks5", Label: "SOCKS5 代理", Fields: []Field{
		fServer, fPort, fUser, fPass, fUDP,
		{Key: "tls", Label: "TLS", Type: FieldBool, Advanced: true},
		fSkip,
	}},
}
