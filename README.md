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
- **网页正文抓取**：`web_fetch` 纯本地抓取网页并转成 Markdown，零额度消耗，不引入无头浏览器。

## 安装

依赖 Go 1.27+。首次运行会自动创建 `data/` 目录与 SQLite 库。

### Windows（PowerShell）

```powershell
cd C:\path\to\WebSearch
.\build.ps1 build     # 编译到 bin\websearch.exe
.\build.ps1 test      # 运行测试
.\build.ps1 all       # vet + test + build
.\build.ps1 doctor    # 三平台连通性自检
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

## 在 Codex 中配置

编辑 `C:\Users\<你的用户名>\.codex\config.toml`，加入：

```toml
[mcp_servers.websearch]
enabled = true
command = 'C:\path\to\WebSearch\bin\websearch.exe'
args = ["serve", "--config", 'C:\path\to\WebSearch\config.yaml']
```

`env_vars` 列出客户端需要透传的环境变量名。若 `config.yaml` 里已填入密钥，这三项可以省略。

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
        "TAVILY_API_KEY": "${TAVILY_API_KEY}"
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

返回归一化后的结果列表，每条包含 `title`、`url`、`snippet`、`score`、`date`，
外加结果条数 `count`。

工具不暴露平台、主题、深度等参数：每次查询由 round-robin 游标在 Exa / Brave /
Tavily 之间轮转，三家额度均匀消耗。某个平台失败时自动降级到下一个。

### `web_fetch`

| 参数 | 类型 | 说明 |
|---|---|---|
| `url` | string | 要抓取的网页 URL，必须是 http 或 https 开头（必填） |
| `max_chars` | int | 返回正文的字符上限，1-200000，默认 20000 |

抓取网页并返回 Markdown 风格的正文，附带 `title`、`url`、`truncated`。

**零额度消耗** —— 抓取不占用 Exa / Brave / Tavily 的搜索额度，两者完全隔离。
纯本地实现，不引入无头浏览器，产物仍是单一二进制。

**能力边界**：只支持服务端渲染的正文。依赖 JavaScript 渲染的页面（部分 SPA）、
需要登录的页面、以及强反爬的站点（Cloudflare 挑战页等）会明确报错，不会静默返回空内容。
遇到这类页面可改用 Tavily Extract 或 Exa Contents 这类服务端抓取方案。

## 命令行诊断

```bash
websearch quota     # 查看各平台已用/剩余/上限/状态/重置时刻
websearch config    # 查看生效配置，密钥脱敏
websearch doctor   # 对三平台做连通性自检
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
internal/provider/   三家平台适配器与归一化结果模型
internal/quota/      周期模型与服务端真值采集
internal/router/     负载均衡、熔断与降级链
internal/search/     web_search 业务逻辑
internal/fetch/      纯本地网页抓取与正文提取
internal/mcp/        MCP 服务器与工具注册
internal/logging/    stderr 结构化日志
```

> **stdio 下 stdout 是 MCP 的 JSON-RPC 通道**，任何写入都会破坏协议帧。
> 本项目所有日志一律走 stderr，请勿在代码中直接 `fmt.Println` 到 stdout。

## License

MIT




