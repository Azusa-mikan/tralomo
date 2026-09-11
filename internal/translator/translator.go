package translator

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// errNoProgress 表示对端在超时时间内没有发送任何数据（既不断开也不响应）。
// 这类失败重试只会再等一个超时周期，因此不重试。
var errNoProgress = errors.New("对端响应超时")

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

// Streamer 是支持流式输出的 Engine，目前只有 AI 引擎实现。
// CLI 在流式模式下会优先用它；不支持流式的引擎照常走 Translate。
type Streamer interface {
	// TranslateStream 把 text 翻译成目标语言 to，并按到达顺序把译文片段交给 emit。
	// emit 返回错误时立即中止。某个分段一旦已产出内容就不会重试，以免重复输出。
	TranslateStream(ctx context.Context, text, to string, emit func(string) error) error
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

// streamProvider 是 provider 的可选扩展：支持流式返回译文片段。
type streamProvider interface {
	// stream 发起一次流式请求，把译文片段按到达顺序交给 emit。
	stream(ctx context.Context, text, to string, emit func(string) error) error
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
		if errors.Is(err, errNoProgress) {
			break
		}
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

// translateChunkStream 流式翻译单个分段。只有在尚未产出任何内容时才重试，
// 否则重试会造成重复输出。
func (c chunkedEngine) translateChunkStream(ctx context.Context, text, to string, emit func(string) error) error {
	sp, ok := c.provider.(streamProvider)
	if !ok {
		return fmt.Errorf("引擎不支持流式输出")
	}

	var lastErr error
	for attempt := 1; attempt <= maxTranslateAttempts; attempt++ {
		emitted := false
		err := sp.stream(ctx, text, to, func(delta string) error {
			emitted = true
			return emit(delta)
		})
		if err == nil {
			return nil
		}
		lastErr = err
		if emitted || errors.Is(err, errNoProgress) {
			// 已经吐出部分译文就无法回退；对端超时则重试也只是再等一轮。
			break
		}
		if attempt < maxTranslateAttempts {
			select {
			case <-time.After(time.Duration(attempt) * time.Second):
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}
	return lastErr
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
