package secret

import (
	"bytes"
	"errors"
	"path/filepath"
	"testing"
)

func newBox(t *testing.T) *Box {
	key, err := LoadOrCreateKey(filepath.Join(t.TempDir(), "k", "device.key"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := New(key)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestSealOpen(t *testing.T) {
	b := newBox(t)
	plain := []byte("hunter2-password")
	ct, err := b.Seal(plain, "proxy_nodes.secret:n1")
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(ct, plain) {
		t.Fatal("ciphertext contains plaintext")
	}
	got, err := b.Open(ct, "proxy_nodes.secret:n1")
	if err != nil || !bytes.Equal(got, plain) {
		t.Fatalf("open: %q %v", got, err)
	}

	// 换一行的 AAD 不能解开
	if _, err := b.Open(ct, "proxy_nodes.secret:n2"); !errors.Is(err, ErrDecrypt) {
		t.Fatalf("wrong aad: %v", err)
	}
	// 篡改
	ct[len(ct)-1] ^= 1
	if _, err := b.Open(ct, "proxy_nodes.secret:n1"); !errors.Is(err, ErrDecrypt) {
		t.Fatalf("tampered: %v", err)
	}
	if _, err := b.Open([]byte{1, 2}, ""); !errors.Is(err, ErrDecrypt) {
		t.Fatalf("short: %v", err)
	}
}

func TestSealNonDeterministic(t *testing.T) {
	b := newBox(t)
	a, _ := b.Seal([]byte("x"), "")
	c, _ := b.Seal([]byte("x"), "")
	if bytes.Equal(a, c) {
		t.Fatal("same ciphertext twice: nonce reuse")
	}
}

func TestKeyPersistent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "device.key")
	k1, err := LoadOrCreateKey(path)
	if err != nil {
		t.Fatal(err)
	}
	k2, err := LoadOrCreateKey(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(k1, k2) {
		t.Fatal("key changed between loads")
	}
}
