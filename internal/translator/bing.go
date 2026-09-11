package translator

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	bingTranslatorPage = "https://www.bing.com/translator"
	bingTranslateAPI   = "https://www.bing.com/ttranslatev3"
	bingIID            = "translator.5023"

	// 凭证过期前的安全余量，避免用到临界值。
	bingCredsMarginMs = 5 * 60 * 1000

	// bingMaxTextLen 是单次请求的文本上限，Bing 的 ttranslatev3 限制较小且偏严。
	bingMaxTextLen = 1000
)

var (
	bingIGRe    = regexp.MustCompile(`IG:"([A-F0-9]+)"`)
	bingCredsRe = regexp.MustCompile(`params_AbusePreventionHelper\s*=\s*\[(\d+),"([^"]+)",(\d+)\]`)
)

// Bing 使用 Bing 翻译网页版的免费接口，无需 API key。
// 需要先访问翻译页提取会话凭证（IG / key / token），再调用 ttranslatev3。
type Bing struct {
	client *http.Client // 请求翻译页，允许自动重定向
	post   *http.Client // 请求翻译接口，手动跟随重定向以保留 POST 请求体
}

type bingCreds struct {
	IG      string `json:"ig"`
	Key     string `json:"key"`
	Token   string `json:"token"`
	Expires int64  `json:"expires"` // 过期时间（Unix 毫秒）
}

func (b *Bing) limit(string) int { return bingMaxTextLen }

func (b *Bing) measure(text string) int { return len(text) }

func (b *Bing) translate(ctx context.Context, text, to string) (Result, error) {
	if b.client == nil {
		b.client = &http.Client{Timeout: 20 * time.Second}
	}
	if b.post == nil {
		b.post = &http.Client{
			Timeout: 20 * time.Second,
			// 不自动跟随重定向：默认策略会把 302 的 POST 降级成 GET 并丢掉请求体。
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		}
	}

	creds, err := b.credentials(ctx)
	if err != nil {
		return Result{}, err
	}

	form := url.Values{}
	form.Set("fromLang", "auto-detect")
	form.Set("text", text)
	form.Set("key", creds.Key)
	form.Set("token", creds.Token)

	endpoint := fmt.Sprintf("%s?isVertical=1&IG=%s&IID=%s&to=%s",
		bingTranslateAPI, creds.IG, bingIID, url.QueryEscape(toBingLang(to)))
	body, err := b.postForm(ctx, endpoint, form)
	if err != nil {
		return Result{}, err
	}

	var parsed []struct {
		Translations []struct {
			Text string `json:"text"`
		} `json:"translations"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return Result{}, fmt.Errorf("解析 bing 响应失败：%w（原始响应：%s）", err, truncate(string(body), 200))
	}
	if len(parsed) == 0 || len(parsed[0].Translations) == 0 {
		return Result{}, fmt.Errorf("bing 返回了空结果：%s", truncate(string(body), 200))
	}

	return Result{Text: parsed[0].Translations[0].Text}, nil
}

// postForm 发送表单 POST，并在遇到重定向时保留方法体和请求体继续跟随。
// Bing 会按地区把请求 302 到 cn.bing.com 等域名，默认重定向策略会丢失请求体。
func (b *Bing) postForm(ctx context.Context, endpoint string, form url.Values) ([]byte, error) {
	payload := form.Encode()

	for hop := 0; hop < 5; hop++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(payload))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("User-Agent", userAgent)

		resp, err := b.post.Do(req)
		if err != nil {
			return nil, err
		}
		data, readErr := io.ReadAll(resp.Body)
		resp.Body.Close()
		if readErr != nil {
			return nil, readErr
		}

		if resp.StatusCode >= 300 && resp.StatusCode < 400 {
			loc := resp.Header.Get("Location")
			if loc == "" {
				return data, nil
			}
			next, err := resp.Request.URL.Parse(loc)
			if err != nil {
				return nil, err
			}
			endpoint = next.String()
			continue
		}
		return data, nil
	}

	return nil, fmt.Errorf("bing 请求重定向次数过多")
}

// credentials 返回有效的会话凭证，优先读缓存，过期则重新获取。
func (b *Bing) credentials(ctx context.Context) (*bingCreds, error) {
	if c := loadBingCreds(); c != nil && c.Expires-time.Now().UnixMilli() > bingCredsMarginMs {
		return c, nil
	}
	c, err := b.fetchCredentials(ctx)
	if err != nil {
		return nil, err
	}
	saveBingCreds(c)
	return c, nil
}

// fetchCredentials 访问 Bing 翻译页并提取 IG / key / token。
func (b *Bing) fetchCredentials(ctx context.Context) (*bingCreds, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, bingTranslatorPage, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)

	resp, err := b.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	ig := bingIGRe.FindSubmatch(body)
	creds := bingCredsRe.FindSubmatch(body)
	if ig == nil || creds == nil {
		return nil, fmt.Errorf("无法从 bing 页面提取会话凭证（页面结构可能已变更）")
	}

	createdMs, _ := strconv.ParseInt(string(creds[1]), 10, 64)
	ttlMs, _ := strconv.ParseInt(string(creds[3]), 10, 64)

	return &bingCreds{
		IG:      string(ig[1]),
		Key:     string(creds[1]),
		Token:   string(creds[2]),
		Expires: createdMs + ttlMs,
	}, nil
}

func bingCachePath() (string, error) {
	dir, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "tralomo", "bing.json"), nil
}

func loadBingCreds() *bingCreds {
	path, err := bingCachePath()
	if err != nil {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var c bingCreds
	if err := json.Unmarshal(data, &c); err != nil {
		return nil
	}
	return &c
}

func saveBingCreds(c *bingCreds) {
	path, err := bingCachePath()
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return
	}
	data, err := json.Marshal(c)
	if err != nil {
		return
	}
	_ = os.WriteFile(path, data, 0o600)
}

// toBingLang 把通用语言码转成 Bing 需要的格式。
// Bing 不接受带地区的语言码（en-US、ja-JP 会报错），中文只认 zh-Hans / zh-Hant。
func toBingLang(code string) string {
	c := strings.ToLower(code)
	switch {
	case strings.HasPrefix(c, "zh-hant"), strings.HasPrefix(c, "zh-tw"), strings.HasPrefix(c, "zh-hk"):
		return "zh-Hant"
	case strings.HasPrefix(c, "zh"):
		return "zh-Hans"
	}
	if i := strings.Index(c, "-"); i > 0 {
		return c[:i]
	}
	return c
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
