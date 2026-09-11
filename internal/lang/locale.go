package lang

import (
	"os"
	"strings"
)

// SystemTarget 根据系统 locale 环境变量推断默认目标语言，
// 返回 BCP-47 风格的语言码（如 zh-CN、en）。
func SystemTarget() string {
	for _, key := range []string{"LC_ALL", "LC_MESSAGES", "LANG"} {
		code := normalize(os.Getenv(key))
		if code == "" || code == "C" || code == "POSIX" {
			continue
		}
		return code
	}
	return "en"
}

// normalize 把 zh_CN.UTF-8 这类 locale 转成 zh-CN。
func normalize(locale string) string {
	if i := strings.IndexAny(locale, ".@"); i >= 0 {
		locale = locale[:i]
	}
	return strings.ReplaceAll(strings.TrimSpace(locale), "_", "-")
}
