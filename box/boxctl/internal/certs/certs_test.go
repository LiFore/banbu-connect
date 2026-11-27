package certs

import (
	"crypto/tls"
	"crypto/x509"
	"net"
	"os"
	"path/filepath"
	"testing"
)

func TestEnsureSelfSigned(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "tls")
	certFile, keyFile, err := EnsureSelfSigned(dir, []string{"192.168.1.1", "boxctl.lan", "0.0.0.0"})
	if err != nil {
		t.Fatal(err)
	}
	pair, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range []string{"192.168.1.1", "boxctl.lan", "localhost", "127.0.0.1"} {
		if err := cert.VerifyHostname(h); err != nil {
			t.Errorf("%s: %v", h, err)
		}
	}
	for _, ip := range cert.IPAddresses {
		if ip.Equal(net.IPv4zero) {
			t.Error("unspecified address in SAN")
		}
	}

	// 已存在则不重新生成
	before, _ := os.ReadFile(certFile)
	if _, _, err := EnsureSelfSigned(dir, nil); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(certFile)
	if string(before) != string(after) {
		t.Fatal("certificate regenerated")
	}
}
