package desktop

import (
	"slices"
	"testing"
)

func TestParseNetworkServices(t *testing.T) {
	out := "An asterisk (*) denotes that a network service is disabled.\nWi-Fi\n*Thunderbolt Bridge\nUSB 10/100/1000 LAN\n"
	if got := parseNetworkServices(out); !slices.Equal(got, []string{"Wi-Fi", "USB 10/100/1000 LAN"}) {
		t.Fatalf("%q", got)
	}
}

func TestProxyOnLoopback(t *testing.T) {
	cases := map[string]bool{
		"Enabled: Yes\nServer: 127.0.0.1\nPort: 7890\nAuthenticated Proxy Enabled: 0\n": true,
		"Enabled: Yes\nServer: localhost\nPort: 7890\n":                                 true,
		"Enabled: No\nServer: 127.0.0.1\nPort: 7890\n":                                  false,
		"Enabled: Yes\nServer: proxy.corp.example\nPort: 8080\n":                        false,
		"URL: http://127.0.0.1:33331/commands/pac\nEnabled: Yes\n":                      true,
		"URL: http://wpad.corp.example/proxy.pac\nEnabled: Yes\n":                       false,
		"URL: (null)\nEnabled: No\n":                                                    false,
	}
	for in, want := range cases {
		if got := proxyOnLoopback(in); got != want {
			t.Errorf("%q = %v", in, got)
		}
	}
}
