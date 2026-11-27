package nodes

import (
	"strings"
	"testing"

	"boxctl/internal/sub"
)

func one(t *testing.T, text string) Candidate {
	t.Helper()
	cs := ParseImport(text, sub.ValidateWithMihomo)
	if len(cs) != 1 {
		t.Fatalf("%q: %d candidates", text, len(cs))
	}
	return cs[0]
}

// M3-05：ISP 代理的各种写法。
func TestImportISP(t *testing.T) {
	cases := []struct {
		name, in                string
		typ, server, user, pass string
		port                    int
		tls                     bool
	}{
		{"brightdata curl", `curl -i --proxy brd.superproxy.io:33335 --proxy-user brd-customer-hl_x-zone-isp_proxy1:fakepw -k "https://geo.brdtest.com/welcome.txt?product=isp&method=native"`,
			"http", "brd.superproxy.io", "brd-customer-hl_x-zone-isp_proxy1", "fakepw", 33335, false},
		{"curl -x url with encoded pw", `curl -x http://u1:p%40ss@proxy.example.com:8080 https://ip.me`,
			"http", "proxy.example.com", "u1", "p@ss", 8080, false},
		{"curl --proxy= socks5h", `curl --proxy=socks5h://u2:p2@s.example.com:1080 https://ip.me`,
			"socks5", "s.example.com", "u2", "p2", 1080, false},
		{"curl --socks5-hostname -U", `curl --socks5-hostname s.example.com:1081 -U u3:p3 https://ip.me`,
			"socks5", "s.example.com", "u3", "p3", 1081, false},
		{"curl https proxy", `curl -x https://h.example.com:443 https://ip.me`,
			"http", "h.example.com", "", "", 443, true},
		{"curl -xVALUE default port", `curl -xh.example.com https://ip.me`,
			"http", "h.example.com", "", "", 1080, false},
		{"curl multiline", "curl \\\n  -x http://h.example.com:3128 \\\n  -U 'u 4:p\"4' \\\n  https://ip.me",
			"http", "h.example.com", "u 4", `p"4`, 3128, false},
		{"host:port:user:pass", `gw.example.com:22225:user-zone-isp:pa:ss`,
			"http", "gw.example.com", "user-zone-isp", "pa:ss", 22225, false},
		{"user:pass@host:port", `u5:p5@h.example.com:3128`, "http", "h.example.com", "u5", "p5", 3128, false},
		{"socks5 url", `socks5://u6:p6@h.example.com:1080`, "socks5", "h.example.com", "u6", "p6", 1080, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := one(t, c.in)
			if got.Error != "" {
				t.Fatal(got.Error)
			}
			m := got.Mapping
			if got.Kind != KindISP || m["type"] != c.typ || m["server"] != c.server || sub.PortOf(m) != c.port {
				t.Fatalf("got %+v", m)
			}
			u, _ := m["username"].(string)
			p, _ := m["password"].(string)
			if u != c.user || p != c.pass {
				t.Fatalf("user/pass %q/%q", u, p)
			}
			if tls, _ := m["tls"].(bool); tls != c.tls {
				t.Fatalf("tls %v", m["tls"])
			}
			if strings.Contains(got.Summary, c.pass) && c.pass != "" {
				t.Fatal("summary leaks password")
			}
		})
	}
}

func TestImportISPErrors(t *testing.T) {
	for _, in := range []string{
		`curl https://ip.me`,
		`curl -x ftp://h.example.com:21 https://ip.me`,
		`curl -x "http://h.example.com:99999" https://ip.me`,
		`curl -x 'unterminated`,
		`just some text`,
	} {
		if c := one(t, in); c.Error == "" {
			t.Errorf("%q: want error, got %+v", in, c.Mapping)
		}
	}
}

// M3-03：各协议分享链接，每种至少 3 个变体。
func TestImportShareLinks(t *testing.T) {
	const pbk = "DYc6P1WSgMqKoUcV1yBvUNhAXyL9OKYgR8rK0d0Pbkk"
	const uuid = "00000000-0000-0000-0000-000000000001"
	links := map[string][]string{
		"vless": {
			"vless://" + uuid + "@v.example.com:443?encryption=none&security=reality&sni=www.example.com&fp=chrome&pbk=" + pbk + "&sid=01ab&type=tcp&flow=xtls-rprx-vision#%F0%9F%87%AF%F0%9F%87%B5%20JP%20reality",
			"vless://" + uuid + "@v.example.com:443?encryption=none&security=tls&sni=v.example.com&type=ws&path=%2Fws&host=v.example.com#ws",
			"vless://" + uuid + "@v.example.com:443?encryption=none&security=tls&sni=v.example.com&type=grpc&serviceName=gs#grpc",
		},
		"trojan": {
			"trojan://fake-pw@t.example.com:443?sni=t.example.com#tcp",
			"trojan://fake-pw@t.example.com:443?sni=t.example.com&type=ws&path=%2Ftr&host=t.example.com#ws",
			"trojan://fake-pw@t.example.com:443?sni=t.example.com&type=grpc&serviceName=gs#grpc",
		},
		"hysteria2": {
			"hysteria2://fake-pw@h.example.com:8443?sni=h.example.com#plain",
			"hy2://fake-pw@h.example.com:8443?sni=h.example.com&obfs=salamander&obfs-password=ob#obfs",
			"hysteria2://fake-pw@h.example.com:8443?sni=h.example.com&insecure=1&mport=20000-30000#hop",
		},
		"anytls": {
			"anytls://fake-pw@a.example.com:443?sni=a.example.com#a1",
			"anytls://fake-pw@a.example.com:8443?sni=a.example.com&insecure=1#a2",
			"anytls://fake-pw@a.example.com:443#a3",
		},
		"ss": {
			"ss://YWVzLTEyOC1nY206ZmFrZS1wdw@s.example.com:8388#aes",
			"ss://MjAyMi1ibGFrZTMtYWVzLTEyOC1nY206QUFBQUFBQUFBQUFBQUFBQUFBQUFBQT09@s.example.com:8388#2022",
			"ss://Y2hhY2hhMjAtaWV0Zi1wb2x5MTMwNTpmYWtlLXB3@s.example.com:8389#chacha",
		},
		"tuic": {
			"tuic://" + uuid + ":fake-pw@u.example.com:443?sni=u.example.com&congestion_control=bbr&alpn=h3#t1",
			"tuic://" + uuid + ":fake-pw@u.example.com:443?sni=u.example.com&udp_relay_mode=quic#t2",
			"tuic://" + uuid + ":fake-pw@u.example.com:8443?alpn=h3&disable_sni=1#t3",
		},
	}
	for proto, list := range links {
		for i, l := range list {
			c := one(t, l)
			if c.Error != "" || c.Protocol != proto || c.Kind != KindManual {
				t.Errorf("%s[%d]: %+v", proto, i, c)
			}
		}
	}
	if c := one(t, links["vless"][0]); c.Region != "日本" || c.DisplayName != "🇯🇵 JP reality" {
		t.Errorf("name/region: %q %q", c.DisplayName, c.Region)
	}

	// 多行混合，重名自动编号
	cs := ParseImport(links["trojan"][0]+"\n"+links["trojan"][0]+"\n\n# 注释\nfoo://bar", sub.ValidateWithMihomo)
	if len(cs) != 3 || cs[1].DisplayName != "tcp 2" || !strings.Contains(cs[2].Error, "foo://") {
		t.Fatalf("multi: %+v", cs)
	}
}

func TestImportYAML(t *testing.T) {
	block := "name: 自建 日本\ntype: trojan\nserver: t.example.com\nport: 443\npassword: fake-pw\ndialer-proxy: 机场"
	c := one(t, block)
	if c.Error != "" || c.DisplayName != "自建 日本" || c.Region != "日本" || c.Warning == "" {
		t.Fatalf("block: %+v", c)
	}
	if _, ok := c.Mapping["dialer-proxy"]; ok {
		t.Fatal("dialer-proxy kept")
	}
	if c := one(t, `{name: x, type: ss, server: s.example.com, port: 8388, cipher: aes-128-gcm, password: p}`); c.Error != "" {
		t.Fatalf("flow: %+v", c)
	}
	cs := ParseImport("proxies:\n  - {name: a, type: trojan, server: a.example.com, port: 443, password: p}\n  - {name: b, type: trojan, server: b.example.com, port: 443}", sub.ValidateWithMihomo)
	if len(cs) != 2 || cs[0].Error != "" || cs[1].Error == "" { // b 缺密码，校验失败
		t.Fatalf("list: %+v", cs)
	}
	cs = ParseImport("proxies:\n  - {name: i, type: http, server: h.example.com, port: 8080, username: u, password: p}\n  - {name: s, type: socks5, server: s.example.com, port: 1080}\n  - {name: t, type: trojan, server: t.example.com, port: 443, password: p}", sub.ValidateWithMihomo)
	if len(cs) != 3 || cs[0].Kind != KindISP || cs[1].Kind != KindISP || cs[2].Kind != KindManual {
		t.Fatalf("kinds: %+v", cs)
	}
}

func TestShellSplit(t *testing.T) {
	got, err := shellSplit(`curl -U 'a b' "c \"d\"" e\ f`)
	if err != nil || strings.Join(got, "|") != `curl|-U|a b|c "d"|e f` {
		t.Fatalf("%q %v", got, err)
	}
}
