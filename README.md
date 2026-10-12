# zen2api

把 **OpenCode Zen 免费档**反推成一个独立运行的 **OpenAI 兼容** HTTP 服务。

## 简介

OpenCode Zen 的免费档模型无需 API key 即可调用，但上游只认 opencode 客户端发出的请求形状（特定的 `User-Agent`、`x-opencode-client` / `x-opencode-session` 会话头、`bash`/`glob`/`grep`/`read` 四件套工具、以及强制流式）。zen2api 把这些要求封装在一个本地服务里，对外提供标准的 OpenAI 接口，任何支持自定义 Base URL 的 OpenAI 客户端都能直接接入。

## 特性

- **OpenAI 兼容**：`GET /v1/models`、`POST /v1/chat/completions`，外加公开的 `GET /healthz`。
- **自动伪装上游指纹**：`User-Agent`、`x-opencode-client`、`x-opencode-session`、四件套工具、强制 `stream:true` 全部自动处理，客户端无需关心。
- **多种模型名写法**：`oc/<上游 id>`、配置的别名、裸 `<上游 id>` 都能解析到同一个上游模型。
- **工具名还原**：客户端声明的 `Bash` 会被改写成上游要求的小写 `bash`，响应里再还原成 `Bash`。
- **流式与非流式都支持**：非流式可选强制聚合、拒绝或原样透传。
- **出口代理池**：支持 `http`/`https`/`socks5`/`socks5h`/`direct` 多出口，按策略轮转，失败后指数退避冷却并换下一个。
- **模型目录缓存**：从上游拉取模型目录，按规则筛出免费档模型并落盘缓存。

## 快速开始

```bash
# 构建
go build -buildvcs=false -o bin/zen2api .

# 复制并按需修改配置
cp config.example.yaml config.yaml

# 启动（默认读取 ./config.yaml）
./bin/zen2api

# 指定配置文件
./bin/zen2api -config /path/to/config.yaml

# 打印版本
./bin/zen2api -version
```

默认监听 `127.0.0.1:8318`。`server.api_keys` 为空时 `/v1` 不鉴权（仅建议本机监听时使用）。

## 部署（ai，原生 systemd）

生产实例跑在 `ai`（10.1.0.8）的 LXC 层，不用 docker：

| 项 | 值 |
|---|---|
| 目录 | `/zfspool/data/ai/zen2api/`（**项目根 = 源码仓库根**：源码与 `config.yaml`、二进制 `zen2api`、缓存 `data/` 同层） |
| 二进制 | `/zfspool/data/ai/zen2api/zen2api` |
| 服务账号 | `app`（uid 10001，与容器内一致 → `data/` 属主不用动） |
| 单元 | `zen2api.service`（备份在 `/zfspool/share/nas/config/systemd/ai/`） |
| 监听 | `0.0.0.0:3068`（`server.listen`；对外 `http://10.1.0.8:3068/v1`，不鉴权） |
| 日志 | `journalctl -u zen2api -f` |

```bash
# 编译（在 ai 上，它自带 Go；GOTOOLCHAIN=auto 会自动拉 go1.26）
cd /zfspool/data/ai/zen2api
CGO_ENABLED=0 go build -buildvcs=false -trimpath -ldflags "-s -w" -o zen2api .
systemctl restart zen2api
curl -s http://10.1.0.8:3068/healthz
```

> 二进制是静态的，目标机不需要 Go；`data/catalog.json` 属 uid 10001（`chown -R 10001:10001 data`）。

## 客户端接法

- **Base URL**：`http://<host>:8318/v1`
- **鉴权**：`Authorization: Bearer <api_keys 里的 key>`（也接受 `x-api-key`，或省略 `Bearer ` 前缀的裸 key）
- **模型名**：`oc/<上游 id>`（例如 `oc/mimo-v2.6-flash-free`），也可以直接写裸 `<上游 id>`；配置了别名时还能用别名
- 先调 `GET /v1/models` 查看当前可用的模型名

```bash
# 健康检查（公开，不鉴权）
curl http://127.0.0.1:8318/healthz

# 列出可用模型
curl -H 'Authorization: Bearer sk-test-1234' \
  http://127.0.0.1:8318/v1/models

# 流式对话
curl -N -X POST http://127.0.0.1:8318/v1/chat/completions \
  -H 'Authorization: Bearer sk-test-1234' \
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
| `upstream.models` | []string | `[]` | 额外暴露的上游 id（即使不命中 `match_regex` 也会暴露） |
| `upstream.match_regex` | string | `(-free$)|(^big-pickle$)|(^space-bunny)` | 从上游目录里筛选要暴露的模型；显式置空表示关闭正则兜底 |
| `upstream.model_aliases` | map[string][]string | `{}` | 上游 id → 额外客户端名（别名不带前缀） |
| `upstream.catalog_file` | string | `"./data/catalog.json"` | 模型目录缓存路径 |
| `upstream.catalog_refresh` | duration | `6h` | 目录缓存过期后，`/v1/models` 会同步重拉 |
| `upstream.catalog_timeout` | duration | `10s` | 拉取目录超时 |
| `upstream.catalog_via_proxy` | bool | `false` | 拉取目录是否走代理池 |
| `upstream.inject_tools` | bool | `true` | 注入/归一化 `bash`/`glob`/`grep`/`read` 四件套工具 |
| `upstream.force_stream` | bool | `true` | 强制上游使用流式 |
| `upstream.nonstream_policy` | string | `force` | 非流式策略：`force`/`reject`/`pass` |
| `upstream.restore_tool_names` | bool | `true` | 响应里把工具名还原成客户端原本的拼写 |
| `upstream.user_agent` | string | `"opencode/1.18.31"` | 上游 `User-Agent`，必须包含 `opencode/` |
| `upstream.client_name` | string | `"desktop"` | 上游 `x-opencode-client` 的值 |
| `upstream.session_ttl` | duration | `30m` | 会话 id 的有效期 |
| `upstream.request_timeout` | duration | `300s` | 单次请求的整体时间预算 |
| `upstream.first_byte_timeout` | duration | `60s` | 等待上游响应头的超时 |

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
| `big-pickle`、`space-bunny*`、`*-free`（mimo 等） | `/chat/completions` |
| `muse-spark-1.2-contributor-free`、`muse-spark-1.3-contributor-free` | 只走 `/responses` |

zen2api 对 `responses_models` 里的模型自动走 `/responses` 上游通道，并在内部做
chat.completions ↔ responses 的协议翻译（含流式与非流式），客户端仍按普通 chat 请求调用。

| 键 | 类型 | 默认值 | 说明 |
|---|---|---|---|
| `upstream.responses_models` | []string | `[]` | 上游只支持 `/responses` 的模型 id；命中即走协议翻译通道 |
| `upstream.exclude_models` | []string | `[]` | 从暴露集合中剔除的上游 id（例如永远不可用的模型） |

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
- 模型目录会变，本地缓存可能短暂滞后。
- 只提供 OpenAI 面（`/v1/models`、`/v1/chat/completions` 与 `/healthz`），不做 Anthropic `/v1/messages`，也不做 Responses 面。
- 不复制上游响应头，响应头由本服务自行设置。

> 本项目复用 cpa-zen 的指纹/会话/工具名还原/代理池实现。

> 本项目使用 [2api-skill](https://github.com/69gg/2api-skill) 辅助制作。
