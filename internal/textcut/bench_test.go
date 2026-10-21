package textcut

import (
	"strings"
	"testing"
)

// BenchmarkTruncate 截断开销，重点是 UTF-8 回退扫描的代价。
func BenchmarkTruncate(b *testing.B) {
	ascii := strings.Repeat("a", 100_000)
	chinese := strings.Repeat("中文内容", 25_000)
	for _, tc := range []struct {
		name string
		s    string
	}{
		{"ascii", ascii},
		{"chinese", chinese},
	} {
		b.Run(tc.name, func(b *testing.B) {
			b.SetBytes(int64(len(tc.s)))
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				_ = Truncate(tc.s, 20_000)
			}
		})
	}
}
