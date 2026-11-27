package objstore

import (
	"strings"
	"testing"
)

func TestPartSize(t *testing.T) {
	cases := []struct {
		size  int64
		part  int64
		parts int
	}{
		{1, 16 << 20, 1},
		{16 << 20, 16 << 20, 1},
		{16<<20 + 1, 16 << 20, 2},
		{100 << 30, 103 << 20, 995}, // 100 GB：片数不超过 1000，片大小取整 MB
	}
	for _, c := range cases {
		part, n := PartSize(c.size)
		if part != c.part || n != c.parts || n > maxParts || part%(1<<20) != 0 {
			t.Errorf("PartSize(%d) = %d, %d；应为 %d, %d", c.size, part, n, c.part, c.parts)
		}
	}
}

func TestCleanKeyAndDisposition(t *testing.T) {
	if _, err := CleanKey("files/../../etc/passwd"); err == nil {
		t.Fatal("不应允许 ..")
	}
	if k, _ := CleanKey("/a//b/./c"); k != "a/b/c" {
		t.Fatalf("CleanKey = %q", k)
	}
	cd := ContentDisposition(`安装 "指南".zip`)
	if !strings.Contains(cd, `filename="__ ____.zip"`) {
		t.Fatalf("缺少 ASCII 文件名：%s", cd)
	}
	if !strings.Contains(cd, "filename*=UTF-8''%E5%AE%89%E8%A3%85%20%22%E6%8C%87%E5%8D%97%22.zip") {
		t.Fatalf("UTF-8 文件名不对：%s", cd)
	}
}
