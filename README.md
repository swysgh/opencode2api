# opencode2api

把 **OpenCode Zen 免费档**反推成一个独立运行的 **OpenAI 兼容** HTTP 服务。

## 简介

OpenCode Zen 的免费档模型无需 API key 即可调用，但上游只认 opencode 客户端发出的请求形状（特定的 `User-Agent`、`x-opencode-client` / `x-opencode-session` 会话头、`bash`/`glob`/`grep`/`read` 四件套工具、以及强制流式）。opencode2api 把这些要求封装在一个本地服务里，对外提供标准的 OpenAI 接口，任何支持自定义 Base URL 的 OpenAI 客户端都能直接接入。

## 特性

- **OpenAI 兼容**：`GET /v1/models`、`POST /v1/chat/completions`，外加公开的 `GET /healthz`。
- **自动伪装上游指纹**：`User-Agent`、`x-opencode-client`、`x-opencode-session`（前缀编码时间戳，过上游新鲜度校验）、四件套工具、强制 `stream:true` 全部自动处理，客户端无需关心。
- **元数据驱动的模型发现**：叠加官方模型目录 `models.opencode.ai/api.json`，按 `cost`（0/0）与 `status` 判定免费档、按 `provider.npm` SDK 判定上游协议，正则只做兜底——不再靠手工维护模型名单。
- **按模型分协议自动路由**：元数据声明 `@ai-sdk/openai` 的模型自动走 `/responses` 上游并做协议翻译，客户端无感。
- **多种模型名写法**：`oc/<上游 id>`、配置的别名、裸 `<上游 id>` 都能解析到同一个上游模型。
- **工具名还原**：客户端声明的 `Bash` 会被改写成上游要求的小写 `bash`，响应里再还原成 `Bash`。
- **流式与非流式都支持**：非流式可选强制聚合、拒绝或原样透传。
- **出口代理池**：支持 `http`/`https`/`socks5`/`socks5h`/`direct` 多出口，按策略轮转，失败后指数退避冷却并换下一个。
- **目录与元数据双缓存**：模型目录（上游 `/models`）与元数据（官方目录）都落盘缓存，重启零请求，拉取失败沿用缓存。

## 快速开始

```bash
# 构建
go build -buildvcs=false -o bin/opencode2api .

# 复制并按需修改配置
cp config.example.yaml config.yaml

# 启动（默认读取 ./config.yaml）
./bin/opencode2api

# 指定配置文件
./bin/opencode2api -config /path/to/config.yaml

# 打印版本
./bin/opencode2api -version
```

默认监听 `127.0.0.1:8318`。`server.api_keys` 为空时 `/v1` 不鉴权（仅建议本机监听时使用）。

## 部署（ai，原生 systemd）

生产实例跑在 `ai`（10.1.0.8）的 LXC 层，不用 docker：

| 项 | 值 |
|---|---|
| 目录 | `/zfspool/data/ai/opencode2api/`（**项目根 = 源码仓库根**：源码与 `config.yaml`、二进制 `opencode2api`、缓存 `data/` 同层） |
| 二进制 | `/zfspool/data/ai/opencode2api/opencode2api` |
| 服务账号 | `app`（uid 10001，与容器内一致 → `data/` 属主不用动） |
| 单元 | `opencode2api.service`（备份在 `/zfspool/share/nas/config/systemd/ai/`） |
| 监听 | `0.0.0.0:3068`（`server.listen`；对外 `http://10.1.0.8:3068/v1`，不鉴权） |
| 日志 | `journalctl -u opencode2api -f` |

```bash
# 编译（在 ai 上，它自带 Go；GOTOOLCHAIN=auto 会自动拉 go1.26）
cd /zfspool/data/ai/opencode2api
CGO_ENABLED=0 go build -buildvcs=false -trimpath -ldflags "-s -w" -o opencode2api .
systemctl restart opencode2api
curl -s http://10.1.0.8:3068/healthz
```

> 二进制是静态的，目标机不需要 Go；`data/` 属 uid 10001（`chown -R 10001:10001 data`）。

## 客户端接法

- **Base URL**：`http://<host>:8318/v1`
- **鉴权**：`Authorization: Bearer <key>`（也接受 `x-api-key`，或省略 `Bearer ` 前缀的裸 key）
- **模型名**：`oc/<上游 id>`（例如 `oc/mimo-v2.6-flash-free`），也可以直接写裸 `<上游 id>`；配置了别名时还能用别名
- 先调 `GET /v1/models` 查看当前可用的模型名

```bash
# 健康检查（公开，不鉴权）
curl http://127.0.0.1:8318/healthz

# 列出可用模型
curl -H 'Authorization: Bearer <key>' \
  http://127.0.0.1:8318/v1/models

# 流式对话
curl -N -X POST http://127.0.0.1:8318/v1/chat/completions \
  -H 'Authorization: Bearer <key>' \
  -H 'Content-Type: application/json' \
  -d '{"model":"oc/big-pickle","stream":true,"messages":[{"role":"user","content":"你好"}]}'
```

## 配置项

配置文件为 YAML，默认路径 `./config.yaml`，用 `-config <path>` 覆盖。出现未知键（拼错）会直接报错退出。

### server

| 键 | 类型 | 默认值 | 说明 |
|---|---|---|---|
| `server.listen` | string | `"127.0.0.1:8318"` | 监听地址，格式 `host:port` |
| `server.api_keys` | []string | `[]` | 允许的 API key；为空表示 `/v1` 不鉴权 |
| `server.max_body_bytes` | int64 | `67108864` | 请求体上限（字节，64MiB），超出返回 413 |
| `server.shutdown_timeout` | duration | `10s` | 优雅关闭超时 |
| `server.log_level` | string | `info` | 日志级别：`debug`/`info`/`warn`/`error` |
| `server.log_file` | string | `""` | 日志文件路径；空表示输出到 stdout |

### upstream

| 键 | 类型 | 默认值 | 说明 |
|---|---|---|---|
| `upstream.base_url` | string | `"https://opencode.ai/zen/v1"` | 上游基址，必须以 `http://` 或 `https://` 开头 |
| `upstream.model_prefix` | string | `"oc/"` | 客户端调用名前缀 |
| `upstream.models` | []string | `[]` | 额外暴露的上游 id（不受元数据与 `match_regex` 约束，但仍受 `exclude_models` 约束） |
| `upstream.match_regex` | string | `(-free$)|(^big-pickle$)|(^space-bunny)` | 兜底正则：只对「元数据未收录的目录 id」生效（见下） |
| `upstream.model_aliases` | map[string][]string | `{}` | 上游 id → 额外客户端名（别名不带前缀） |
| `upstream.catalog_file` | string | `"./data/catalog.json"` | 模型目录缓存路径 |
| `upstream.catalog_refresh` | duration | `6h` | 目录缓存过期后，`/v1/models` 会同步重拉 |
| `upstream.catalog_timeout` | duration | `10s` | 拉取目录超时 |
| `upstream.catalog_via_proxy` | bool | `false` | 拉取目录/元数据是否走代理池 |
| `upstream.metadata_url` | string | `"https://models.opencode.ai/api.json"` | 官方模型目录地址；显式置空关闭元数据发现 |
| `upstream.metadata_file` | string | `"./data/metadata.json"` | 元数据缓存路径 |
| `upstream.metadata_refresh` | duration | `24h` | 元数据缓存过期后，`/v1/models` 会同步重拉 |
| `upstream.metadata_timeout` | duration | `15s` | 拉取元数据超时 |
| `upstream.inject_tools` | bool | `true` | 注入/归一化 `bash`/`glob`/`grep`/`read` 四件套工具 |
| `upstream.force_stream` | bool | `true` | 强制上游使用流式 |
| `upstream.nonstream_policy` | string | `force` | 非流式策略：`force`/`reject`/`pass` |
| `upstream.restore_tool_names` | bool | `true` | 响应里把工具名还原成客户端原本的拼写 |
| `upstream.user_agent` | string | `"opencode/1.18.31"` | 上游 `User-Agent`，必须包含 `opencode/` |
| `upstream.client_name` | string | `"desktop"` | 上游 `x-opencode-client` 的值 |
| `upstream.session_ttl` | duration | `30m` | 会话 id 的有效期 |
| `upstream.request_timeout` | duration | `300s` | 单次请求的整体时间预算 |
| `upstream.first_byte_timeout` | duration | `60s` | 等待上游响应头的超时 |

### 元数据驱动的暴露集合

上游 `/zen/v1/models` 目录只有模型 id（无协议/价格/弃用状态）。服务把它与官方模型目录
`models.opencode.ai/api.json`（`opencode` 段）叠加，按下面的顺序算出暴露集合：

1. **显式 `models`**：无条件暴露（人工兜底通道，仅受 `exclude_models` 约束）；
2. **元数据可用时**：目录里的 id 命中「免费档（`cost` input/output 均为 0 且 `status != deprecated`）
   且协议不是 `anthropic`（本服务没有 `/messages` 通道）」就暴露；元数据已收录但不符合的
   id 一律不暴露——**正则不会再把它捞回来**（防止已下线模型继续出现在列表里）；
3. **元数据未收录的目录 id**（新上架模型）：`match_regex` 兜底；
4. **元数据不可用**（拉取失败且无缓存、或显式置空 `metadata_url`）：整体回退老的纯正则行为。

`/v1/models` 请求时会顺带检查目录与元数据缓存是否过期（过期则同步重拉），所以模型上下线
最迟在下一次列模型时反映出来；`/healthz` 的 `metadata` 字段是已加载的元数据模型数。

### proxy

| 键 | 类型 | 默认值 | 说明 |
|---|---|---|---|
| `proxy.proxies` | []string | `[]` | 出口列表，支持 `http://`/`https://`/`socks5://`/`socks5h://`/`direct` |
| `proxy.proxy_file` | string | `""` | 从文件读取出口，每行一条，`#` 开头为注释 |
| `proxy.strategy` | string | `round_robin` | 选择策略：`round_robin`/`random`/`least_inflight` |
| `proxy.max_attempts` | int | `3` | 单请求最多尝试几个出口；`<1` 视为 `1` |
| `proxy.cooldown.base` | duration | `30s` | 冷却基数 |
| `proxy.cooldown.factor` | float | `2.0` | 退避倍数（须 `>= 1`） |
| `proxy.cooldown.max` | duration | `10m` | 冷却上限（须 `>= base`） |
| `proxy.cooldown.jitter` | float | `0.2` | 随机抖动比例，取值 `[0,1]` |
| `proxy.cooldown.statuses` | []int | `[403,407,429,500,502,503,504]` | 命中这些状态码即冷却该出口并换下一个 |

> 说明：`duration` 接受 `6h`、`30m`、`10s` 这类字符串，也接受以秒为单位的数字。

## 上游按模型分协议（`/responses` 翻译）

实测：opencode zen 免费档按模型分协议，客户端无感。

| 上游模型 | 上游端点 |
|---|---|
| `big-pickle`、`space-bunny*`、大部分 `*-free` | `/chat/completions` |
| `muse-spark-*-contributor-free` | 只走 `/responses` |
| 少数模型（如 minimax/qwen 系） | `/messages`（Anthropic 协议，本服务不暴露） |

协议判定优先读元数据的 `provider.npm` SDK 声明（`@ai-sdk/openai-compatible`→chat、
`@ai-sdk/openai`→responses、`@ai-sdk/anthropic`→messages），自动路由并做
chat.completions ↔ responses 的协议翻译（含流式与非流式），客户端仍按普通 chat 请求调用。

| 键 | 类型 | 默认值 | 说明 |
|---|---|---|---|
| `upstream.responses_models` | []string | `[]` | 显式追加走 `/responses` 的模型 id（元数据不可用时的兜底；元数据已覆盖的模型无需再写） |
| `upstream.exclude_models` | []string | `[]` | 从暴露集合中剔除的上游 id（对显式 `models` 同样生效） |

注意事项：

- `upstream.nonstream_policy` 对 responses 模型**不适用**：上游免费档不接受 `stream:false`，
  所以无论 `force`/`pass`，responses 路径都恒发 `stream:true` 并把 SSE 聚合成 `chat.completion`
  （`pass` 时只在日志里记一条 warn）；`reject` 仍按原样返回 400 `nonstream_rejected`。
- `/responses` 的 `tool_choice` **只接受 `"auto"`**：其它取值（`none`/`required`/具名函数）会被
  翻译层丢弃，不会透传给上游。

## 出口代理池用法

- **`proxies`**：直接内联出口列表。为空时隐式使用 `direct`（直连）。
- **`proxy_file`**：从文件读取出口，每行一条，`#` 开头为注释、空行忽略；与 `proxies` 合并（内联的排在前面）。
- **`strategy`**：
  - `round_robin`（默认）：依次轮转；
  - `random`：随机选一个；
  - `least_inflight`：选当前进行中请求数最少的。
- **`max_attempts`**：单次请求最多尝试几个出口，失败就换下一个（池里只有一个出口时也允许重试同一个，避免瞬时抖动直接失败）。
- **`cooldown`**：出口失败后进入冷却。失败包括传输层错误，以及状态码命中 `statuses`。第 n 次冷却时长约为 `base * factor^(n-1)`（不超过 `max`），再叠加 `jitter` 比例的随机抖动；冷却中的出口会被跳过，全部冷却时退回到剩余冷却最短的那个。

```yaml
proxy:
  proxies:
    - http://user:pass@10.0.0.1:8080
    - socks5://10.0.0.2:1080
    - direct
  strategy: round_robin
  max_attempts: 3
  cooldown:
    base: 30s
    factor: 2.0
    max: 10m
    jitter: 0.2
    statuses: [403, 407, 429, 500, 502, 503, 504]
```

## 错误码

错误响应统一为 OpenAI 信封：`{"error":{"message":"...","type":"...","code":"...","param":null}}`。

| 场景 | HTTP 状态码 | `code` |
|---|---|---|
| 未找到模型 | 400 | `model_not_found` |
| body 不是合法 JSON / 缺少字符串 `model` | 400 | `invalid_request` |
| body 超过 `max_body_bytes` | 413 | `request_too_large` |
| 未鉴权 | 401 | `invalid_api_key` |
| 方法不对 | 405 | `method_not_allowed` |
| 非流式请求被 `reject` 策略拒绝 | 400 | `nonstream_rejected` |
| 上游 4xx/5xx | 上游状态码 | 上游的 `error.type` / `error.code`，取不到则为 `upstream_error` |
| 全部出口失败（传输层） | 503 | `upstream_unreachable` |
| 内部异常 | 500 | `internal_error` |

## 局限

- 免费档随时可能变动：可用模型、额度限制、上游要求的指纹都可能变化。
- 模型目录与元数据都会变，本地缓存可能短暂滞后（拉取失败时沿用缓存，不致命）。
- 只提供 OpenAI 面（`/v1/models`、`/v1/chat/completions` 与 `/healthz`），不做 Anthropic `/v1/messages`，也不做 Responses 面。
- Anthropic 协议的免费模型（元数据 `@ai-sdk/anthropic`）不会暴露——上游只给 `/messages` 端点。
- 不复制上游响应头，响应头由本服务自行设置。

> 本项目复用 cpa-zen 的指纹/会话/工具名还原/代理池实现。

> 本项目使用 [2api-skill](https://github.com/69gg/2api-skill) 辅助制作。
