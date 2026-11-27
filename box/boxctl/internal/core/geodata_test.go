package core

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestSeedGeoDataCopiesBundledFilesOnce(t *testing.T) {
	src, core := t.TempDir(), filepath.Join(t.TempDir(), "core")
	big := bytes.Repeat([]byte{1}, geoMinSize)
	if err := os.WriteFile(filepath.Join(src, "GeoIP.dat"), big, 0o600); err != nil {
		t.Fatal(err)
	}
	// 太小的文件是坏的，不复制。
	if err := os.WriteFile(filepath.Join(src, "GeoSite.dat"), []byte("<html>"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := SeedGeoData(src, core); err != nil {
		t.Fatal(err)
	}
	if !geoPresent(filepath.Join(core, "GeoIP.dat")) {
		t.Fatal("应复制随应用打包的 GeoIP.dat")
	}
	if geoPresent(filepath.Join(core, "GeoSite.dat")) {
		t.Fatal("不完整的文件不应复制")
	}
	// 已有的文件不覆盖：内核可能已经更新过。
	newer := bytes.Repeat([]byte{2}, geoMinSize)
	if err := os.WriteFile(filepath.Join(core, "GeoIP.dat"), newer, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := SeedGeoData(src, core); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(core, "GeoIP.dat")); b[0] != 2 {
		t.Fatal("已有的 geo 文件不应被覆盖")
	}
}
