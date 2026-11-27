package dist

import (
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"testing"
)

// fakeSig 生成和 tauri signer 同样结构的 .sig 内容（签名本身是假的，服务端不验签，只读 trusted comment）。
func fakeSig(version, file string) string {
	comment := "timestamp:1763280000\tfile:" + file
	if version != "" {
		comment += "\tversion:" + version
	}
	text := "untrusted comment: signature from tauri secret key\nRUQ...\ntrusted comment: " + comment + "\nabc==\n"
	return base64.StdEncoding.EncodeToString([]byte(text))
}

func TestReleaseSignatureMustMatch(t *testing.T) {
	ctx := context.Background()
	s := newServer(t)
	add := func(version, file, sig string) error {
		_, err := s.AddRelease(ctx, version, "darwin-aarch64", "", file, sig, strings.NewReader("pkg"))
		return err
	}
	var ie *InputError
	// 这次遇到的情况：按 0.1.0 构建的包填成 0.2.0。
	if err := add("0.2.0", "Banbu Connect.app.tar.gz", fakeSig("0.1.0", "Banbu Connect.app.tar.gz")); !errors.As(err, &ie) || !strings.Contains(ie.Msg, "签名里的版本是 0.1.0") {
		t.Fatalf("version mismatch should be rejected: %v", err)
	}
	// 文件和签名不是同一份（比如传了 .dmg）。
	if err := add("0.2.0", "Banbu Connect.dmg", fakeSig("0.2.0", "Banbu Connect.app.tar.gz")); !errors.As(err, &ie) || !strings.Contains(ie.Msg, "签名对应的文件") {
		t.Fatalf("file mismatch should be rejected: %v", err)
	}
	if err := add("0.2.0", "a.tar.gz", "not base64 !!"); !errors.As(err, &ie) {
		t.Fatalf("garbage signature: %v", err)
	}
	if err := add("0.2.0", "Banbu Connect.app.tar.gz", fakeSig("0.2.0", "Banbu Connect.app.tar.gz")); err != nil {
		t.Fatal(err)
	}
	// 旧版 tauri cli 的签名没有 version 字段：不拦。
	if err := add("0.3.0", "c.tar.gz", fakeSig("", "c.tar.gz")); err != nil {
		t.Fatal(err)
	}
}
