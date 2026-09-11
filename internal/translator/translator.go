package translator

import (
	"context"
	"fmt"
	"strings"
	"time"
)

const userAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36 Edg/120.0.0.0"

// Engine 是对外的翻译引擎接口，自动处理长文本分段。
type Engine interface {
	// Translate 把 text 翻译成目标语言 to（BCP-47，如 zh-CN、en）。
	// 源语言由引擎自动检测。
	Translate(ctx context.Context, text, to string) (Result, error)
}

// Result 是一次翻译的结果。
type Result struct {
	Text string
}

// provider 是各引擎实现的「单次请求」翻译；长文本分段与重试由 chunkedEngine 统一处理。
type provider interface {
	translate(ctx context.Context, text, to string) (Result, error)
	// limit 返回单次请求中「待翻译文本」的容量上限（单位由 measure 决定）。
	// to 供需要按目标语言预留提示词开销的引擎使用。
	limit(to string) int
	// measure 返回 text 占用的容量单位数（字节数或 token 数）。
	measure(text string) int
}

// engineRegistry 是所有可用引擎的注册表，顺序即展示顺序。
var engineRegistry = []struct {
	name    string
	factory func() (Engine, error)
}{
	{"google", func() (Engine, error) { return chunkedEngine{&Google{}}, nil }},
	{"bing", func() (Engine, error) { return chunkedEngine{&Bing{}}, nil }},
	{"ai", newAIEngine},
}

// Engines 返回所有可用引擎的名字。
func Engines() []string {
	names := make([]string, len(engineRegistry))
	for i, e := range engineRegistry {
		names[i] = e.name
	}
	return names
}

// New 按名字创建翻译引擎。
func New(name string) (Engine, error) {
	for _, e := range engineRegistry {
		if e.name == name {
			return e.factory()
		}
	}
	return nil, fmt.Errorf("未知引擎 %q（可选：%s）", name, strings.Join(Engines(), "、"))
}

// maxTranslateAttempts 是单个分段的最大尝试次数，用于应对服务端的偶发限流。
const maxTranslateAttempts = 3

// chunkedEngine 把 provider 包装成带「长文本分段 + 分段重试」能力的 Engine。
type chunkedEngine struct {
	provider provider
}

func (c chunkedEngine) Translate(ctx context.Context, text, to string) (Result, error) {
	chunks := splitText(text, c.provider.limit(to), c.provider.measure)
	parts := make([]string, 0, len(chunks))
	for _, chunk := range chunks {
		res, err := c.translateChunk(ctx, chunk, to)
		if err != nil {
			return Result{}, err
		}
		parts = append(parts, res.Text)
	}
	// 分段保留各自末尾的换行，直接拼接即可还原分行；最后统一去掉首尾空白。
	return Result{Text: strings.TrimSpace(strings.Join(parts, ""))}, nil
}

func (c chunkedEngine) translateChunk(ctx context.Context, text, to string) (Result, error) {
	var lastErr error
	for attempt := 1; attempt <= maxTranslateAttempts; attempt++ {
		res, err := c.provider.translate(ctx, text, to)
		if err == nil {
			return res, nil
		}
		lastErr = err
		if attempt < maxTranslateAttempts {
			select {
			case <-time.After(time.Duration(attempt) * time.Second):
			case <-ctx.Done():
				return Result{}, ctx.Err()
			}
		}
	}
	return Result{}, lastErr
}

// splitText 把文本切成每段 measure 不超过 limit 的若干段，尽量在换行处断开，
// 并保留每段末尾的换行符，以便拼接译文时还原原有分行。
func splitText(text string, limit int, measure func(string) int) []string {
	if limit <= 0 || measure(text) <= limit {
		return []string{text}
	}

	var chunks []string
	var buf strings.Builder

	flush := func() {
		if buf.Len() > 0 {
			chunks = append(chunks, buf.String())
			buf.Reset()
		}
	}

	for _, line := range strings.SplitAfter(text, "\n") {
		if buf.Len() > 0 && measure(buf.String()+line) > limit {
			flush()
		}
		// 单行本身超出上限时，按 UTF-8 边界硬切。
		for measure(line) > limit {
			cut := maxPrefix(line, limit, measure)
			if cut <= 0 {
				break
			}
			chunks = append(chunks, line[:cut])
			line = line[cut:]
		}
		buf.WriteString(line)
	}
	flush()
	return chunks
}

// maxPrefix 返回 line 中满足 measure(前缀) <= limit 的最大 UTF-8 前缀（字节长度）。
func maxPrefix(line string, limit int, measure func(string) int) int {
	if measure(line) <= limit {
		return len(line)
	}
	runes := []rune(line)
	lo, hi, best := 1, len(runes), 0
	for lo <= hi {
		mid := (lo + hi) / 2
		if measure(string(runes[:mid])) <= limit {
			best = mid
			lo = mid + 1
		} else {
			hi = mid - 1
		}
	}
	return len(string(runes[:best]))
}
