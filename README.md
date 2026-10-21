# WebSearch MCP

[![Go](https://img.shields.io/badge/Go-1.27+-00ADD8?logo=go&logoColor=white)](https://go.dev)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)

多平台 AI 搜索网关。在 Exa / Brave / Tavily 三家之间自动负载均衡，精确记录额度，
配额耗尽自动熔断并优雅降级到其他平台。stdio 传输，单二进制，SQLite 记账。

## 特性

- **轮询负载均衡**：单次查询只打一个平台，按 round-robin 游标在三家间轮转，把三家免费额度都用满。
- **硬上限防超限**：每次调用前在 SQLite 事务里预扣额度，并发下也不会超出配置的上限，绝不意外扣费。
- **精确记账**：Exa 按响应体的 `costDollars` 逐次累加实际花费；Tavily 按 credit 计（advanced 为 2）。
- **熔断与降级**：额度耗尽标记 `exhausted`，限速进入冷却，鉴权失败停止重试；单次查询最多尝试 3 个平台。
- **优雅降级**：某个平台挂了自动切到下一个，全部不可用时返回结构化错误说明，便于模型自我纠正。
- **免密钥搜索链**：三家全部不可用时依次走 Parallel Search → TinyFish Search，不消耗付费额度。
- **远程抓取优先**：`web_fetch` 依次走 TinyFish Fetch → Jina Reader → 本地提取，前两档支持 JS 渲染。
- **多进程安全**：`BEGIN IMMEDIATE` 预扣事务，多个 websearch 实例共享同一数据库也不会丢额度记录。

## 安装

依赖 Go 1.27+。首次运行会自动创建 `data/` 目录与 SQLite 库。

### Windows（PowerShell）

```powershell
cd C:\path\to\WebSearch
.\build.ps1 build     # 编译到 bin\websearch.exe
.\build.ps1 test      # 运行测试
.\build.ps1 all       # vet + test + build
.\build.ps1 doctor    # 付费平台与免密钥通道连通性自检
```

### Windows（已装 mingw32-make）

```powershell
cd C:\path\to\WebSearch
mingw32-make build
```

> Windows 上 `make` 不在 PATH 中通常会报 "make 不是内部或外部命令"。
> 你的机器若装有 MinGW，可用 `mingw32-make` 替代；否则直接用上面的 `build.ps1`。

### macOS / Linux

```bash
git clone https://github.com/nichuanfang/WebSearch.git
cd WebSearch
make build
```

### 不用构建工具

```powershell
go build -o bin\websearch.exe ./cmd\websearch
```


## 配置

复制示例配置并填入密钥：

```bash
copy config.example.yaml config.yaml
```

密钥解析顺序：**环境变量 → 配置文件**。推荐用环境变量注入，`config.yaml` 里的 `api_key` 留空：

```yaml
providers:
  exa:
    api_key_env: EXA_API_KEY
    api_key: ""
```

> `config.yaml` 含明文密钥，已在 `.gitignore` 中排除，不应提交到版本库。

### 各平台免费额度申请

| 平台 | 申请地址 | 免费额度 |
|---|---|---|
| Exa | https://dashboard.exa.ai/api-keys | $10/月，`type=lite` 约 2500 次 |
| Brave | https://api-dashboard.search.brave.com/app/plans | $5/月，约 1000 次 |
| Tavily | https://app.tavily.com | 1000 credits/月 |

### 免密钥通道

在 `config.yaml` 的 `routing` 下可配置两条免密钥链：

```yaml
routing:
  strategy: round_robin
  max_attempts: 3
  fallback:
    # 未写时默认开启；设为 false 可关闭。
    enabled: true
    # 通道限流或反爬后的冷却时长。
    cooldown: 10m
    # 搜索顺序：Parallel → TinyFish。
    search: [parallel, tinyfish]
    # 抓取顺序：TinyFish → Jina Reader → 本地。
    fetch: [tinyfish, jina, local]
    # TinyFish 需要密钥：环境变量优先，留空时回落到配置文件。
    tinyfish_api_key_env: TINYFISH_API_KEY
    tinyfish_api_key: ""
```

免密钥通道不消耗任何付费额度，也不写入 `provider_quota`；只有 Exa / Brave /
Tavily 全部不可用时才会触发搜索链，付费平台成功返回空结果时不会补搜。

| 通道 | 用途 | 说明 |
|---|---|---|
| Parallel | 搜索 + 抓取 | 免 key 的官方 MCP，长查询质量最好；匿名层会限流且不公布阈值 |
| TinyFish | 搜索 + 抓取 | 需 key，免费 12,000 次搜索/天、1,000 URL 抓取/天；快且稳定 |
| Bing | 搜索 | 不推荐接入；三词以上长查询会退化为只搜首个关键词 |
| Jina | 抓取 | 免 key，20 次/分钟，能渲染 JS 页面 |
| local | 抓取 | 纯本地提取，无外部依赖；只支持服务端渲染页面 |

任一通道返回 429 或反爬挑战时进入 `cooldown` 冷却，期间自动跳过并继续下一个通道。

TinyFish 未配置密钥时会被自动跳过并记录 warn 日志，不影响其他通道；
需要启用时设置 `TINYFISH_API_KEY` 环境变量或填写 `tinyfish_api_key`。
它的密钥通过 `X-API-Key` 请求头传递，没有匿名模式，搜索与抓取都需要。

### TinyFish 密钥与额度

1. 在 https://agent.tinyfish.ai/api-keys 创建密钥（可用 Google 或邮箱登录）。
2. 配置到环境变量或 `config.yaml`：

```powershell
$env:TINYFISH_API_KEY = "sk-tinyfish-..."
```

免费额度按天重置：搜索 12,000 次、抓取 1,000 个 URL。超出后返回
`INSUFFICIENT_CREDITS`，本工具按限流处理，进入冷却并跳转到下一个通道。

搜索链只有两档，TinyFish 是最后一环：Parallel 限流时会由 TinyFish 接手，
两者都不用时返回统一错误。这也是不接入 Bing 的原因——实测其长查询命中率
仅 18–22%，低于 TinyFish 的 49%，接进来只会拖低整体质量。

## 在 Codex 中配置

编辑 `C:\Users\<你的用户名>\.codex\config.toml`，加入：

```toml
[mcp_servers.websearch]
enabled = true
command = 'C:\path\to\WebSearch\bin\websearch.exe'
args = ["serve", "--config", 'C:\path\to\WebSearch\config.yaml']
```

`env_vars` 列出客户端需要透传的环境变量名（含可选的 `TINYFISH_API_KEY`）。
若 `config.yaml` 里已填入密钥，这些项可以省略。

## 在 Claude Desktop / Cursor 中配置

```json
{
  "mcpServers": {
    "websearch": {
      "command": "C:\\path\\to\\WebSearch\\bin\\websearch.exe",
      "args": ["serve", "--config", "C:\\path\\to\\WebSearch\\config.yaml"],
      "env": {
        "EXA_API_KEY": "${EXA_API_KEY}",
        "BRAVE_API_KEY": "${BRAVE_API_KEY}",
        "TAVILY_API_KEY": "${TAVILY_API_KEY}",
        "TINYFISH_API_KEY": "${TINYFISH_API_KEY}"
      }
    }
  }
}
```

## 工具

### `web_search`

| 参数 | 类型 | 说明 |
|---|---|---|
| `query` | string | 搜索关键词（必填） |
| `num_results` | int | 返回条数，1-20，默认 5 |

返回归一化后的结果列表，每条包含 `title`、`url`、`snippet`、`date`，
外加结果条数 `count`。`score` 仅在 Tavily 返回评分时存在。

工具不暴露平台、主题、深度等参数：每次查询由 round-robin 游标在 Exa / Brave /
Tavily 之间轮转，三家额度均匀消耗。某个平台失败时自动降级到下一个。

三家全部不可用（额度耗尽、鉴权失败或网络故障）时，按 `search` 顺序改走免密钥
通道：先请求 Parallel Search MCP，失败或限流时再退到 TinyFish Search API。
结果与付费平台共用同一套 `count` / `results` 出参，不额外暴露来源字段。

**能力边界**：Parallel 匿名层没有 SLA，也不公布限流阈值，实测高峰期会返回 429
且不带 `Retry-After`；TinyFish 需要密钥，免费额度按天计。任一通道限流后都会
进入冷却（可通过 `routing.fallback.cooldown` 调整），全部不可用时返回统一错误，
并保留各付费平台与免密钥通道的失败原因。

实测参考（9 个中英文长短查询，命中权威域名比例）：Parallel 67–70%，
TinyFish 49%，AnySearch 31%，Bing 18–22%。Parallel 质量最好但偶尔限流，
TinyFish 速度更快（约 0.3s）且额度充足，两者互补。

### `web_fetch`

| 参数 | 类型 | 说明 |
|---|---|---|
| `url` | string | 要抓取的网页 URL，必须是 http 或 https 开头（必填） |
| `max_chars` | int | 返回正文的字符上限，1-200000，默认 20000 |

抓取网页并返回 Markdown 风格的正文，附带 `title`、`url`、`truncated`。

**零额度消耗** —— 抓取不占用 Exa / Brave / Tavily 的搜索额度，两者完全隔离。

抓取按 `fetch` 顺序进行：先用 TinyFish Fetch 渲染（支持 JS 页面，返回 Markdown），
失败或限流时切到 Jina Reader，最后回落到本地提取。远程通道触发 429
后进入冷却，期间自动使用下一个通道。

纯文本与 Markdown 文档（如 `.md`）走原样返回，不再因为「不是 HTML」而失败；
缺少 `<title>` 时用正文首个标题行补位。

**能力边界**：本地提取只支持服务端渲染的正文；需要登录的页面、强反爬站点
（Cloudflare 挑战页等）仍会明确报错，不会静默返回空内容。

实测参考（5 个页面）：TinyFish 全程命中，0.6–2.8s；纯本地对飞书定价这类
JS 渲染页取不到正文，必须依赖远程通道。

## 命令行诊断

```bash
websearch quota     # 查看各平台已用/剩余/上限/状态/重置时刻
websearch config    # 查看生效配置，密钥脱敏
websearch doctor   # 对三平台与免密钥通道做连通性自检
```

`quota` 输出的「真值」列标注剩余量的推导依据：

- `local` —— 纯本地账本（Brave 服务端不限量时、Tavily `/usage` 不实时上报时）
- `server` —— 服务端真值更严格，取服务端值
- `mixed` —— 两者都有，本地闸门更严格

## 额度周期（重要）

三家平台的额度重置规则**各不相同**，实测结论如下：

| 平台 | 周期 | 说明 |
|---|---|---|
| **Exa** | **每月 1 号**归零 | 余额**不累积**，上月未用完作废。建议月末集中使用。 |
| **Brave** | **滚动 31 天**窗口 | 不是自然月。响应头 `x-ratelimit-policy` 实测为 `0;w=2678400`。 |
| **Tavily** | **账号开通日的账单周期**滚动 | 官方未公开开通日，本工具用首次使用日近似。 |

各平台的服务端真值来源差异：

- **Exa**：响应体 `costDollars` 逐次上报实际花费（实测精确到 $0.001），HTTP 402 表示耗尽。
- **Brave**：响应头 `x-ratelimit-limit` / `-remaining` / `-reset` 提供额度与重置倒计时。
  **实测月度上限为 0，表示不限量**，只有 50 QPS 突发限制；真正的约束是 $5 预付信用耗尽，
  而这个数字服务端不告知，所以本地 `monthly_limit` 是唯一防线。
- **Tavily**：`GET /usage` **实测不实时上报**（多次搜索后 `plan_usage` 恒为 0），
  因此本工具以本地记账为准，靠 HTTP 432/433 错误兜底。

各家真实余额以官方后台为准。`websearch quota` 会尽量展示服务端真值。

## 计费细节

| 平台 | 单次消耗 | 说明 |
|---|---|---|
| Exa | $0.004 | `type=lite` + `numResults<=10` + 只请求 `highlights`。超出 10 条按 $1/1k 另计，故内部强制截断到 10。 |
| Brave | $0.005 | 服务端不限制月度次数，约束是 $5 预付信用。 |
| Tavily | 1 credit | 固定 `basic` 深度。 |

**Exa 的 `contents.summary` 绝不会被请求**——它是按页额外计费的 LLM 项（$1/1k pages），
请求它会烧穿 $10 免费额度。

## 开发

```powershell
.\build.ps1 test    # go test ./... -p 1
.\build.ps1 vet     # go vet ./...
.\build.ps1 build   # 产出 bin\websearch.exe
```

> 测试用 `-p 1` 串行执行：Windows 下并行构建测试二进制会偶发文件锁冲突。

正在运行的 MCP 服务会占用 `bin\websearch.exe`。构建脚本先把新产物写到临时文件，
再把被占用的旧文件改名归档（只保留最近 3 份），因此**构建不会因文件占用而失败**，
但新版本要重启 Codex 后才会被加载。需要立刻替换时可强制结束本仓库的实例：

```powershell
.\build.ps1 build -StopRunning
```

> 这也会结束当前 Codex 会话的 MCP 服务，需要重启客户端恢复。

```bash
make test    # 等价于 go test ./... -p 1
make vet
make build
```

### 目录结构

```
cmd/websearch/       子命令入口
build.ps1            Windows 原生构建脚本（无需 make）
internal/config/     YAML 配置与环境变量覆盖
internal/store/      SQLite 额度账本（预扣/退还事务）
internal/provider/   平台适配器（付费三家 + parallel/tinyfish，另有 bing 备用实现）
internal/quota/      周期模型与服务端真值采集
internal/router/     负载均衡、熔断与降级链
internal/search/     web_search 业务逻辑
internal/fetch/      网页抓取链（tinyfish/jina/local）
internal/textcut/    UTF-8 安全截断与空白压缩
internal/mcp/        MCP 服务器与工具注册
internal/logging/    stderr 结构化日志
```

> **stdio 下 stdout 是 MCP 的 JSON-RPC 通道**，任何写入都会破坏协议帧。
> 本项目所有日志一律走 stderr，请勿在代码中直接 `fmt.Println` 到 stdout。

## License

MIT




