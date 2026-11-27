package dist

import (
	"context"
	"errors"
	"testing"
)

func TestEnrollApproveClaimBind(t *testing.T) {
	ctx := context.Background()
	s := newServer(t)
	seed(t, s)
	if _, err := s.Publish(ctx, ""); err != nil {
		t.Fatal(err)
	}
	url, fp := tlsServer(t, s)
	c, _ := NewClient(t.TempDir(), "0.2.0", nil)
	info := EnrollRequest{Name: "张三的 MacBook", Platform: "darwin/arm64", OSVersion: "15.2"}

	v, err := c.Enroll(ctx, url, fp, info)
	if err != nil || v.Status != EnrollPending {
		t.Fatalf("申请：%+v %v", v, err)
	}
	list, _ := s.Enrollments(ctx)
	if len(list) != 1 || list[0].Name != "张三的 MacBook" || list[0].AppVersion != "0.2.0" || list[0].OSVersion != "15.2" {
		t.Fatalf("服务端记录：%+v", list)
	}

	// 未授权时刷新：仍在等待。
	if v, bound, err := c.RefreshEnrollment(ctx); bound || err != nil || v.Status != EnrollPending {
		t.Fatalf("未授权：%+v %v %v", v, bound, err)
	}

	// 同一台再次申请：合并成一条，旧密钥失效。
	if _, err := c.Enroll(ctx, url, fp, info); err != nil {
		t.Fatal(err)
	}
	if list, _ = s.Enrollments(ctx); len(list) != 1 {
		t.Fatalf("重复申请应合并：%+v", list)
	}

	// 授权（管理员改了名字）后刷新：领取令牌并绑定，配置同步下来。
	if _, err := s.ApproveEnrollment(ctx, list[0].ID, "研发-张三"); err != nil {
		t.Fatal(err)
	}
	v, bound, err := c.RefreshEnrollment(ctx)
	if err != nil || !bound || v != nil {
		t.Fatalf("授权后应绑定：%+v %v %v", v, bound, err)
	}
	if st := c.Status(); !st.Bound || st.Version == 0 {
		t.Fatalf("绑定状态：%+v", st)
	}
	// 授权时管理员改了名称：绑定后客户端上报的主机名只记到 reported_name，不覆盖名称。
	wantReported := "张三的 MacBook"
	if h, _ := deviceInfo(); h != "" {
		wantReported = h
	}
	devs, _ := s.Devices(ctx)
	if len(devs) != 1 || devs[0].Name != "研发-张三" || !devs[0].NameCustom || devs[0].ReportedName != wantReported {
		t.Fatalf("客户端：%+v", devs)
	}
	if e, _ := c.Enrollment(); e != nil {
		t.Fatal("绑定后应清掉本机的申请")
	}

	// 令牌只能领取一次。
	list, _ = s.Enrollments(ctx)
	if list[0].Status != EnrollClaimed {
		t.Fatalf("应为已领取：%s", list[0].Status)
	}
}

func TestEnrollRejectAndDelete(t *testing.T) {
	ctx := context.Background()
	s := newServer(t)
	url, fp := tlsServer(t, s)
	c, _ := NewClient(t.TempDir(), "0.2.0", nil)
	if _, err := c.Enroll(ctx, url, fp, EnrollRequest{Name: "PC"}); err != nil {
		t.Fatal(err)
	}
	list, _ := s.Enrollments(ctx)
	if err := s.RejectEnrollment(ctx, list[0].ID); err != nil {
		t.Fatal(err)
	}
	if v, _, _ := c.RefreshEnrollment(ctx); v.Status != EnrollRejected {
		t.Fatalf("应为已拒绝：%+v", v)
	}
	if err := s.DeleteEnrollment(ctx, list[0].ID); err != nil {
		t.Fatal(err)
	}
	if v, _, _ := c.RefreshEnrollment(ctx); v.Status != EnrollRejected || v.LastError == "" {
		t.Fatalf("服务端删除后应提示重新申请：%+v", v)
	}
}

func TestEnrollLimitsAndRevokedDevice(t *testing.T) {
	ctx := context.Background()
	s := newServer(t)
	for i := range maxPendingPerIP {
		id := "0123456789abcdef0123456789abcd" + string(rune('a'+i)) + "0"
		if _, err := s.Enroll(ctx, EnrollRequest{InstallID: id, Name: "PC"}, "198.51.100.1"); err != nil {
			t.Fatal(err)
		}
	}
	_, err := s.Enroll(ctx, EnrollRequest{InstallID: "ffffffffffffffffffffffffffffffff", Name: "PC"}, "198.51.100.1")
	if !errors.Is(err, ErrTooManyEnrollments) {
		t.Fatalf("同一 IP 待处理的申请应有上限：%v", err)
	}
	if _, err := s.Enroll(ctx, EnrollRequest{InstallID: "bad", Name: "PC"}, "198.51.100.2"); err == nil {
		t.Fatal("安装 ID 格式不对应拒绝")
	}

	// 授权后客户端被吊销：领取时按拒绝处理。
	tk, err := s.Enroll(ctx, EnrollRequest{InstallID: "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee", Name: "PC"}, "198.51.100.3")
	if err != nil {
		t.Fatal(err)
	}
	e, _ := s.ApproveEnrollment(ctx, tk.ID, "")
	if err := s.RevokeDevice(ctx, *e.DeviceID); err != nil {
		t.Fatal(err)
	}
	if r, _ := s.ClaimEnrollment(ctx, tk.ID, tk.Secret); r.Status != EnrollRejected || r.Token != "" {
		t.Fatalf("吊销后不应发令牌：%+v", r)
	}
	// 重新授权会解除吊销。
	if _, err := s.ApproveEnrollment(ctx, tk.ID, ""); err != nil {
		t.Fatal(err)
	}
	if r, _ := s.ClaimEnrollment(ctx, tk.ID, tk.Secret); r.Token == "" {
		t.Fatalf("重新授权后应能领取：%+v", r)
	}
	if _, err := s.ClaimEnrollment(ctx, tk.ID, "wrong"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("密钥不对应拒绝：%v", err)
	}
}
