package gen

import (
	"bytes"
	"flag"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"boxctl/internal/groups"
)

// 生成器黄金文件测试（M13-02）：固定输入生成的配置与 testdata/golden 下保存的期望文件逐字节比对。
// 生成器的改动是有意的，用 `go test ./internal/gen -run TestGolden -update` 重新生成，并在提交里审阅差异。
var update = flag.Bool("update", false, "重新生成黄金文件")

func goldenCases() map[string]Input {
	desktop := input()

	router := input()
	router.Router = true
	router.RouteExclude = []string{"192.168.100.0/24"}
	router.ProbePortBase = 19300
	router.Controller = "127.0.0.1:19290"

	gate := input()
	gate.ProbePortBase = 19200
	gate.Gate = &GateInput{Port: 19180, Secret: "g", DirectPort: 19181}

	direct := input()
	direct.Mode = "direct"

	ruleSets := input()
	ruleSets.Router = true
	ruleSets.RuleSets = []RuleSet{
		{Name: "claude", Behavior: "classical", URL: "https://example.com/claude.yaml", Target: "Claude"},
		{Name: "cn", Behavior: "ipcidr", Format: "text", URL: "https://example.com/cn.txt", Target: "DIRECT", NoResolve: true},
		{Type: TypeGeoIP, Name: "LAN", Target: "DIRECT", NoResolve: true},
		{Name: "gone", Behavior: "domain", URL: "https://example.com/gone.yaml", Target: "不存在的组"},
		{Name: "off", Behavior: "domain", URL: "https://example.com/off.yaml", Target: "DIRECT", Disabled: true},
	}
	ruleSets.Groups = append(ruleSets.Groups, groups.Export{Name: "负载", Type: groups.TypeLoadBalance,
		Members: []string{"香港 - 01", "美国 - 01"}, Options: groups.Options{URL: "http://t", Interval: 600, Strategy: "round-robin", Lazy: true}})

	devices := input()
	devices.Router = true
	devices.DeviceRules = []string{"SRC-IP-CIDR,192.168.1.10/32,DIRECT", "SRC-IP-CIDR,192.168.1.11/32,Claude",
		"SUB-RULE,(SRC-IP-CIDR,192.168.1.12/32),dev-192.168.1.12", "SRC-IP-CIDR,192.168.1.13/32,不存在的组"}
	devices.SubRules = map[string][]string{"dev-192.168.1.12": {"DOMAIN-SUFFIX,example.com,REJECT", "DOMAIN-KEYWORD,x,不存在的组"}}
	devices.ExcludeMAC = []string{"aa:bb:cc:dd:ee:ff"}

	return map[string]Input{
		"devices":  devices,
		"desktop":  desktop,
		"router":   router,
		"gate":     gate,
		"direct":   direct,
		"rulesets": ruleSets,
	}
}

func TestGolden(t *testing.T) {
	for name, in := range goldenCases() {
		t.Run(name, func(t *testing.T) {
			r, err := Build(in)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join("testdata", "golden", name+".yaml")
			if *update {
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, r.YAML, 0o644); err != nil {
					t.Fatal(err)
				}
				return
			}
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("没有黄金文件，先运行 go test ./internal/gen -run TestGolden -update：%v", err)
			}
			if !bytes.Equal(r.YAML, want) {
				t.Fatalf("生成结果与 %s 不同；若是有意的改动，用 -update 重新生成后审阅差异。\n%s", path, firstDiff(want, r.YAML))
			}
			// 同一份输入两次生成逐字节相同（判断配置是否真的变了靠这一点）。
			again, _ := Build(in)
			if !bytes.Equal(again.YAML, r.YAML) {
				t.Fatal("生成结果不稳定")
			}
		})
	}
}

func firstDiff(a, b []byte) string {
	al, bl := bytes.Split(a, []byte("\n")), bytes.Split(b, []byte("\n"))
	for i := 0; i < len(al) || i < len(bl); i++ {
		var x, y []byte
		if i < len(al) {
			x = al[i]
		}
		if i < len(bl) {
			y = bl[i]
		}
		if !bytes.Equal(x, y) {
			return "第 " + strconv.Itoa(i+1) + " 行\n期望：" + string(x) + "\n实际：" + string(y)
		}
	}
	return ""
}
