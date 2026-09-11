package cmd

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	"github.com/spf13/cobra"

	"tralomo/internal/lang"
	"tralomo/internal/translator"
)

var (
	flagTo        string
	flagEngine    string
	flagStripANSI bool
	engine        translator.Engine
)

var rootCmd = &cobra.Command{
	Use:   "tralomo [文本]",
	Short: "命令行翻译工具（Google / Bing / OpenAI 兼容 AI）",
	Long: `tralomo 是一个命令行翻译工具，支持 Google / Bing 免费网页接口以及 OpenAI 兼容的 AI 接口。

文本既可以作为参数传入，也可以通过标准输入管道传入。
源语言自动检测；目标语言用 -t 指定，未设置时使用系统 locale。

也可以用 run 子命令直接运行命令，并翻译其输出（stdout 与 stderr 合并）：
  tralomo run -- <命令> [参数...]
输出含 ANSI 颜色/控制序列时，加 --strip-ansi 先剥掉再翻译：
  tralomo run --strip-ansi -- <命令> [参数...]

AI 引擎（-e ai）需要配置环境变量（均必填）：
  TRALOMO_AI_BASE_URL  接口地址，如 https://api.openai.com/v1
  TRALOMO_AI_API_KEY   API key
  TRALOMO_AI_MODEL     模型名，如 gpt-4o-mini`,
	Example: `  tralomo hello world
  tralomo -t en 你好世界
  tralomo -e bing -t ja "good morning"
  echo "some text" | tralomo
  tralomo run -- sysx -h
  TRALOMO_AI_BASE_URL=https://api.openai.com/v1 TRALOMO_AI_API_KEY=sk-... TRALOMO_AI_MODEL=gpt-4o-mini tralomo -e ai -t zh-CN "hello"`,
	Args:              cobra.ArbitraryArgs,
	SilenceUsage:      true,
	PersistentPreRunE: validateFlags,
	RunE:              run,
}

var runCmd = &cobra.Command{
	Use:   "run -- <命令> [参数...]",
	Short: "运行命令并翻译其输出（合并 stdout 与 stderr）",
	Args: func(_ *cobra.Command, args []string) error {
		if len(args) < 1 {
			return fmt.Errorf("run 需要一个命令，例如：tralomo run -- sysx -h")
		}
		return nil
	},
	SilenceUsage: true,
	RunE:         runExec,
}

// Execute 运行 CLI，出错时以非零状态码退出。
func Execute() {
	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}

func init() {
	rootCmd.PersistentFlags().StringVarP(&flagEngine, "engine", "e", envOr("TRALOMO_ENGINE", "google"),
		fmt.Sprintf("翻译引擎：%s（可用环境变量 TRALOMO_ENGINE 设置）", strings.Join(translator.Engines(), "、")))
	rootCmd.PersistentFlags().StringVarP(&flagTo, "to", "t", os.Getenv("TRALOMO_TO"),
		"目标语言，如 zh-CN、en、ja（可用环境变量 TRALOMO_TO 设置，未设置则用系统 locale）")
	rootCmd.PersistentFlags().BoolVar(&flagStripANSI, "strip-ansi", false,
		"翻译前剥掉 ANSI 转义序列（用于处理带颜色的命令输出）")

	rootCmd.AddCommand(runCmd)
}

// envOr 返回环境变量的值，为空时返回 fallback。
func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// validateFlags 在读取输入之前校验参数合法性。
func validateFlags(_ *cobra.Command, _ []string) error {
	e, err := translator.New(flagEngine)
	if err != nil {
		return err
	}
	engine = e
	return nil
}

func run(_ *cobra.Command, args []string) error {
	text, err := readText(args)
	if err != nil {
		return err
	}
	return translateAndPrint(text)
}

// runExec 运行子命令，并把它合并后的 stdout/stderr 拿去翻译。
func runExec(_ *cobra.Command, args []string) error {
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Stdin = os.Stdin
	// Stdout 与 Stderr 指向同一个 writer：os/exec 会把写入串行化，从而得到合并输出。
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf

	runErr := cmd.Run()

	output := strings.TrimRight(buf.String(), "\n")
	if output == "" {
		if runErr != nil {
			return fmt.Errorf("执行命令失败：%w", runErr)
		}
		return fmt.Errorf("命令没有任何输出")
	}

	if err := translateAndPrint(output); err != nil {
		return err
	}
	if runErr != nil {
		return fmt.Errorf("命令以非零状态退出：%w", runErr)
	}
	return nil
}

// translateAndPrint 解析目标语言、翻译并打印结果。
func translateAndPrint(text string) error {
	if flagStripANSI {
		text = stripANSI(text)
	}
	if strings.TrimSpace(text) == "" {
		return fmt.Errorf("没有可翻译的内容")
	}

	to := flagTo
	if to == "" {
		to = lang.SystemTarget()
	}

	res, err := engine.Translate(context.Background(), text, to)
	if err != nil {
		return err
	}

	fmt.Println(res.Text)
	return nil
}

// readText 从参数或标准输入获取待翻译文本。
func readText(args []string) (string, error) {
	if len(args) > 0 {
		return strings.TrimSpace(strings.Join(args, " ")), nil
	}

	// 无参数时从标准输入读取；仅当输入被重定向/管道时，避免在终端下阻塞。
	stat, err := os.Stdin.Stat()
	if err != nil {
		return "", err
	}
	if stat.Mode()&os.ModeCharDevice != 0 {
		return "", nil
	}

	data, err := io.ReadAll(os.Stdin)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
}

// stripANSI 移除 ANSI 转义序列（CSI / OSC / 其它双字符转义），
// 让带颜色、光标控制等输出的命令也能安全翻译。
func stripANSI(s string) string {
	var b strings.Builder
	b.Grow(len(s))

	for i := 0; i < len(s); {
		if s[i] != 0x1b {
			b.WriteByte(s[i])
			i++
			continue
		}
		i++ // 跳过 ESC
		if i >= len(s) {
			break
		}
		switch s[i] {
		case '[': // CSI：跳过参数/中间字节，直到 0x40-0x7E 的终止字节
			i++
			for i < len(s) && (s[i] < 0x40 || s[i] > 0x7e) {
				i++
			}
			if i < len(s) {
				i++
			}
		case ']': // OSC：跳过内容，直到 BEL 或 ST（ESC \）
			i++
			for i < len(s) {
				if s[i] == 0x07 {
					i++
					break
				}
				if s[i] == 0x1b && i+1 < len(s) && s[i+1] == '\\' {
					i += 2
					break
				}
				i++
			}
		default: // 其它 ESC X 形式的双字节转义
			i++
		}
	}
	return b.String()
}
