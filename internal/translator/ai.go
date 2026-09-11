package translator

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/openai/openai-go"
	"github.com/openai/openai-go/option"
	"github.com/pkoukk/tiktoken-go"
	tiktoken_loader "github.com/pkoukk/tiktoken-go-loader"
)

const (
	// aiMaxInputTokens 是单次请求允许的最大输入 token 数（含 system 提示词）。
	aiMaxInputTokens = 4096
	// aiMessageOverhead 预留给 chat 消息框架（role 等字段）的 token。
	aiMessageOverhead = 16
	// defaultAIIdleTimeout 是「多久没收到任何数据就放弃」的默认值。
	defaultAIIdleTimeout = 60 * time.Second
)

// AI 使用 OpenAI 兼容的 /chat/completions 接口做翻译。
// 配置来自环境变量：TRALOMO_AI_BASE_URL、TRALOMO_AI_API_KEY、TRALOMO_AI_MODEL，
// 以及可选的 TRALOMO_AI_IDLE_TIMEOUT。
type AI struct {
	client openai.Client
	model  string

	encOnce sync.Once
	enc     *tiktoken.Tiktoken
}

// aiEngine 在 chunkedEngine 的基础上增加流式输出能力。
type aiEngine struct {
	chunkedEngine
	ai *AI
}

// newAIEngine 构造 AI 引擎，并包装上长文本分段能力。
func newAIEngine() (Engine, error) {
	p, err := newAI()
	if err != nil {
		return nil, err
	}
	return aiEngine{chunkedEngine{p}, p}, nil
}

// TranslateStream 按分段顺序流式翻译：每段内部边收边吐，段与段之间直接拼接。
func (e aiEngine) TranslateStream(ctx context.Context, text, to string, emit func(string) error) error {
	for _, chunk := range splitText(text, e.ai.limit(to), e.ai.measure) {
		if err := e.chunkedEngine.translateChunkStream(ctx, chunk, to, emit); err != nil {
			return err
		}
	}
	return nil
}

func newAI() (*AI, error) {
	baseURL := os.Getenv("TRALOMO_AI_BASE_URL")
	if baseURL == "" {
		return nil, fmt.Errorf("AI 引擎需要设置环境变量 TRALOMO_AI_BASE_URL")
	}
	apiKey := os.Getenv("TRALOMO_AI_API_KEY")
	if apiKey == "" {
		return nil, fmt.Errorf("AI 引擎需要设置环境变量 TRALOMO_AI_API_KEY")
	}
	model := os.Getenv("TRALOMO_AI_MODEL")
	if model == "" {
		return nil, fmt.Errorf("AI 引擎需要设置环境变量 TRALOMO_AI_MODEL")
	}
	idleTimeout, err := aiIdleTimeout()
	if err != nil {
		return nil, err
	}

	client := openai.NewClient(
		option.WithAPIKey(apiKey),
		option.WithBaseURL(baseURL),
		// 给每个响应包一层空闲看门狗，避免上游不吐数据也不关连接时无限阻塞。
		option.WithHTTPClient(&http.Client{
			Transport: idleTimeoutTransport{rt: http.DefaultTransport, timeout: idleTimeout},
		}),
	)
	return &AI{client: client, model: model}, nil
}

// aiIdleTimeout 解析空闲超时：默认 60 秒，可用 TRALOMO_AI_IDLE_TIMEOUT 覆盖（如 90s、2m）。
func aiIdleTimeout() (time.Duration, error) {
	raw := os.Getenv("TRALOMO_AI_IDLE_TIMEOUT")
	if raw == "" {
		return defaultAIIdleTimeout, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("TRALOMO_AI_IDLE_TIMEOUT 需要是正的时间长度（如 60s、2m）")
	}
	return d, nil
}

// idleTimeoutTransport 给每次响应体套上空闲看门狗：
// 只要超过 timeout 没有任何字节到达，就关闭响应体，让阻塞中的 Read 立即返回错误。
type idleTimeoutTransport struct {
	rt      http.RoundTripper
	timeout time.Duration
}

func (t idleTimeoutTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if t.timeout <= 0 {
		return t.rt.RoundTrip(req)
	}
	// 请求挂在一个可取消的上下文上，看门狗才能中断阻塞中的读取。
	ctx, cancel := context.WithCancel(req.Context())
	res, err := t.rt.RoundTrip(req.WithContext(ctx))
	if err != nil {
		cancel()
		return nil, err
	}
	if res.Body == nil {
		cancel()
		return res, nil
	}
	res.Body = &idleTimeoutBody{rc: res.Body, timeout: t.timeout, cancel: cancel}
	return res, nil
}

type idleTimeoutBody struct {
	rc      io.ReadCloser
	timeout time.Duration
	cancel  context.CancelFunc

	mu       sync.Mutex
	timer    *time.Timer
	timedOut bool
}

func (b *idleTimeoutBody) Read(p []byte) (int, error) {
	b.mu.Lock()
	if b.timedOut {
		b.mu.Unlock()
		return 0, b.timeoutErr()
	}
	if b.timer == nil {
		b.timer = time.AfterFunc(b.timeout, b.abort)
	} else {
		b.timer.Reset(b.timeout)
	}
	b.mu.Unlock()

	n, err := b.rc.Read(p)

	b.mu.Lock()
	timedOut := b.timedOut
	b.mu.Unlock()
	if timedOut {
		return n, b.timeoutErr()
	}
	return n, err
}

func (b *idleTimeoutBody) timeoutErr() error {
	return fmt.Errorf("%w（%s 内没有收到数据，可用 TRALOMO_AI_IDLE_TIMEOUT 调整）", errNoProgress, b.timeout)
}

// abort 由看门狗触发：取消请求并关闭响应体，让阻塞中的 Read 立即返回。
func (b *idleTimeoutBody) abort() {
	b.mu.Lock()
	b.timedOut = true
	b.timer = nil
	b.mu.Unlock()

	b.cancel()
	_ = b.rc.Close()
}

func (b *idleTimeoutBody) Close() error {
	b.mu.Lock()
	if b.timer != nil {
		b.timer.Stop()
		b.timer = nil
	}
	b.mu.Unlock()
	b.cancel()
	return b.rc.Close()
}

// limit 返回「待翻译文本」可用的 token 预算：总上限 − system 提示词 − 消息框架开销。
func (a *AI) limit(to string) int {
	budget := aiMaxInputTokens - a.countTokens(a.systemPrompt(to)) - aiMessageOverhead
	if budget < 1 {
		return 1
	}
	return budget
}

func (a *AI) measure(text string) int { return a.countTokens(text) }

// systemPrompt 是固定的翻译指令；因含目标语言，其 token 数随 to 变化。
func (a *AI) systemPrompt(to string) string {
	return fmt.Sprintf("You are a professional translator. Translate the user's text into %s. "+
		"Preserve the original line breaks and formatting. Output only the translation, "+
		"without explanations, quotes, or any extra text.", to)
}

// countTokens 用 tiktoken 统计 token 数。词表在首次调用时惰性加载（离线内置）。
func (a *AI) countTokens(text string) int {
	a.encOnce.Do(func() {
		tiktoken.SetBpeLoader(tiktoken_loader.NewOfflineLoader())
		enc, err := tiktoken.EncodingForModel(a.model)
		if err != nil {
			// 未知模型时回退到最新的通用词表。
			enc, _ = tiktoken.GetEncoding("o200k_base")
		}
		a.enc = enc
	})
	if a.enc == nil {
		// 词表不可用时的兜底估算。
		return len(text)/4 + 1
	}
	return len(a.enc.Encode(text, nil, nil))
}

func (a *AI) translate(ctx context.Context, text, to string) (Result, error) {
	resp, err := a.client.Chat.Completions.New(ctx, a.params(text, to))
	if err != nil {
		return Result{}, fmt.Errorf("AI 请求失败：%w", err)
	}
	if len(resp.Choices) == 0 {
		return Result{}, fmt.Errorf("AI 返回了空结果")
	}
	return Result{Text: resp.Choices[0].Message.Content}, nil
}

// stream 以流式方式请求，把每个增量片段交给 emit。
func (a *AI) stream(ctx context.Context, text, to string, emit func(string) error) error {
	stream := a.client.Chat.Completions.NewStreaming(ctx, a.params(text, to))
	for stream.Next() {
		chunk := stream.Current()
		if len(chunk.Choices) == 0 {
			continue
		}
		if delta := chunk.Choices[0].Delta.Content; delta != "" {
			if err := emit(delta); err != nil {
				return err
			}
		}
	}
	if err := stream.Err(); err != nil {
		return fmt.Errorf("AI 请求失败：%w", err)
	}
	return nil
}

// params 构造 chat 请求参数，供一次性请求与流式请求共用。
func (a *AI) params(text, to string) openai.ChatCompletionNewParams {
	return openai.ChatCompletionNewParams{
		Model: openai.ChatModel(a.model),
		Messages: []openai.ChatCompletionMessageParamUnion{
			openai.SystemMessage(a.systemPrompt(to)),
			openai.UserMessage(text),
		},
		Temperature: openai.Float(0),
	}
}
