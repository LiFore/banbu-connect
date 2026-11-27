package desktop

import (
	"testing"
	"time"

	"boxctl/internal/dist"
)

func TestTokenOnlyAction(t *testing.T) {
	now := time.Now()
	ms := func(d time.Duration) int64 { return now.Add(d).UnixMilli() }
	S := &dist.AICredential{AccessToken: "at-s", ExpiresAt: ms(8 * time.Hour)}
	cases := []struct {
		name    string
		S       *dist.AICredential
		version int64
		logged  bool
		emailOK bool
		W       dist.AICredential
		want    string
	}{
		{"没有托管、本机登录过：上报", nil, 0, true, true, dist.AICredential{AccessToken: "w", RefreshToken: "rt"}, tokUpload},
		{"托管被清除过：不上报", nil, 3, true, true, dist.AICredential{AccessToken: "w", RefreshToken: "rt"}, tokNone},
		{"工作区没登录：写入", S, 1, false, true, dist.AICredential{}, tokImport},
		{"已一致", S, 1, true, true, dist.AICredential{AccessToken: "at-s", ExpiresAt: S.ExpiresAt}, tokApplied},
		{"托管的更新了：写入", S, 2, true, true, dist.AICredential{AccessToken: "old", ExpiresAt: ms(time.Hour)}, tokImport},
		{"本机刚登录（令牌更新）：上报", S, 1, true, true, dist.AICredential{AccessToken: "new", RefreshToken: "rt-l", ExpiresAt: ms(9 * time.Hour)}, tokUpload},
		{"本机留着旧的 Refresh Token：去掉，不上报", S, 1, true, true, dist.AICredential{AccessToken: "old", RefreshToken: "rt-old", ExpiresAt: ms(time.Hour)}, tokImport},
		{"上报后（本地副本带 RT）：去掉本机 RT", &dist.AICredential{AccessToken: "new", RefreshToken: "rt-l", ExpiresAt: ms(9 * time.Hour)}, 2, true, true,
			dist.AICredential{AccessToken: "new", RefreshToken: "rt-l", ExpiresAt: ms(9 * time.Hour)}, tokImport},
		{"登录的不是这个账号：不上报", S, 1, true, false, dist.AICredential{AccessToken: "new", RefreshToken: "rt-l", ExpiresAt: ms(9 * time.Hour)}, tokImport},
		{"托管的只有 Refresh Token 还没续期：不动", &dist.AICredential{}, 1, true, true, dist.AICredential{AccessToken: "x"}, tokNone},
	}
	for _, c := range cases {
		if got := tokenOnlyAction(c.S, c.version, c.logged, c.emailOK, c.W); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}
