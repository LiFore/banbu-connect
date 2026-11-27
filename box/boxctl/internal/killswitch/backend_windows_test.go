package killswitch

import (
	"context"
	"encoding/base64"
	"os/exec"
	"strings"
	"testing"
	"unicode/utf16"
)

func TestPowerShellScriptsParse(t *testing.T) {
	for name, script := range map[string]string{"interfaces": psInterfaces, "inspect": psInspect, "apply": psApply, "remove": psRemove} {
		t.Run(name, func(t *testing.T) {
			u := utf16.Encode([]rune(psPrelude + script))
			raw := make([]byte, 2*len(u))
			for i, c := range u {
				raw[2*i], raw[2*i+1] = byte(c), byte(c>>8)
			}
			encoded := base64.StdEncoding.EncodeToString(raw)
			check := `$s=[Text.Encoding]::Unicode.GetString([Convert]::FromBase64String('` + encoded + `'));$e=$null;$t=$null;` +
				`$null=[System.Management.Automation.Language.Parser]::ParseInput($s,[ref]$t,[ref]$e);$e.Count`
			out, err := exec.Command(powershellPath(), "-NoProfile", "-Command", check).CombinedOutput()
			if err != nil || strings.TrimSpace(string(out)) != "0" {
				t.Fatalf("解析错误：%v %s", err, out)
			}
		})
	}
}

// 只读操作，不需要管理员权限，不改动系统。
func TestRealReadOnlyCalls(t *testing.T) {
	ctx := context.Background()
	b := windowsBackend{}
	ifaces, err := b.Interfaces(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("物理网卡：%v", ifaces)
	a, err := b.Inspect(ctx, Desired{ListenIP: "127.0.0.2", ListenPort: 53})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("现状：%+v", a)
}

func TestCompareWindows(t *testing.T) {
	d := Desired{Suffixes: []string{"claude.ai"}, CIDRs: []string{"160.79.104.0/23"}, ListenIP: "127.0.0.2", ListenPort: 53, Interfaces: []string{"Ethernet"}}
	good := func() psInspectResult {
		return psInspectResult{
			Namespaces: []string{"claude.ai", ".claude.ai"},
			Servers:    []string{"127.0.0.2"},
			Firewall:   []psFirewallRule{{Description: firewallHash(d), Enabled: "True", Action: "Block"}},
		}
	}
	if a := compareWindows(good(), d); !a.InSync || !a.Installed {
		t.Fatalf("应当一致：%+v", a)
	}
	cases := map[string]func(*psInspectResult){
		"缺 NRPT":  func(r *psInspectResult) { r.Namespaces = nil },
		"DNS 被改":  func(r *psInspectResult) { r.Servers = []string{"8.8.8.8"} },
		"缺防火墙":    func(r *psInspectResult) { r.Firewall = nil },
		"网段变化":    func(r *psInspectResult) { r.Firewall[0].Description = "old" },
		"规则被禁用":   func(r *psInspectResult) { r.Firewall[0].Enabled = "False" },
		"规则被改成放行": func(r *psInspectResult) { r.Firewall[0].Action = "Allow" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			r := good()
			mutate(&r)
			if a := compareWindows(r, d); a.InSync {
				t.Fatal("应当发现不一致")
			}
		})
	}
	r := good()
	r.ProfilesOff = []string{"Private"}
	if a := compareWindows(r, d); !a.InSync || len(a.Warnings) != 1 {
		t.Fatalf("防火墙关闭应当警告：%+v", a)
	}
	empty := compareWindows(psInspectResult{}, Desired{ListenIP: "127.0.0.2"})
	if !empty.InSync || empty.Installed {
		t.Fatalf("没有规则也没有期望时应一致：%+v", empty)
	}
}
