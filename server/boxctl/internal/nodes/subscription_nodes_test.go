package nodes_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"boxctl/internal/nodes"
	"boxctl/internal/secret"
	"boxctl/internal/store"
	"boxctl/internal/sub"
	"boxctl/internal/subscriptions"
)

// 订阅写入的节点可被节点服务解密（共用 SecretAAD），且只能改属性不能改配置/删除。
func TestSubscriptionNodesReadableButRestricted(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	db, err := store.Open(ctx, filepath.Join(dir, "boxctl.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	key, _ := secret.LoadOrCreateKey(filepath.Join(dir, "device.key"))
	box, _ := secret.New(key)

	sample, err := os.ReadFile("../sub/testdata/sample_sub.json")
	if err != nil {
		t.Fatal(err)
	}
	subs := subscriptions.New(db, box, "")
	subs.Fetch = func(context.Context, sub.FetchRequest) (sub.FetchResult, error) {
		return sub.FetchResult{Body: sample}, nil
	}
	sb, err := subs.Create(ctx, subscriptions.Input{Name: "jsyun", URL: "https://sub.example.com/x"})
	if err != nil {
		t.Fatal(err)
	}
	p, err := subs.Preview(ctx, sb.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := subs.Apply(ctx, sb.ID, p.ID, false); err != nil {
		t.Fatal(err)
	}

	svc := nodes.New(db, box)
	list, err := svc.List(ctx)
	if err != nil || len(list) != 107 || list[0].SubscriptionName != "jsyun" || list[0].Kind != nodes.KindSubscription {
		t.Fatalf("list %d %v", len(list), err)
	}
	d, err := svc.Get(ctx, list[0].ID, true)
	if err != nil || d.Mapping["password"] != "fake-password-01" {
		t.Fatalf("decrypt sub node: %v %v", err, d.Mapping["password"])
	}

	var ve *nodes.ValidationError
	if _, err := svc.Update(ctx, d.ID, nodes.Input{Kind: nodes.KindManual, DisplayName: "x", Mapping: d.Mapping}); !errors.As(err, &ve) {
		t.Fatalf("update sub node: %v", err)
	}
	if err := svc.Delete(ctx, d.ID, true); !errors.As(err, &ve) {
		t.Fatalf("delete sub node: %v", err)
	}
	if n, skipped, err := svc.DeleteMany(ctx, []string{d.ID}, true); err != nil || n != 0 || skipped != 1 {
		t.Fatalf("batch delete sub node: %d %d %v", n, skipped, err)
	}
	off := false
	if n, err := svc.Patch(ctx, d.ID, nodes.Attrs{Enabled: &off}); err != nil || n.Enabled {
		t.Fatalf("patch sub node: %v", err)
	}
}
