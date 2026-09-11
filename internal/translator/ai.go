package translator

import (
	"context"
	"fmt"
	"os"
	"sync"

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
)

// AI 使用 OpenAI 兼容的 /chat/completions 接口做翻译。
// 配置来自环境变量：TRALOMO_AI_BASE_URL、TRALOMO_AI_API_KEY、TRALOMO_AI_MODEL。
type AI struct {
	client openai.Client
	model  string

	encOnce sync.Once
	enc     *tiktoken.Tiktoken
}

// newAIEngine 构造 AI 引擎，并包装上长文本分段能力。
func newAIEngine() (Engine, error) {
	p, err := newAI()
	if err != nil {
		return nil, err
	}
	return chunkedEngine{p}, nil
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

	client := openai.NewClient(option.WithAPIKey(apiKey), option.WithBaseURL(baseURL))
	return &AI{client: client, model: model}, nil
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
	resp, err := a.client.Chat.Completions.New(ctx, openai.ChatCompletionNewParams{
		Model: openai.ChatModel(a.model),
		Messages: []openai.ChatCompletionMessageParamUnion{
			openai.SystemMessage(a.systemPrompt(to)),
			openai.UserMessage(text),
		},
		Temperature: openai.Float(0),
	})
	if err != nil {
		return Result{}, fmt.Errorf("AI 请求失败：%w", err)
	}
	if len(resp.Choices) == 0 {
		return Result{}, fmt.Errorf("AI 返回了空结果")
	}
	return Result{Text: resp.Choices[0].Message.Content}, nil
}
