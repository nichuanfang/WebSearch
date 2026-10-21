// Bing 网页搜索适配器（免密钥兜底）。
//
// 设计要点：
//   - 使用 Bing 的 RSS 结果页，返回标准 XML，解析比 HTML 稳定
//   - 中文等 CJK 查询走 cn.bing.com，其余走 www.bing.com
//   - 不消耗 Exa / Brave / Tavily 的任何额度
//
// 能力边界：这是公开结果页，不是官方 API。Bing 可能对自动化请求返回
// 挑战页或 429，此时如实报错，交由上层进入冷却，不做静默降级。
package provider

import (
	"context"
	"encoding/xml"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode"

	"github.com/nichuanfang/WebSearch/internal/textcut"
)

const (
	// BingName 是兜底平台的标识。
	BingName = "bing"
	// bingGlobalHost 用于非 CJK 查询。
	bingGlobalHost = "https://www.bing.com"
	// bingChinaHost 用于 CJK 查询。
	bingChinaHost = "https://cn.bing.com"
	// bingUserAgent 伪装成常见桌面浏览器，避免空 UA 被直接拒绝。
	bingUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 " +
		"(KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"
	// bingMaxBytes 是 RSS 响应的读取上限。
	bingMaxBytes = 2 << 20
)

// Bing 是免密钥的 Bing 搜索适配器。
type Bing struct {
	Client *http.Client
	// BaseURL 允许测试时替换为假服务器；为空时按查询语言选择市场。
	BaseURL string
}

// NewBing 构造 Bing 适配器。
func NewBing(client *http.Client) *Bing {
	return &Bing{Client: client}
}

// Name 返回平台标识。
func (b *Bing) Name() string { return BingName }

// CostOf 免密钥兜底不消耗任何额度。
func (b *Bing) CostOf(Query) float64 { return 0 }

type bingRSS struct {
	Channel struct {
		Items []struct {
			Title       string `xml:"title"`
			Link        string `xml:"link"`
			Description string `xml:"description"`
			PubDate     string `xml:"pubDate"`
		} `xml:"item"`
	} `xml:"channel"`
}

// Search 执行一次 Bing RSS 搜索。
func (b *Bing) Search(ctx context.Context, q Query) (*Response, error) {
	count := q.NumResults
	if count <= 0 {
		count = 5
	}
	if count > 20 {
		count = 20
	}

	form := url.Values{}
	form.Set("q", q.Text)
	form.Set("format", "rss")
	endpoint := b.endpoint(q.Text) + "/search?" + form.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, NewError(b.Name(), KindInvalidRequest, 0, err.Error())
	}
	req.Header.Set("User-Agent", bingUserAgent)
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
	req.Header.Set("Accept", "application/rss+xml,application/xml;q=0.9,text/xml;q=0.8")

	client := b.Client
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, NewError(b.Name(), KindTransient, 0, err.Error())
	}
	defer resp.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(resp.Body, bingMaxBytes))
	if err != nil {
		return nil, NewError(b.Name(), KindTransient, 0, "读取响应失败: "+err.Error())
	}
	if resp.StatusCode != http.StatusOK {
		return nil, classifyWithHeaders(b.Name(), resp, payload)
	}
	if isBingChallenge(payload) {
		return nil, NewError(b.Name(), KindRateLimited, resp.StatusCode, "Bing 返回反爬挑战页")
	}

	var parsed bingRSS
	if err := xml.Unmarshal(payload, &parsed); err != nil {
		return nil, NewError(b.Name(), KindTransient, resp.StatusCode, "解析 RSS 失败: "+err.Error())
	}
	results := make([]Result, 0, len(parsed.Channel.Items))
	for _, item := range parsed.Channel.Items {
		link := strings.TrimSpace(item.Link)
		if link == "" {
			continue
		}
		results = append(results, Result{
			Title:   textcut.Truncate(strings.TrimSpace(item.Title), maxResultFieldChars),
			URL:     link,
			Snippet: textcut.Truncate(textcut.Collapse(item.Description), maxSnippetChars),
			Date:    normalizeBingDate(item.PubDate),
		})
		if len(results) >= count {
			break
		}
	}
	return &Response{
		Provider:  b.Name(),
		Results:   results,
		CostUnits: 0,
	}, nil
}

// 单条结果的字段上限，避免异常页面把超大文本塞给模型。
const (
	maxResultFieldChars = 300
	maxSnippetChars     = 600
)

// endpoint 按查询是否含 CJK 字符选择市场。
func (b *Bing) endpoint(query string) string {
	if b.BaseURL != "" {
		return b.BaseURL
	}
	if hasCJK(query) {
		return bingChinaHost
	}
	return bingGlobalHost
}

// hasCJK 判断查询是否包含中日韩文字。
func hasCJK(s string) bool {
	for _, r := range s {
		if unicode.Is(unicode.Han, r) ||
			unicode.Is(unicode.Hiragana, r) ||
			unicode.Is(unicode.Katakana, r) ||
			unicode.Is(unicode.Hangul, r) {
			return true
		}
	}
	return false
}

// isBingChallenge 识别常见的反爬挑战页。挑战页不是 RSS，直接解析会失败。
func isBingChallenge(payload []byte) bool {
	if len(payload) == 0 {
		return false
	}
	head := payload
	if len(head) > 8192 {
		head = head[:8192]
	}
	lower := strings.ToLower(string(head))
	return strings.Contains(lower, "<title>captcha</title>") ||
		strings.Contains(lower, "captcha") && strings.Contains(lower, "<html")
}

// normalizeBingDate 把 RSS 时间统一成搜索结果常用的日期字符串。
// Bing 的中文市场会返回“周二, 20 10月 2026 23:01:00 GMT”这类格式，
// 需要先替换本地化星期与时区缩写再解析。解析失败时保留原文，避免丢失信息。
func normalizeBingDate(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	normalized := raw
	for zh, en := range bingZhWeekdays {
		normalized = strings.Replace(normalized, zh, en, 1)
	}
	for zh, en := range bingZhMonths {
		normalized = strings.Replace(normalized, zh, en, 1)
	}
	normalized = strings.Replace(normalized, "GMT", "+0000", 1)
	if t, err := time.Parse("Mon, 2 Jan 2006 15:04:05 -0700", normalized); err == nil {
		return t.UTC().Format("2006-01-02")
	}
	layouts := []string{
		time.RFC1123Z,
		time.RFC1123,
		"Mon, 2 Jan 2006 15:04:05 GMT",
		time.RFC822Z,
		time.RFC822,
	}
	for _, layout := range layouts {
		if t, err := time.Parse(layout, raw); err == nil {
			return t.UTC().Format("2006-01-02")
		}
	}
	return raw
}

// bingZhWeekdays 是 Bing 中文结果里出现的星期写法。
var bingZhWeekdays = map[string]string{
	"周一": "Mon",
	"周二": "Tue",
	"周三": "Wed",
	"周四": "Thu",
	"周五": "Fri",
	"周六": "Sat",
	"周日": "Sun",
	"周天": "Sun",
}

// bingZhMonths 是 Bing 中文结果里的月份写法。
var bingZhMonths = map[string]string{
	"1月":  "Jan",
	"2月":  "Feb",
	"3月":  "Mar",
	"4月":  "Apr",
	"5月":  "May",
	"6月":  "Jun",
	"7月":  "Jul",
	"8月":  "Aug",
	"9月":  "Sep",
	"10月": "Oct",
	"11月": "Nov",
	"12月": "Dec",
}
