// Package textcut 提供按字符边界截断文本的工具。
//
// Go 的字符串切片按字节计，直接 s[:n] 会切断多字节 UTF-8 字符，
// 产生非法编码，模型侧会看到乱码。本包保证截断结果始终是合法 UTF-8。
package textcut

import (
	"strings"
	"unicode/utf8"
)

// Truncate 按字符数上限截断，并保证结果是合法 UTF-8。
// limit<=0 时返回空字符串；未超限时原样返回。
func Truncate(s string, limit int) string {
	if limit <= 0 {
		return ""
	}
	if len(s) <= limit {
		return s
	}
	// 先截到字节位置，再回退到最近的字符边界。
	cut := s[:limit]
	for len(cut) > 0 && !utf8.ValidString(cut) {
		_, size := utf8.DecodeLastRuneInString(cut)
		if size <= 0 {
			break
		}
		cut = cut[:len(cut)-1]
	}
	return cut
}

// TruncateRunes 按字符数（rune）截断，适合需要精确字符数的场景。
func TruncateRunes(s string, limit int) string {
	if limit <= 0 {
		return ""
	}
	if utf8.RuneCountInString(s) <= limit {
		return s
	}
	runes := []rune(s)
	return string(runes[:limit])
}

// Clamp 把请求的字符上限收敛到允许区间。
func Clamp(n, def, max int) int {
	if n <= 0 {
		return def
	}
	if n > max {
		return max
	}
	return n
}

// Collapse 压缩连续空白为单个空格，便于把多段摘要拼成一行。
func Collapse(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
