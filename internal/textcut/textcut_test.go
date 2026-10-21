package textcut

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestTruncateKeepsValidUTF8(t *testing.T) {
	s := "这是一段中文正文内容"
	for limit := 1; limit <= len(s)+2; limit++ {
		got := Truncate(s, limit)
		if !utf8.ValidString(got) {
			t.Errorf("limit=%d 截断结果非法 UTF-8: %q", limit, got)
		}
		if len(got) > limit {
			t.Errorf("limit=%d 结果超出上限: len=%d", limit, len(got))
		}
	}
}

func TestTruncateBoundaries(t *testing.T) {
	s := "abc中文"
	if got := Truncate(s, 0); got != "" {
		t.Errorf("limit=0 应返回空, 实际 %q", got)
	}
	if got := Truncate(s, -1); got != "" {
		t.Errorf("负数应返回空, 实际 %q", got)
	}
	if got := Truncate(s, len(s)); got != s {
		t.Errorf("恰好等于长度时应原样返回, 实际 %q", got)
	}
	if got := Truncate(s, len(s)+10); got != s {
		t.Errorf("超出长度时应原样返回, 实际 %q", got)
	}
}

func TestTruncateRunes(t *testing.T) {
	s := "中文abc"
	if got := TruncateRunes(s, 2); got != "中文" {
		t.Errorf("按 rune 截断 = %q, 期望 中文", got)
	}
	if got := TruncateRunes(s, 10); got != s {
		t.Errorf("未超限应原样返回, 实际 %q", got)
	}
}

func TestClamp(t *testing.T) {
	cases := []struct {
		in, want int
	}{
		{0, 100},
		{-5, 100},
		{50, 50},
		{500, 200},
	}
	for _, tc := range cases {
		if got := Clamp(tc.in, 100, 200); got != tc.want {
			t.Errorf("Clamp(%d) = %d, 期望 %d", tc.in, got, tc.want)
		}
	}
}

func TestCollapse(t *testing.T) {
	got := Collapse("a \n\t b   中文\n")
	if got != "a b 中文" {
		t.Errorf("Collapse = %q, 期望 %q", got, "a b 中文")
	}
	if got := Collapse(strings.Repeat(" ", 100)); got != "" {
		t.Errorf("纯空白应折叠为空, 实际 %q", got)
	}
}
