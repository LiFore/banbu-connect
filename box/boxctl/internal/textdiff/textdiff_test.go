package textdiff

import "testing"

func TestUnified(t *testing.T) {
	a := "a\nb\nc\nd\ne\nf\ng\nh\n"
	b := "a\nb\nc\nD\ne\nf\ng\nh\ni\n"
	got := Unified(a, b, 1)
	want := "@@ -3,3 +3,3 @@\n c\n-d\n+D\n e\n@@ -8,1 +8,2 @@\n h\n+i\n"
	if got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
	if Unified(a, a, 3) != "" {
		t.Fatal("相同内容没有差异")
	}
	if got := Unified("", "x\n", 3); got != "@@ -1,0 +1,1 @@\n+x\n" {
		t.Fatalf("新增全文：%q", got)
	}
}
