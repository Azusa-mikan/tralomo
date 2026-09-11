package translator

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	googleEndpoint = "https://translate.google.com/translate_a/single"
	// googleMaxTextLen 是单次请求的文本上限（用 POST 提交，不受 URL 长度限制）。
	googleMaxTextLen = 10000
)

// Google 使用 Google 翻译网页版的免费接口，无需 API key。
// client=dict-chrome-ex 是 Chrome 词典插件使用的客户端标识，比 gtx 更不容易被风控拦截。
type Google struct {
	client *http.Client
}

func (g *Google) limit(string) int { return googleMaxTextLen }

func (g *Google) measure(text string) int { return len(text) }

func (g *Google) translate(ctx context.Context, text, to string) (Result, error) {
	if g.client == nil {
		g.client = &http.Client{Timeout: 20 * time.Second}
	}

	q := url.Values{}
	q.Set("client", "dict-chrome-ex")
	q.Set("dt", "t")
	q.Set("sl", "auto")
	q.Set("tl", to)
	q.Set("q", text)

	// 用 POST 提交 text：长文本走 GET 会把 URL 撑爆，被服务端以 400 拒绝。
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, googleEndpoint, strings.NewReader(q.Encode()))
	if err != nil {
		return Result{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", userAgent)

	resp, err := g.client.Do(req)
	if err != nil {
		return Result{}, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return Result{}, err
	}
	if resp.StatusCode != http.StatusOK {
		return Result{}, fmt.Errorf("google 请求失败：HTTP %d", resp.StatusCode)
	}

	// 响应形如：[[["译文1","原文1",...],["译文2","原文2",...]],null,"en",...]
	var raw []json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return Result{}, fmt.Errorf("解析 google 响应失败：%w", err)
	}
	if len(raw) == 0 {
		return Result{}, fmt.Errorf("google 返回了空响应")
	}

	var segments [][]json.RawMessage
	if err := json.Unmarshal(raw[0], &segments); err != nil {
		return Result{}, fmt.Errorf("解析 google 翻译片段失败：%w", err)
	}

	var sb strings.Builder
	for _, seg := range segments {
		if len(seg) == 0 {
			continue
		}
		var piece string
		if err := json.Unmarshal(seg[0], &piece); err != nil {
			continue
		}
		sb.WriteString(piece)
	}

	return Result{Text: sb.String()}, nil
}
