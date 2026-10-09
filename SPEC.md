# SPEC: zen2api —— OpenCode Zen 免费档反代（独立 Go 服务）

> 本文档是唯一的实现依据。**逐节实现，不要跳节**。实现完成后按 §14 实跑验证并贴**真实**输出。
> 代码注释与错误信息一律中文。禁止编造任何命令输出。禁止修改 `/zfspool/share/nas/code/cpa-zen`（只读参考）。

## §0 目标与范围

把 **OpenCode Zen 免费档模型**（上游公开、无需 API key 的免费模型）反代成一个**独立运行**的
OpenAI 兼容 HTTP 服务。

- **对外协议**：只 OpenAI 面 —— `GET /v1/models`、`POST /v1/chat/completions`，外加 `GET /healthz`。
  **不做** Anthropic `/v1/messages`，**不做** Responses 面。
- **上游**：`<base_url>/chat/completions`（默认 `https://opencode.ai/zen/v1`）。免费档**无鉴权**，
  但必须伪装 opencode 客户端的指纹（UA / `x-opencode-client` / `x-opencode-session` /
  `bash,glob,grep,read` 四件套工具 / 强制 `stream:true`）—— 这些规则已在仓库现有代码里实现，**不要重新发明**。
- **出口代理池**：支持 `http`/`https`/`socks5`/`socks5h`/`direct` 多条出口，按策略轮转，
  失败指数退避冷却并换下一个。
- **不做**：webui / 面板 / 管理路由、账号池、用量补报、付费档 API key、持久化（唯一落盘物是目录缓存）。

## §1 仓库现状：已有文件与其处置

| 文件 | 处置 |
|---|---|
| `fingerprint.go` | **原样复用**（`applyFingerprint` / `quartetTools` / `decoyTool` 等）。 |
| `restore.go` | **原样复用**（`restoreJSON` / `restoreToolNames`）。 |
| `session.go` | **原样复用**（`sessionIDFor`）。 |
| `helpers.go` | **原样复用**（`bufioReader` / `readAll` / `cloneHeader` / `sjsonSet` / `noTimeoutCtx`）。 |
| `tlshelper.go` | **原样复用**（`cloneTLSConfig` / `tlsClient`）。 |
| `catalog.go` | **小改**：拉目录的 HTTP 出口可注入（§5.3）、UA 改为 `zen2api-catalog/<version>`。逻辑不变。 |
| `router.go` | **小改**：解析时额外接受**裸上游 id**（§5.2）。其余不动。 |
| `proxy.go` | **小改**：配置字段路径改成分组后的 `cfg.Proxy.*` / `cfg.Upstream.FirstByteTimeout`；新增单次 `Do`（§10）。 |
| `fingerprint_test.go` `restore_test.go` `session_test.go` | **原样复用**，**期望值一个都不许改**。 |
| `proxy_test.go` | **小改**：删掉依赖 `pluginabi` 的 `configureExpectError` 辅助函数（§12），其余用例保留。 |

**只读参考**（禁止修改）：`/zfspool/share/nas/code/cpa-zen`。上游请求构造、SSE 聚合、流式中继的
**原始可用实现**都在它的 `plugin.go` 里，按下面的定位照抄结构、把插件 API 换成普通 `net/http`：

| 参考函数 | plugin.go 行 | 本仓库去向 |
|---|---|---|
| `rewriteModel` | 432-456 | `aggregate.go` |
| `parseUpstreamError` / `truncate` | 457-489 | `aggregate.go` |
| `buildUpstreamRequest` / `cloneWithBody` | 490-550 | `upstream.go` |
| `execute`（非流式） | 554-634 | `server.go` + `upstream.go` |
| `proxyFailureSummary` | 637-647 | `aggregate.go` |
| `aggregateSSE` | 650-787 | `aggregate.go` |
| `executeStream`（流式） | 797-922 | `server.go` + `upstream.go` |
| `rewritePayloadModel` | 925-937 | `aggregate.go` |

## §2 交付物（新增文件，全部 `package main`，仓库根平铺）

| 文件 | 内容 |
|---|---|
| `main.go` | flag 解析（**只在 `main()` 里**）、加载配置、建 logger / catalog / proxyPool / router、打印生效配置、起 HTTP 服务、优雅关闭。 |
| `config.go` | **重写**：分组配置结构 + 默认值 + 校验 + `summary()`（脱敏）。保留 `durationValue`、`levelRank`、`floatPtr`、`boolPtr`。 |
| `log.go` | `logger` 类型（level 过滤、可选写文件、并发安全）。 |
| `version.go` | `const version = "0.1.0"`（唯一内容）。 |
| `upstream.go` | `buildUpstreamRequest`、`cloneWithBody`、`executeNonStream`、`executeStreamUpstream`、`relaySSE`、`apiError`（见 §6.5）。 |
| `aggregate.go` | `rewriteModel`、`parseUpstreamError`、`truncate`、`proxyFailureSummary`、`aggregateSSE`、`rewritePayloadModel`。 |
| `server.go` | HTTP 层：路由、鉴权中间件、错误信封、`/healthz`、`/v1/models`、`/v1/chat/completions`、SSE 中继。 |
| `config_test.go` | 重写（新配置结构）。 |
| `server_test.go` | 新增（`httptest` 假上游端到端）。 |
| `upstream_test.go` | 新增（请求构造 + 指纹 + 会话 id + 超时）。 |
| `aggregate_test.go` | 新增（SSE 聚合、错误解析）。 |
| `README.md` | 用户向：功能、快速开始、配置项表、客户端接法、局限。 |
| `config.example.yaml` | **已存在，不要改**（就是全部配置项与默认值）。 |
| `Dockerfile` `.gitignore` `LICENSE` `go.mod` | **已存在，不要改**。 |

## §3 配置（`config.go`）

### 3.1 加载

- 配置文件格式 **YAML**，默认路径 `./config.yaml`，命令行 `-config <path>` 覆盖。
- 只允许两个 flag：`-config`（默认 `config.yaml`）与 `-version`（打印版本号后 exit 0）。
  **`flag.Parse()` 必须写在 `main()` 里，禁止放进 `init()`**（否则 `go test` 会因 `-test.*` 参数直接 FAIL）。
- 解析用 `yaml.NewDecoder` + **`dec.KnownFields(true)`**：配置里出现未知键（拼错）→ 中文报错并**非 0 退出**。
- 配置文件不存在 / 读不了 → 中文报错并**非 0 退出**（`os.Exit(1)`，在 `main()` 里）。
- 「默认非零」的开关/时长一律用 `*bool` / `*int` / `*float64` / `durationValue` 指针形态，
  以区分「没写」与「显式写了 false/0」。`durationValue` 直接沿用现有实现。

### 3.2 结构与默认值（键名与 `config.example.yaml` **逐字一致**）

```go
type config struct {
	Server   serverConfig   `yaml:"server"`
	Upstream upstreamConfig `yaml:"upstream"`
	Proxy    proxyConfig    `yaml:"proxy"`

	// 归一化后的派生值（不是配置项）
	matchRe *regexp.Regexp
	ttl     time.Duration
	modelSet map[string]struct{}
}

type serverConfig struct {
	Listen          string   `yaml:"listen"`            // 默认 "127.0.0.1:8318"
	APIKeys         []string `yaml:"api_keys"`          // 默认 []（空 = 不鉴权）
	MaxBodyBytes    *int64   `yaml:"max_body_bytes"`    // 默认 67108864（64MiB）
	ShutdownTimeout *durationValue `yaml:"shutdown_timeout"` // 默认 10s
	LogLevel        string   `yaml:"log_level"`         // 默认 "info"
	LogFile         string   `yaml:"log_file"`          // 默认 ""（stdout）
}

type upstreamConfig struct {
	BaseURL          *string             `yaml:"base_url"`           // 默认 "https://opencode.ai/zen/v1"（指针：显式空/非法 → 报错）
	ModelPrefix      string              `yaml:"model_prefix"`       // 默认 "oc/"
	Models           []string            `yaml:"models"`             // 默认 []
	MatchRegex       string              `yaml:"match_regex"`        // 默认 `(-free$)|(^big-pickle$)|(^space-bunny)`
	ModelAliases     map[string][]string `yaml:"model_aliases"`      // 默认 {}
	CatalogFile      string              `yaml:"catalog_file"`       // 默认 "./data/catalog.json"
	CatalogRefresh   *durationValue      `yaml:"catalog_refresh"`    // 默认 6h
	CatalogTimeout   *durationValue      `yaml:"catalog_timeout"`    // 默认 10s
	CatalogViaProxy  *bool               `yaml:"catalog_via_proxy"`  // 默认 false
	InjectTools      *bool               `yaml:"inject_tools"`       // 默认 true
	ForceStream      *bool               `yaml:"force_stream"`       // 默认 true
	NonStreamPolicy  string              `yaml:"nonstream_policy"`   // 默认 "force"
	RestoreToolNames *bool               `yaml:"restore_tool_names"` // 默认 true
	UserAgent        string              `yaml:"user_agent"`         // 默认 "opencode/1.18.31"
	ClientName       string              `yaml:"client_name"`        // 默认 "desktop"
	SessionTTL       *durationValue      `yaml:"session_ttl"`        // 默认 30m
	RequestTimeout   *durationValue      `yaml:"request_timeout"`    // 默认 300s
	FirstByteTimeout *durationValue      `yaml:"first_byte_timeout"` // 默认 60s
}

type proxyConfig struct {
	Proxies     []string            `yaml:"proxies"`      // 默认 []
	ProxyFile   string              `yaml:"proxy_file"`   // 默认 ""
	Strategy    string              `yaml:"strategy"`     // 默认 "round_robin"
	MaxAttempts *int                `yaml:"max_attempts"` // 默认 3
	Cooldown    proxyCooldownConfig `yaml:"cooldown"`     // 见下
}

type proxyCooldownConfig struct {
	Base     *durationValue `yaml:"base"`     // 默认 30s
	Factor   *float64       `yaml:"factor"`   // 默认 2.0
	Max      *durationValue `yaml:"max"`      // 默认 10m
	Jitter   *float64       `yaml:"jitter"`   // 默认 0.2
	Statuses []int          `yaml:"statuses"` // 默认 [403,407,429,500,502,503,504]
}
```

保留现有常量与辅助：`levelRank`、`nonStreamForce/Reject/Pass`、`defaultBaseURL`、
`defaultMatchRegex`、`defaultModelPrefix`、`defaultCooldown*`、`defaultCooldownStatuses`、
`defaultProxyMaxAttempts`、`floatPtr`、`boolPtr`、`durationValue`。

### 3.3 校验（全部返回**中文**原因的 error，fail fast）

| 项 | 规则 |
|---|---|
| `server.listen` | 必须能 `net.SplitHostPort`，端口 1..65535；空则用默认。例：`配置错误: server.listen: 非法监听地址 "127.0.0.1"（应为 host:port）` |
| `server.api_keys` | 去空白、去空串、去重。空 → 启动时 `warn`：`未配置 api_keys，/v1 不鉴权（仅建议本机监听时使用）` |
| `server.max_body_bytes` | > 0，否则报错 |
| `server.shutdown_timeout` | > 0 |
| `server.log_level` | ∈ `levelRank`，否则报错 |
| `upstream.base_url` | 必须 `http://` / `https://` 开头。字段是 `*string`：**没写 → 用默认**；**显式写了空串或非 http(s) → 报错**（fail fast）。消费方一律用 `cfg.baseURL()`（已归一化）。 |
| `upstream.user_agent` | 必须含 `opencode/`（大小写不敏感），否则报错（上游 Cloudflare 拦别的 UA） |
| `upstream.client_name` | 空 → 默认 `desktop` |
| `upstream.match_regex` | 必须能编译，否则报错 |
| `upstream.nonstream_policy` | ∈ `force|reject|pass` |
| `upstream.session_ttl` `request_timeout` `first_byte_timeout` `catalog_refresh` `catalog_timeout` | 都 > 0 |
| `proxy.strategy` | ∈ `round_robin|random|least_inflight` |
| `proxy.max_attempts` | < 1 视为 1 |
| `proxy.cooldown.base/max` | > 0 且 `max >= base`；`factor >= 1`；`jitter` ∈ [0,1]；`statuses` 每项 100..599 |
| `models` / `model_aliases` | 去空白、去空项、去重（沿用现有归一化代码） |

- 显式写 `xxx: null` 让指针变 nil 的情况：先补齐成零值 `durationValue` 再判错，**不许 panic**（照抄 cpa-zen 的补齐写法）。

### 3.4 生效配置打印（`func (c *config) summary() string`）

- 一行一个 `key=value`，分段 `[server] [upstream] [proxy]`，值里有空格用 `%q`。
- **脱敏**：`api_keys` 只留前 4 位 + `***`（长度 < 4 时全打 `***`）；代理 URL 里的凭据脱敏成 `user:***@`。
- 由 `main()` 在 logger 就绪后以 **info** 级打印。

## §4 日志（`log.go`）

```go
type logger struct {
	level string        // debug|info|warn|error
	out   io.Writer     // stdout 或 log_file（O_APPEND|O_CREATE|O_WRONLY, 0644）
	mu    sync.Mutex
	file  *os.File      // 打开文件时持有，便于 Close
}

func newLogger(level, file string) (*logger, error)   // file 为空 → os.Stdout
func (l *logger) debug(msg string, fields map[string]any)
func (l *logger) info(msg string, fields map[string]any)
func (l *logger) warn(msg string, fields map[string]any)
func (l *logger) error(msg string, fields map[string]any)
func (l *logger) Close() error
```

- 方法签名必须与上面一致（`proxy.go` / `proxy_test.go` 直接调用 `&logger{level: "debug"}` +
  `logger_.info(msg, nil)`，**不要改它们的调用点**）。
- 格式：`2026-10-09T16:00:00+08:00 INFO  消息 key=value key2="带 空格"`；`fields` 为 nil 时省略尾部。
- 时间用 `time.Now().Format(time.RFC3339)`（本地时区，容器里靠 TZ）。
- 级别过滤用现有 `levelRank`：`levelRank[msgLevel] < levelRank[l.level]` 则丢弃。

## §5 模型目录与路由

### 5.1 暴露集合

`暴露集合 = {上游目录里的 id 命中 match_regex} ∪ {upstream.models 显式列出的 id}`
（显式列出的即使不命中正则也暴露；上游目录里没有的显式 id 也照样暴露）。

### 5.2 客户端调用名（`router.go` 小改）

- 注册名 = `model_prefix + 上游 id`（默认 `oc/mimo-v2.6-flash-free`）。
- 别名（`model_aliases: {上游 id: [额外名...]}`）里的每个额外名各注册一条（不带前缀）。
- **额外接受裸上游 id**：请求 `big-pickle` 等价于 `oc/big-pickle`（独立服务没有名字冲突问题，
  比插件版宽松）。改动点：`resolveUpstreamModel` 与 `matchRoute` 在「不带前缀且不在别名表」时，
  再看一眼 `exposed[requestedModel]`。其余逻辑不变。
- 都解析不到 → 上游 id 为空 → 返回 400（§9.3）。

### 5.3 目录拉取（`catalog.go` 小改）

- 上游目录：`GET <base_url>/models`，公开无鉴权，返回 `{"object":"list","data":[{"id":...}]}`（实测 87 条）。
- `fetch` 增加一个可注入的出口参数：
  ```go
  type httpDoer interface{ Do(*http.Request) (*http.Response, error) }
  func (c *catalog) fetch(ctx context.Context, baseURL string, timeout time.Duration, doer httpDoer) error
  ```
  `doer == nil` → 用默认 `&http.Client{Timeout: timeout}`（直连）；
  `upstream.catalog_via_proxy == true` 时传代理池（§10 的 `pool.Do`）。UA 用 `zen2api-catalog/<version>`。
- 时机：**启动时先拉一次**（失败不致命：用缓存 + `warn`）；此后 `/v1/models` 请求时若
  `age() > catalog_refresh` 则同步重拉（超时 `catalog_timeout`），失败同样用缓存 + `warn`。
- 拉完把结果 `router.setExposed(...)` 同步给路由器（`setExposed` 用暴露集合，不是全量目录）。
- 目录缓存落盘路径 `catalog_file`（父目录不存在要 `MkdirAll`）。

## §6 上游请求构造（`upstream.go`）

```go
// 返回：上游请求、工具改名映射（小写名 → 客户端原拼写）、已改造的 body 副本
func buildUpstreamRequest(cfg *config, req *http.Request, clientModel, upstreamModel string, forceUpstreamStream bool) (*http.Request, map[string]string, []byte, error)
```

1. body 来自客户端请求体（已在 server 层按 `max_body_bytes` 读完），顶层 `model` 改写为上游 id（`rewriteModel`）。
2. 指纹改造：`applyFingerprint(body, "openai", fingerprintOptions{InjectTools: cfg.injectTools(), ForceStream: forceUpstreamStream, ClientStreams: true})`。
   失败只 `warn` 并原样发送。`Renamed` 非空且 `restore_tool_names=true` → 由**调用方**持有并传给
   聚合/中继函数（**不要**再做插件版的全局 rename 表 —— 独立服务里它就在请求作用域内）。
3. 会话 id：`seed = 客户端 X-Request-Id`（非空时）否则 `客户端请求名`；
   `sessionIDFor(seed, cfg.SessionTTL, time.Now())`。
4. URL：`<base_url>/chat/completions`。
5. 请求头**逐条显式设置**：`Content-Type: application/json`、`User-Agent: <user_agent>`、
   `x-opencode-client: <client_name>`、`x-opencode-session: <sessionID>`。
   **绝不透传客户端 `Authorization`**；**不设** `Accept-Encoding: gzip`。
6. 超时：整体预算 `context.WithTimeout(..., request_timeout)`；等响应头由每个代理 transport 的
   `ResponseHeaderTimeout = first_byte_timeout` 约束（在 `buildProxyPool` 里设置）。
7. 请求由代理池发出：`pool.do(ctx, httpReq, body, log)`（现有实现，含多代理重试与冷却）。

### 6.5 upstream.go 对外接口（server.go 只依赖这几个符号，签名不许改）

> 上游基址一律用 `cfg.baseURL()`（`*string` 已归一化，别直接解引用 `cfg.Upstream.BaseURL` 的裸指针）。

```go
// apiError 是已归类好的错误，server.go 直接拿它写 OpenAI 错误信封（§9.3）。
type apiError struct {
	Status  int    // HTTP 状态码
	Code    string // 错误码
	Message string // 中文/上游原文（截断 ≤512 字节）
	Type    string // OpenAI 的 error.type
}

func (e *apiError) Error() string

// 构造上游请求（§6.1-§6.5）。clientReq 是客户端的原始请求（取 X-Request-Id 用）。
func buildUpstreamRequest(cfg *config, clientReq *http.Request, clientModel, upstreamModel string, forceUpstreamStream bool) (*http.Request, map[string]string, []byte, error)

// 每次重试时构造带新 body reader 的请求（proxy.go 的 do 会调用它）。
func cloneWithBody(ctx context.Context, base *http.Request, body []byte) *http.Request

// 非流式：返回（响应体字节, Content-Type, 错误）。
// force / pass 两种策略都在这里分支；reject 由 server.go 提前拦掉。
func executeNonStream(ctx context.Context, cfg *config, log *logger, pool *proxyPool, clientReq *http.Request, body []byte, clientModel, upstreamModel string) ([]byte, string, *apiError)

// 流式：只负责「拿到上游响应体」，返回（上游 body, 上游响应头, 错误）。
// 调用方负责 Close 与逐帧中继；上游非 2xx 时返回 *apiError（此时 body 已关）。
func executeStreamUpstream(ctx context.Context, cfg *config, log *logger, pool *proxyPool, clientReq *http.Request, body []byte, clientModel, upstreamModel string) (io.ReadCloser, map[string]string, *apiError)

// SSE 中继（§8）：按行读 in，只取 data: 行，还原工具名 + 改写 model，逐帧写 out 并 flush。
// in 读到 EOF 视为正常结束；读错误返回 error（调用方记 warn 并结束响应）。
func relaySSE(ctx context.Context, in io.Reader, out io.Writer, flush func(), renamed map[string]string, clientModel string, restoreToolNames bool, log *logger) error
```

## §7 非流式（`server.go` 里调 `upstream.go` 的执行函数）

`nonstream_policy` 三策略：

- **`force`（默认）**：上游仍用 `stream:true` 发，把 SSE 用 `aggregateSSE` 聚合成一个
  `chat.completion` JSON 返回（`Content-Type: application/json`）。
  聚合结果里 `model` 必须是**客户端请求名**，工具名按 `restoreJSON` 还原，`usage` 上游给了就带上。
- **`reject`**：不发上游请求，直接 400：
  `{"error":{"message":"OpenCode Zen 免费档只接受流式请求（stream:true）。请让客户端开启流式，或把配置 upstream.nonstream_policy 改为 force。","type":"invalid_request_error","code":"nonstream_rejected"}}`。
- **`pass`**：不强制 `stream`，把上游**状态码与 body 原样**返回（上游 403 也照原样透出）。

## §8 流式（SSE 中继）

- 客户端 `stream:true` → 响应头：`Content-Type: text/event-stream`、`Cache-Control: no-cache`、
  `Connection: keep-alive`、`X-Accel-Buffering: no`；用 `http.Flusher` **每帧立即 flush**。
- 从上游 body 按行读：只取 `data:` 行，去掉前缀与首尾空白（`: 心跳` / `event:` / 空行全部丢弃）。
  每帧做两件事后写出：① `restore_tool_names=true` 且 `renamed` 非空 → `restoreJSON`；② `rewritePayloadModel` 改 `model` 为客户端请求名。
- 写出格式恒为 `data: <payload>\n\n`；**`[DONE]` 照常转发**。绝不允许出现 `data: data:` 或双前缀。
- 上游非 2xx（还没写出任何帧）→ 返回 §9.3 的 JSON 错误信封（**带上游真实状态码**）。
- 上游流中途读失败 → 记 `warn`，结束响应（客户端看到截断的流），不要 panic。
- 客户端断开（`r.Context().Done()`）→ 结束中继并取消上游请求，释放连接。

## §9 HTTP 服务（`server.go`）

### 9.1 路由

| 方法+路径 | 行为 |
|---|---|
| `GET /healthz` | **公开**，200 `{"status":"ok","version":"<version>","models":<暴露集合大小>,"proxies":<池大小>}` |
| `GET /v1/models` | 鉴权；返回 OpenAI 列表形状，`data[] = {id, object:"model", created:<进程启动 unix 秒>, owned_by:"opencode"}`，`id` 是**客户端调用名**（`前缀+id` 与全部别名），排序去重 |
| `POST /v1/chat/completions` | 鉴权；代理（§6/§7/§8） |
| 其它 `/v1/*` | 鉴权后 404 错误信封 |
| 路径/方法不匹配 | 405 错误信封 |

### 9.2 鉴权

- `server.api_keys` 非空时，`/v1/*` 必须带 `Authorization: Bearer <key>`；也接受 `x-api-key: <key>`
  与不带 `Bearer ` 前缀的裸 key。比较用 `crypto/subtle.ConstantTimeCompare`。
- 不通过 → 401 + 错误信封（`code: "invalid_api_key"`）。`api_keys` 为空 → 直接放行（启动已 warn）。
- `/healthz` 永远不鉴权；先短路它，避免日志被健康检查噪声淹没。

### 9.3 错误信封（统一函数）

```go
func writeError(w http.ResponseWriter, status int, code, message, errType string)
// 体：{"error":{"message":"...","type":"...","code":"...","param":null}}
```

| 场景 | status | code |
|---|---|---|
| 未找到模型 | 400 | `model_not_found`（message 含请求名与可用模型数量） |
| body 不是合法 JSON / 没有字符串 `model` | 400 | `invalid_request` |
| body 超过 `max_body_bytes` | 413 | `request_too_large` |
| 未鉴权 | 401 | `invalid_api_key` |
| 方法不对 | 405 | `method_not_allowed` |
| 上游 4xx/5xx | **上游状态码** | 上游 `error.type`/`error.code`，取不到用 `upstream_error`（message 取上游 message，截断 ≤512 字节） |
| 全部出口失败（传输层） | 503 | `upstream_unreachable`（message 里带每个代理的失败摘要） |
| 内部异常 | 500 | `internal_error` |

- 每个 handler 都要 `defer recover()`（或统一中间件）把 panic 变成 500 错误信封，**不能让进程崩**。

### 9.4 其它

- 请求体：`io.LimitReader` 读满 `max_body_bytes`；超限即 413（`http.MaxBytesReader` 也可）。
- 响应头**不复制**上游头（`Content-Type` 由我们设）。
- 每个请求一条 **info** 日志：`请求: client=<请求名> upstream=<上游id> stream=<bool> proxy=<host:port|direct> status=<code> ms=<总耗时> first_byte_ms=<首字节耗时>`。
- 优雅关闭：`signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)` → `srv.Shutdown(带 shutdown_timeout 的 ctx)` → 关 logger 文件。

## §10 代理池（`proxy.go` 小改）

- 配置字段路径改为分组形态：`cfg.Proxy.Proxies` / `cfg.Proxy.ProxyFile` / `cfg.Proxy.Strategy` /
  `cfg.Proxy.Cooldown` / `cfg.Upstream.FirstByteTimeout`；`cfg.proxyMaxAttempts()` 保留在 `config.go`（读 `cfg.Proxy.MaxAttempts`）。
- `buildProxyPool(cfg *config, log *logger)` 的**校验与行为一字不改**（非法条目 / 缺 proxy_file → 中文报错；
  空 → 隐式 `direct`；去重；启动 info 打印池摘要且凭据脱敏）。
- 新增单次调用（给目录拉取用）：
  ```go
  // Do 选一条代理发一次请求：失败（传输层错误或状态码命中冷却表）→ 冷却该代理并返回错误；
  // 成功（2xx/3xx）→ 清冷却并返回响应。不做多代理重试。
  func (p *proxyPool) Do(req *http.Request) (*http.Response, error)
  ```
  它满足 §5.3 的 `httpDoer` 接口。

## §11 main.go

```
1. flag.Parse()（-config / -version）；-version → 打印 "<name> <version>" 后 exit 0
   （`const version` 定义在 `version.go`，不在 main.go）
2. 读配置 → 失败：fmt.Fprintln(os.Stderr, err) + os.Exit(1)
3. newLogger(cfg.Server.LogLevel, cfg.Server.LogFile)
4. info 打印：版本、生效配置 summary()
5. loadCatalog(cfg.Upstream.CatalogFile) → 首次 fetch（失败 warn）
6. buildProxyPool → 失败：error 日志 + os.Exit(1)
7. newRouter(prefix, aliases) + setExposed(目录命中的 id ∪ 显式 models)
8. http.Server{Addr: listen, Handler: routes, ReadHeaderTimeout: 30s} + ListenAndServe
   → 监听失败（端口占用等）：中文 error 日志 + os.Exit(1)
9. 等 SIGINT/SIGTERM → 优雅关闭
```

`const version = "0.1.0"`（定义在 `version.go`，同时用于 `/healthz`、启动日志、目录 UA）。

## §12 测试要求

### 12.1 必须继续全绿（**断言与期望值一个都不许改**）

`fingerprint_test.go`、`restore_test.go`、`session_test.go`、`proxy_test.go`。

`proxy_test.go` 需要两处**纯机械**改动（只动这两处，别的行不要碰）：

1. 删掉 `configureExpectError`（它依赖 `pluginabi`）与 `pluginabi` import，
   新增等价辅助函数 `buildPoolExpectError(t, cfgYAML, want)`：`decodeConfig` → `buildProxyPool`
   → 断言 error 非 nil 且 `strings.Contains(err.Error(), want)`；把 3 个调用点改成它。
2. 配置结构已分组，所以 fixture 里的 YAML 键要**只改嵌套层级**，值一律不动：
   `proxies:` → `proxy:\n  proxies:`、`proxy_file:` → `proxy:\n  proxy_file:`（共 7 处）。
   `proxy_file` 的临时文件路径、代理条目、断言字符串（如 `proxies[1]`、`不支持的代理地址`、
   `proxy_file`）都保持原样。

### 12.2 新增测试（用 `httptest` 假上游，**不依赖真实网络**）

1. **配置**：只写必需字段 → 默认值全部正确（逐字段断言）；显式 `false` / `0` 覆盖默认值；
   非法 `log_level` / `nonstream_policy` / `proxy.strategy` / `base_url` / `listen` / 未知键 → 报错且信息含中文原因。
2. **模型解析**：`oc/<id>`、别名、裸上游 id 三种请求名都能解析；未暴露的名字 → 400。
3. **鉴权**：无 key → 401；错 key → 401；对 key → 200；`/healthz` 无 key → 200。
4. **流式中继**：假上游吐一段真实形状 SSE（含 `content` 分片、`tool_calls` 分片、`finish_reason`、
   `usage`、`data: [DONE]`）→ 断言：客户端收到的帧顺序与内容一致、**没有** `data: data:`、
   `[DONE]` 被转发、`Content-Type: text/event-stream`、响应体里 `model` 是客户端请求名。
5. **非流式聚合**：同一段 SSE 走 `stream:false` → 断言 `object=chat.completion`、
   `message.content` 拼接正确、`tool_calls` 按 index 归并、`finish_reason`、`usage`、`model` 正确。
6. **非流式 `reject`** → 400 + `code=nonstream_rejected`；`pass` → 上游状态码原样透出。
7. **上游错误**：假上游返回 403 + `{"error":{"type":"FreeTierError","message":"..."}}`
   → 客户端收到 403 且 `code=FreeTierError`。
8. **代理失败换下一个**：池里第一条指向必然连不上的地址（如 `http://127.0.0.1:1`），
   第二条是假上游 → 请求最终成功，且第一条进入冷却（断言 `coolUntil` 在未来）。
9. **超大 body** → 413。
10. **工具名还原**：客户端声明 `Bash`（大写）→ 断言发往上游的 body 里工具名是小写 `bash`，
    且返回给客户端的响应里又变回 `Bash`。

## §13 明确排除（不要做）

1. 不做 webui / 面板 / `/admin` / 管理路由。
2. 不做 Anthropic `/v1/messages`、不做 Responses 面。
3. 不做付费档 API key、不做上游鉴权、不做账号池、不做用量补报。
4. 不改复用文件的既有逻辑与既有测试期望（§1 列出的「小改」除外）。
5. 不改 `config.example.yaml` / `Dockerfile` / `.gitignore` / `LICENSE` / `go.mod`。
6. 不引入新依赖（只用 go.mod 里已有的 gjson / sjson / golang.org/x/net / yaml.v3）。
7. 不批量 `gofmt` 无关文件；不动仓库外的任何文件（尤其 `/zfspool/share/nas/code/cpa-zen` 与 `/zfspool/data/ai/**`）。

## §14 验证（必须实跑，贴**真实**输出；跑不通就如实说，禁止编造）

```bash
go build -buildvcs=false -o bin/zen2api .
go vet -buildvcs=false ./...
gofmt -l .                                  # 必须无输出
go test ./... 2>&1 | tail -20               # 必须全绿
go test ./... -v 2>&1 | grep -c -- '--- PASS'
go test ./... -v 2>&1 | grep -c -- '--- FAIL'   # 必须是 0
```

**端到端实跑**（本机能直连 `opencode.ai`，免费档无需 key）：

```bash
cp config.example.yaml config.yaml          # 把 listen 改成 127.0.0.1:18318，api_keys 填 ["sk-test-1234"]
go build -o bin/zen2api . && ./bin/zen2api -config config.yaml &   # 记下 PID，收尾只杀这个 PID
curl -s http://127.0.0.1:18318/healthz
curl -s -H 'Authorization: Bearer sk-test-1234' http://127.0.0.1:18318/v1/models | head -c 400
curl -s -o /dev/null -w '%{http_code}\n' http://127.0.0.1:18318/v1/models        # 期望 401
curl -sN -X POST http://127.0.0.1:18318/v1/chat/completions \
  -H 'Authorization: Bearer sk-test-1234' -H 'Content-Type: application/json' \
  -d '{"model":"oc/big-pickle","stream":true,"messages":[{"role":"user","content":"Reply with exactly: ZEN_OK"}]}' | head -20
curl -s -X POST http://127.0.0.1:18318/v1/chat/completions \
  -H 'Authorization: Bearer sk-test-1234' -H 'Content-Type: application/json' \
  -d '{"model":"big-pickle","stream":false,"messages":[{"role":"user","content":"Reply with exactly: ZEN_OK"}]}'
curl -s -X POST http://127.0.0.1:18318/v1/chat/completions \
  -H 'Authorization: Bearer sk-test-1234' -H 'Content-Type: application/json' \
  -d '{"model":"oc/nope","messages":[{"role":"user","content":"hi"}]}'            # 期望 400 model_not_found
kill <PID>
```

- 收尾用 `kill <记录的 PID>`，**禁止** `pkill -f zen2api`（会误杀别人的进程）。
- 交付前清掉 `config.yaml`（含测试 key，且已被 .gitignore 忽略）与 `bin/`，仓库里只留 SPEC 与源码。
- 总结里逐条列出 §14 的命令与**实际输出**，并说明 §12.2 每个用例覆盖了哪个行为。
