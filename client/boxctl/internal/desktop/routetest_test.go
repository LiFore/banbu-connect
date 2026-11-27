package desktop

import "testing"

func TestParseRouteTarget(t *testing.T) {
	cases := map[string]string{
		"github.com":                          "https://github.com/",
		" GitHub.com:8443 ":                   "https://github.com:8443/",
		"http://example.com/x?y=1":            "http://example.com/x?y=1",
		"https://www.google.com/generate_204": "https://www.google.com/generate_204",
	}
	for in, want := range cases {
		u, err := parseRouteTarget(in)
		if err != nil || u.String() != want {
			t.Fatalf("%q → %v, %v；期望 %s", in, u, err, want)
		}
	}
	for _, bad := range []string{"", "ftp://x.com", "https://"} {
		if _, err := parseRouteTarget(bad); err == nil {
			t.Fatalf("%q 应当报错", bad)
		}
	}
}
