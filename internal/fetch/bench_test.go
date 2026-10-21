package fetch

import (
	"strings"
	"testing"
)

// makeHTML 生成一个接近真实文档规模的 HTML 页面。
func makeHTML(paragraphs int) string {
	var sb strings.Builder
	sb.WriteString(`<!DOCTYPE html><html><head><title>基准页面</title></head><body><nav>导航</nav><article><h1>标题</h1>`)
	for i := 0; i < paragraphs; i++ {
		sb.WriteString(`<p>这是一段用于基准测试的中文正文内容，包含标点、数字 123 与英文 word。</p>`)
	}
	sb.WriteString(`</article><footer>页脚</footer></body></html>`)
	return sb.String()
}

// BenchmarkExtract 正文本提取开销，随文档规模增长。
func BenchmarkExtract(b *testing.B) {
	for _, n := range []int{50, 500, 2000} {
		doc := makeHTML(n)
		b.Run(sizeLabel(n), func(b *testing.B) {
			b.SetBytes(int64(len(doc)))
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				title, content := Extract(doc)
				if title == "" || content == "" {
					b.Fatal("提取为空")
				}
			}
		})
	}
}

func sizeLabel(paragraphs int) string {
	switch paragraphs {
	case 50:
		return "small_50p"
	case 500:
		return "medium_500p"
	default:
		return "large_2000p"
	}
}
