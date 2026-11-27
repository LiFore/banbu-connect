package dist

import (
	"context"
	"slices"
	"testing"
)

func TestUsersAssignment(t *testing.T) {
	ctx := context.Background()
	s := newServer(t)
	d1, d2, d3 := newDevice(t, s), newDevice(t, s), newDevice(t, s)

	if _, err := s.CreateUser(ctx, UserInput{Name: "  "}); err == nil {
		t.Fatal("姓名为空应该失败")
	}
	zhang, err := s.CreateUser(ctx, UserInput{Name: "张三", Devices: &[]int64{d1.ID, d2.ID}})
	if err != nil || len(zhang.Devices) != 2 {
		t.Fatalf("user = %+v, %v", zhang, err)
	}
	// 一台设备只属于一个用户：转给李四后，张三名下只剩一台。
	li, err := s.CreateUser(ctx, UserInput{Name: "李四", Devices: &[]int64{d2.ID}})
	if err != nil {
		t.Fatal(err)
	}
	users, _ := s.Users(ctx)
	for _, u := range users {
		if u.ID == zhang.ID && !slices.Equal(u.Devices, []int64{d1.ID}) {
			t.Fatalf("张三名下 = %v", u.Devices)
		}
	}
	if _, err := s.CreateUser(ctx, UserInput{Name: "王五", Devices: &[]int64{999}}); err == nil {
		t.Fatal("不存在的设备应该失败")
	}

	// Claude 账号：分配给用户后，用户名下的设备（含之后加入的）都能用，直接分配的设备仍然有效。
	c, err := s.CreateClaudeAccount(ctx, ClaudeAccountInput{Email: "c@gmail.com", Devices: []int64{d3.ID}, Users: []int64{zhang.ID}})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(c.DirectDevices, []int64{d3.ID}) || !slices.Equal(c.Users, []int64{zhang.ID}) || !slices.Equal(c.Devices, []int64{d3.ID, d1.ID}) {
		t.Fatalf("claude = %+v", c)
	}
	authorize(t, s, c.ID)
	if l, _ := s.ClientClaudeAccounts(ctx, d1.ID); len(l) != 1 {
		t.Fatalf("用户名下的设备应该能拿到账号：%v", l)
	}
	if l, _ := s.ClientClaudeAccounts(ctx, d2.ID); len(l) != 0 {
		t.Fatalf("李四的设备不该拿到：%v", l)
	}
	if _, err := s.SetDeviceUser(ctx, d2.ID, zhang.ID); err != nil { // 新设备归到张三名下，立即生效
		t.Fatal(err)
	}
	if l, _ := s.ClientClaudeAccounts(ctx, d2.ID); len(l) != 1 {
		t.Fatalf("加入用户的设备应该能拿到账号：%v", l)
	}
	// 改账号时不传 users 等于清空；直接分配不变。
	c, err = s.UpdateClaudeAccount(ctx, c.ID, ClaudeAccountInput{Email: c.Email, Devices: []int64{d3.ID}})
	if err != nil || !slices.Equal(c.Devices, []int64{d3.ID}) || len(c.Users) != 0 {
		t.Fatalf("update = %+v, %v", c, err)
	}

	// Gemini 账号：同样合并，Users 不占设备上限。
	g, err := s.CreateGeminiAccount(ctx, GeminiAccountInput{Email: "g@gmail.com", MaxDevices: 1, Devices: []int64{d3.ID}, Users: []int64{zhang.ID}})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(g.DirectDevices, []int64{d3.ID}) || len(g.Devices) != 3 || !slices.Equal(g.Users, []int64{zhang.ID}) {
		t.Fatalf("gemini = %+v", g)
	}
	if ok, _ := s.assigned(ctx, g.ID, d1.ID); !ok {
		t.Fatal("用户名下的设备应该分配到 Gemini 账号")
	}

	// 删除用户：设备脱离，分配的账号失去这条分配。
	if err := s.DeleteUser(ctx, zhang.ID); err != nil {
		t.Fatal(err)
	}
	if ok, _ := s.assigned(ctx, g.ID, d1.ID); ok {
		t.Fatal("删除用户后设备不该再有 Gemini 账号")
	}
	if err := s.DeleteUser(ctx, li.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteUser(ctx, li.ID); err == nil {
		t.Fatal("重复删除应该失败")
	}
	if g, _ = s.geminiAccount(ctx, g.ID); len(g.Users) != 0 || len(g.Devices) != 1 {
		t.Fatalf("gemini after delete = %+v", g)
	}
}

func TestClearAIErrors(t *testing.T) {
	ctx := context.Background()
	s := newServer(t)
	a, err := s.CreateClaudeAccount(ctx, ClaudeAccountInput{Email: "e@gmail.com"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.CreateClaudeAccount(ctx, ClaudeAccountInput{Email: "ok@gmail.com"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.Exec(`UPDATE claude_accounts SET last_error = '登录失败', refresh_error = '续期失败' WHERE id = ?`, a.ID); err != nil {
		t.Fatal(err)
	}
	s.authTests = map[string]*AuthTest{
		authTestKey(KindClaude, a.ID): {Kind: KindClaude, AccountID: a.ID, Status: TestFailed},
		authTestKey(KindClaude, b.ID): {Kind: KindClaude, AccountID: b.ID, Status: TestOK},
	}
	res, err := s.ClearAIErrors(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if res.Login != 1 || res.Refresh != 1 || res.Auth != 1 || res.Total != 1 {
		t.Fatalf("res = %+v", res)
	}
	got, _ := s.claudeAccount(ctx, a.ID)
	if got.LastError != "" || got.RefreshError != "" || s.authTest(KindClaude, a.ID) != nil {
		t.Fatalf("错误没清掉：%+v", got)
	}
	if s.authTest(KindClaude, b.ID) == nil {
		t.Fatal("成功的测试结果不该清掉")
	}
	if res, _ = s.ClearAIErrors(ctx); res.Total != 0 {
		t.Fatalf("没有错误时应该是 0：%+v", res)
	}
}
