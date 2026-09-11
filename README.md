# tralomo

一个命令行翻译工具，支持 **Google / Bing 免费网页接口** 以及 **OpenAI 兼容的 AI 接口**。

![tralomo 演示](docs/demo.svg)

- 源语言自动检测；目标语言默认取系统 locale，也可用 `-t` 指定
- 文本可作参数传入，也可从标准输入管道读入
- 自动按引擎上限**分段翻译长文本**，并带分段重试
- `run` 子命令可直接运行命令并翻译其输出（合并 stdout 与 stderr）

## 安装

### 一键安装（Linux / macOS）

从最新 Release 下载对应平台与架构的预编译二进制：

```bash
curl -fsSL https://raw.githubusercontent.com/Azusa-mikan/tralomo/main/install.sh | bash
```

默认安装到 `~/.local/bin/tralomo`，可用环境变量调整：

- `INSTALL_DIR=/somewhere` —— 改变安装目录
- `TRALOMO_VERSION=v1.2.3` —— 安装指定版本（默认取最新）

支持的平台：`linux` / `darwin` × `amd64` / `arm64`。

### 从源码构建

需要 Go 1.27+。

```bash
git clone https://github.com/Azusa-mikan/tralomo && cd tralomo
go build -o tralomo .
install -m 0755 tralomo ~/.local/bin/tralomo
```

## 快速开始

```bash
tralomo hello world                 # 翻成系统 locale 对应语言
tralomo -t en 你好世界               # 指定目标语言
tralomo -e bing -t ja "good morning" # 指定引擎
echo "some text" | tralomo -t zh-CN  # 从标准输入读
```

## 用法

```
tralomo [文本] [flags]
tralomo run [flags] -- <命令> [参数...]
```

### 参数与开关

| 开关 | 说明 |
|---|---|
| `-t, --to <语言>` | 目标语言，如 `zh-CN`、`en`、`ja`；不填则用 `TRALOMO_TO`，再否则用系统 locale |
| `-e, --engine <引擎>` | 翻译引擎：`google`、`bing`、`ai`；不填则用 `TRALOMO_ENGINE`，再否则 `google` |
| `--strip-ansi` | 翻译前剥掉 ANSI 转义序列（处理带颜色的命令输出） |
| `--no-stream` | 关闭流式输出；`ai` 引擎默认边翻译边打印 |
| `-h, --help` | 帮助 |

优先级：**命令行开关 > 环境变量 > 内置默认**。

### 翻译命令的输出（`run`）

`run` 会启动子命令，把它 **stdout 与 stderr 合并**后的输出拿去翻译：

```bash
tralomo run -- sysx -h
tralomo run -e bing -t zh-CN -- node -h
tralomo run --strip-ansi -t zh-CN -- ls --color=always
```

若子命令以非零状态退出，仍会先输出译文，再以非零状态退出。

### 处理带颜色的输出

彩色/控制序列不是文本，直接翻译会产生乱码，用 `--strip-ansi` 先剥掉：

```bash
tralomo run --strip-ansi -t zh-CN -- ls --color=always
grep --color=always ... | tralomo --strip-ansi -t zh-CN
```

## 环境变量

| 变量 | 说明 | 默认 |
|---|---|---|
| `TRALOMO_ENGINE` | 默认引擎 | `google` |
| `TRALOMO_TO` | 默认目标语言 | 系统 locale |
| `TRALOMO_AI_BASE_URL` | AI 接口地址（`-e ai` 时**必填**），如 `https://api.openai.com/v1` | 无 |
| `TRALOMO_AI_API_KEY` | AI 的 API key（`-e ai` 时**必填**） | 无 |
| `TRALOMO_AI_MODEL` | AI 模型名（`-e ai` 时**必填**），如 `gpt-4o-mini` | 无 |
| `TRALOMO_AI_IDLE_TIMEOUT` | AI 请求「多久没收到数据就放弃」（可选），如 `60s`、`2m` | `60s` |

## 引擎

| 引擎 | 说明 | 单次请求上限 | 是否需要 key |
|---|---|---|---|
| `google` | Google 翻译网页版免费接口（POST 提交） | 10000 字节 | 否 |
| `bing` | Bing 翻译网页版免费接口（`ttranslatev3`），会话凭证缓存于 `~/.cache/tralomo/bing.json` | 1000 字节 | 否 |
| `ai` | OpenAI 兼容的 `POST {base}/chat/completions` | 4096 token | 是 |

AI 引擎示例：

```bash
TRALOMO_AI_BASE_URL=https://api.openai.com/v1 \
TRALOMO_AI_API_KEY=sk-... \
TRALOMO_AI_MODEL=gpt-4o-mini \
  tralomo -e ai -t zh-CN "hello"
```

> 目标语言用通用语言码即可（如 `zh-CN`）。Bing 对语言码较严格，内部会自动归一化（`zh-CN`→`zh-Hans`、`en-US`→`en` 等）。

`ai` 引擎默认**流式输出**，边翻译边打印，长文本不必等整段返回；加 `--no-stream` 可改为一次性输出（其余引擎不受影响）。

## 长文本与分段

超过单次请求上限的文本会自动切分后逐段翻译，再拼接：

- 优先在**换行处**断开，并保留行尾换行，拼接后还原原有分行
- 单行超长时按 UTF-8 边界硬切
- 每个分段失败时**重试 3 次**（1s / 2s 退避），以应对服务端偶发限流
- AI 引擎的 4096 token 预算已扣除 system 提示词与消息框架开销，保证**单次请求总输入不超限**

## 已知限制

- `google` / `bing` 使用的是**非官方免费接口**，无 SLA，可能失效或被限流，接口结构变化时需要适配。Bing 上限较小且偶发限流，翻译长文本会较慢。
- `run` 子命令**无法翻译交互式 TUI**（vim、htop 等）：输出被重定向后它们不会绘制界面；即便强行捕获，得到的也是终端控制序列。`run` 适合非交互命令（`--help`、构建日志、batch 模式等）。
- 目标语言由各引擎自行校验；非法语言码会返回引擎侧错误。

## 项目结构

```
.
├── main.go                       # 入口
├── cmd/root.go                   # cobra 命令、参数解析、run 子命令
└── internal/
    ├── lang/locale.go            # 系统 locale → 语言码
    └── translator/
        ├── translator.go         # Engine 接口、分段与重试
        ├── google.go             # Google 引擎
        ├── bing.go               # Bing 引擎（含凭证缓存）
        └── ai.go                 # OpenAI 兼容 AI 引擎
```

## 开发

```bash
go build ./...
go vet ./...
gofmt -l .
```

## 许可证

本项目基于 [MIT License](LICENSE) 开源。
