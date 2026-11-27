package box

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestParseLeasesAndNeighbors(t *testing.T) {
	ls := parseLeases("1763400000 AA:BB:CC:00:00:01 192.168.1.10 iPhone 01:aa\n0 02:bb:cc:00:00:02 192.168.1.11 * *\n1763400000 00:11:22:33:44:55 fd00::5 x *\n")
	if len(ls) != 2 || ls[0].mac != "aa:bb:cc:00:00:01" || ls[0].host != "iPhone" || ls[1].host != "" {
		t.Fatalf("leases = %+v", ls)
	}
	ns := parseNeighbors("192.168.1.10 lladdr aa:bb:cc:00:00:01 REACHABLE\n192.168.1.99 FAILED\n192.168.1.12 dev br-lan lladdr 00:11:22:33:44:66 STALE\n")
	if len(ns) != 2 || ns[1].ip != "192.168.1.12" || ns[1].state != "STALE" {
		t.Fatalf("neighbors = %+v", ns)
	}
	if !privateMAC("02:bb:cc:00:00:02") || privateMAC("a8:bb:cc:00:00:01") {
		t.Fatal("本地管理地址判断")
	}
}

func TestDevicePolicies(t *testing.T) {
	b, h, sock := setup(t)
	ctx := context.Background()
	leases := filepath.Join(b.opts.DataDir, "leases")
	_ = os.WriteFile(leases, []byte("1763400000 aa:bb:cc:00:00:01 192.168.1.10 ipad *\n1763400000 02:bb:cc:00:00:02 192.168.1.11 phone *\n"), 0o600)
	b.discovery = discovery{leasesPath: leases, neighbors: func(context.Context) []neighbor {
		return []neighbor{{ip: "192.168.1.10", mac: "aa:bb:cc:00:00:01", state: "REACHABLE"}}
	}}

	if w := call(t, h, http.MethodPut, "/api/v1/box/devices/AA:BB:CC:00:00:01", `{"name":"孩子的平板"}`); w.Code != http.StatusNoContent {
		t.Fatalf("备注：%d %s", w.Code, w.Body)
	}
	if w := call(t, h, http.MethodPut, "/api/v1/box/devices/02:bb:cc:00:00:02", `{"bypass":true}`); w.Code != http.StatusNoContent {
		t.Fatalf("绕过内核：%d", w.Code)
	}
	put := func(ip, body string) int {
		return call(t, h, http.MethodPut, "/api/v1/box/policies/"+ip, body).Code
	}
	if c := put("192.168.1.10", `{"egress":"direct","access":"schedule","schedule":[{"days":[6,7],"start":"09:00","end":"21:00"}],"rules":["DOMAIN-SUFFIX,game.example,REJECT"]}`); c != http.StatusOK {
		t.Fatalf("策略：%d", c)
	}
	if c := put("192.168.1.11", `{"egress":"group","egress_group":"不存在"}`); c != http.StatusBadRequest {
		t.Fatalf("不存在的组应拒绝：%d", c)
	}
	if c := put("192.168.1.12", `{"rules":["MATCH,DIRECT"]}`); c != http.StatusBadRequest {
		t.Fatalf("设备规则不能用 MATCH：%d", c)
	}
	if c := put("192.168.1.20", `{"access":"block"}`); c != http.StatusOK {
		t.Fatalf("断网：%d", c)
	}

	rules, sub, exclude := b.deviceConfig(ctx)
	want := []string{"SUB-RULE,(SRC-IP-CIDR,192.168.1.10/32),dev-192.168.1.10", "SRC-IP-CIDR,192.168.1.10/32,DIRECT"}
	if !slices.Equal(rules, want) || !slices.Equal(sub["dev-192.168.1.10"], []string{"DOMAIN-SUFFIX,game.example,REJECT"}) {
		t.Fatalf("rules = %v sub = %v", rules, sub)
	}
	if !slices.Equal(exclude, []string{"02:bb:cc:00:00:02"}) {
		t.Fatalf("exclude = %v", exclude)
	}

	// 断网和时段进 guard 策略，关闭 Kill Switch 时也在。
	call(t, h, http.MethodPost, "/api/v1/box/killswitch/disable", "")
	p, err := b.Policy(ctx)
	if err != nil {
		t.Fatal(err)
	}
	np, err := p.Normalize()
	if err != nil || len(np.Blocks) != 2 || np.Blocks[0].IP != "192.168.1.10" || len(np.Blocks[0].Windows) != 1 || np.Blocks[1].Windows != nil {
		t.Fatalf("blocks = %+v %v", np.Blocks, err)
	}
	b.push()
	if st := guardStatus(t, sock); !st.Installed {
		t.Fatalf("带设备管控的策略应能装上：%+v", st)
	}

	// 列表：在线的排前面；备注、绕过、策略都带上；设置了策略但没有设备用的 IP 单列。
	w := call(t, h, http.MethodGet, "/api/v1/box/devices", "")
	var list []Device
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if len(list) != 3 || list[0].Name != "孩子的平板" || !list[0].Online || list[0].Policy == nil || list[0].Policy.Egress != "direct" {
		t.Fatalf("list[0] = %+v", list[0])
	}
	var phone, orphan *Device
	for i := range list {
		switch {
		case list[i].MAC == "02:bb:cc:00:00:02":
			phone = &list[i]
		case list[i].MAC == "" && list[i].IP == "192.168.1.20":
			orphan = &list[i]
		}
	}
	if phone == nil || !phone.Bypass || !phone.PrivateMAC || phone.Online {
		t.Fatalf("phone = %+v", phone)
	}
	if orphan == nil || orphan.Policy.Access != "block" {
		t.Fatalf("orphan = %+v", orphan)
	}

	// 恢复默认即删除策略。
	if c := put("192.168.1.20", `{}`); c != http.StatusOK {
		t.Fatal(c)
	}
	if ps, _ := b.devicePolicies(ctx); len(ps) != 1 {
		t.Fatalf("恢复默认后应删除：%v", ps)
	}
}
