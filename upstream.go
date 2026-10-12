package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/tidwall/gjson"
)

type apiError struct {
	Status  int
	Code    string
	Message string
	Type    string
}

func (e *apiError) Error() string {
	return e.Message
}

// cloneWithBody 在每次重试时构造一个带新 body reader 的请求（避免 *bytes.Reader 被读到 EOF 后无法重发）。
func cloneWithBody(ctx context.Context, base *http.Request, body []byte) *http.Request {
	r := base.Clone(ctx)
	r.Body = io.NopCloser(bytes.NewReader(body))
	r.ContentLength = int64(len(body))
	return r
}

// apiErrorFromUpstream 把上游 4xx/5xx 的响应体归类成 *apiError（§9.3）：
// code 优先取上游 error.code，其次 error.type，取不到用 upstream_error；
// message 取上游 message（截断 ≤512 字节），没有则退化成 body 原文（同样截断）。
func apiErrorFromUpstream(status int, body []byte) *apiError {
	code, msg := parseUpstreamError(body)
	if code == "" {
		code = "upstream_error"
	}
	if msg == "" {
		msg = truncate(string(body), 512)
	} else {
		msg = truncate(msg, 512)
	}
	return &apiError{Status: status, Code: code, Message: msg, Type: code}
}

// warnChatProtocolUnsupported 在上游以 ModelProtocolUnsupported 拒绝 chat 请求时给一条排障提示（§5）：
// 该模型很可能只支持 /responses，需要加入 upstream.responses_models。错误体与状态码照旧透传。
func warnChatProtocolUnsupported(log *logger, upstreamModel string, ae *apiError) {
	if log == nil || ae == nil || ae.Code != "ModelProtocolUnsupported" {
		return
	}
	log.warn(fmt.Sprintf("上游拒绝 chat 协议，模型 %s 可能需要加入 upstream.responses_models", upstreamModel), nil)
}

// buildUpstreamRequest 构造发往上游的请求（§6）：
//  1. 从客户端请求体读原始字节，顶层 model 改写成上游 id；
//  2. 指纹改造（四件套工具 / 强制 stream），失败按原样发送；
//  3. 会话 id 用客户端 X-Request-Id（非空时）否则客户端请求名做种子；
//  4. 显式设置 Content-Type / User-Agent / x-opencode-client / x-opencode-session，
//     绝不透传客户端 Authorization，也不设 Accept-Encoding。
//
// 返回：上游请求、工具改名映射（小写名 → 客户端原拼写）、已改造的 body 副本。
func buildUpstreamRequest(cfg *config, clientReq *http.Request, clientModel, upstreamModel string, forceUpstreamStream bool) (*http.Request, map[string]string, []byte, error) {
	if clientReq == nil || clientReq.Body == nil {
		return nil, nil, nil, fmt.Errorf("客户端请求体为空")
	}
	body, err := readAll(clientReq.Body)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("读取客户端请求体失败: %w", err)
	}
	if len(body) == 0 {
		return nil, nil, nil, fmt.Errorf("客户端请求体为空")
	}

	body = rewriteModel(body, upstreamModel)

	// 指纹改造（复用 applyFingerprint，§6.2）。签名里没有 logger，失败按 SPEC 原样发送。
	var renamed map[string]string
	if cfg.injectTools() {
		result, ferr := applyFingerprint(body, "openai", fingerprintOptions{
			InjectTools:   true,
			ForceStream:   forceUpstreamStream,
			ClientStreams: true,
		})
		if ferr == nil {
			body = result.Body
			if len(result.Renamed) > 0 && cfg.restoreToolNames() {
				// 独立服务里改名映射只在请求作用域内，不再需要插件版全局表。
				renamed = result.Renamed
			}
		}
	}

	// 会话 id：优先客户端 X-Request-Id，否则用客户端请求名做种子（§6.3）。
	seed := clientModel
	if v := clientReq.Header.Get("X-Request-Id"); v != "" {
		seed = v
	}
	sessionID := sessionIDFor(seed, cfg.ttl, time.Now())

	url := strings.TrimRight(cfg.baseURL(), "/") + "/chat/completions"
	httpReq, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, nil, nil, fmt.Errorf("构造上游请求失败: %w", err)
	}
	// 逐条显式设置，绝不透传客户端 Authorization，也不设 Accept-Encoding（§6.5）。
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("User-Agent", cfg.Upstream.UserAgent)
	httpReq.Header.Set("x-opencode-client", cfg.Upstream.ClientName)
	httpReq.Header.Set("x-opencode-session", sessionID)

	return httpReq, renamed, body, nil
}

// executeNonStream 执行一次非流式请求（§7）：
//   - force（默认）：上游仍按 stream:true 发，把 SSE 聚合成 chat.completion；
//   - pass：不强制 stream，把上游状态码与 body 原样返回（上游 4xx/5xx 由 *apiError 带出真实状态码）。
//
// reject 由 server.go 提前拦掉。返回（响应体字节, Content-Type, 错误）。
func executeNonStream(ctx context.Context, cfg *config, log *logger, pool *proxyPool, clientReq *http.Request, body []byte, clientModel, upstreamModel string) ([]byte, string, *apiError) {
	if ctx == nil {
		ctx = context.Background()
	}
	forceAggregate := cfg.Upstream.NonStreamPolicy != nonStreamPass
	// pass 不强制上游 stream，否则拿回来的是 SSE 帧，没法当非流式响应给客户端。
	forceUpstreamStream := cfg.forceStream() && forceAggregate

	buildReq := clientReq.Clone(ctx)
	buildReq.Body = io.NopCloser(bytes.NewReader(body))
	httpReq, renamed, upBody, err := buildUpstreamRequest(cfg, buildReq, clientModel, upstreamModel, forceUpstreamStream)
	if err != nil {
		return nil, "", &apiError{Status: http.StatusInternalServerError, Code: "internal_error", Message: err.Error(), Type: "internal_error"}
	}

	// §6.6：非流式用 request_timeout 整体预算。
	reqCtx, cancel := context.WithTimeout(ctx, cfg.Upstream.RequestTimeout.value(defaultRequestTimeout))
	defer cancel()

	resp, _, ok, lastErrBody, attempts := pool.do(reqCtx, httpReq, upBody, log)
	if !ok {
		return nil, "", &apiError{Status: http.StatusServiceUnavailable, Code: "upstream_unreachable", Message: proxyFailureSummary(attempts), Type: "upstream_unreachable"}
	}
	defer resp.Body.Close()

	status := resp.StatusCode

	if !forceAggregate {
		// pass：原样透传；上游错误仍以真实状态码走错误信封，客户端才看得到 403 FreeTierError。
		if status >= 400 {
			ae := apiErrorFromUpstream(status, lastErrBody)
			warnChatProtocolUnsupported(log, upstreamModel, ae)
			return nil, "application/json", ae
		}
		bodyBytes, _ := readAll(resp.Body)
		return bodyBytes, "application/json", nil
	}

	if status >= 400 {
		ae := apiErrorFromUpstream(status, lastErrBody)
		warnChatProtocolUnsupported(log, upstreamModel, ae)
		return nil, "application/json", ae
	}

	// force 聚合：把上游 SSE 聚成一个 chat.completion。
	bodyBytes, _ := readAll(resp.Body)
	aggregated := aggregateSSE(bodyBytes, renamed, clientModel)
	return aggregated, "application/json", nil
}

// executeStreamUpstream 只负责「拿到上游响应体」（§8）：
// 返回（上游 body, 工具改名映射, 错误）。整体 ctx 由 server.go 用 request_timeout 创建，
// 流式中继结束后才 cancel；上游非 2xx 时返回 *apiError（此时 body 已关）。
// 第二个返回值是 buildUpstreamRequest 得到的「小写名 → 客户端原拼写」映射（可能为 nil），
// 供 relaySSE 还原工具名用；上游响应头不返回（本服务不复制上游头，§9.4）。
func executeStreamUpstream(ctx context.Context, cfg *config, log *logger, pool *proxyPool, clientReq *http.Request, body []byte, clientModel, upstreamModel string) (io.ReadCloser, map[string]string, *apiError) {
	if ctx == nil {
		ctx = context.Background()
	}
	buildReq := clientReq.Clone(ctx)
	buildReq.Body = io.NopCloser(bytes.NewReader(body))
	httpReq, renamed, upBody, err := buildUpstreamRequest(cfg, buildReq, clientModel, upstreamModel, cfg.forceStream())
	if err != nil {
		return nil, nil, &apiError{Status: http.StatusInternalServerError, Code: "internal_error", Message: err.Error(), Type: "internal_error"}
	}

	resp, _, ok, lastErrBody, attempts := pool.do(ctx, httpReq, upBody, log)
	if !ok {
		return nil, nil, &apiError{Status: http.StatusServiceUnavailable, Code: "upstream_unreachable", Message: proxyFailureSummary(attempts), Type: "upstream_unreachable"}
	}

	status := resp.StatusCode
	if status >= 400 {
		ae := apiErrorFromUpstream(status, lastErrBody)
		warnChatProtocolUnsupported(log, upstreamModel, ae)
		_ = resp.Body.Close()
		return nil, nil, ae
	}

	return resp.Body, renamed, nil
}

// relaySSE 按行读 in，只取 data: 行（注释 / 心跳 / event / 空行全部丢弃），
// 还原工具名 + 把 model 改写成客户端请求名，逐帧写出并 flush（§8）。
// 写出格式恒为 "data: <payload>\n\n"；[DONE] 照常转发；绝不允许 data: data:。
// in 读到 EOF 或客户端断开（ctx.Done()）视为正常结束；读错误返回 error。
func relaySSE(ctx context.Context, in io.Reader, out io.Writer, flush func(), renamed map[string]string, clientModel string, restoreToolNames bool, log *logger) error {
	reader := bufioReader(in)
	for {
		if ctx != nil {
			select {
			case <-ctx.Done():
				return nil
			default:
			}
		}

		line, err := reader.ReadBytes('\n')
		if len(line) > 0 {
			if werr := relaySSELine(line, out, flush, renamed, clientModel, restoreToolNames); werr != nil {
				if log != nil {
					log.warn("SSE 中继写出失败: "+werr.Error(), nil)
				}
				return werr
			}
		}
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
	}
}

// relaySSELine 处理一行上游 SSE：非 data: 行丢弃；data: 行去掉前缀与首尾空白后，
// 做工具名还原与 model 改写，再以 "data: <payload>\n\n" 写出。
func relaySSELine(raw []byte, out io.Writer, flush func(), renamed map[string]string, clientModel string, restoreToolNames bool) error {
	line := bytes.TrimRight(raw, "\r\n")
	trimmed := bytes.TrimLeft(line, " \t")
	if !bytes.HasPrefix(trimmed, []byte("data:")) {
		return nil // 注释 / 心跳 / event / 空行全部丢弃。
	}
	payload := bytes.TrimSpace(trimmed[len("data:"):])
	if len(payload) == 0 {
		return nil
	}

	if !bytes.Equal(payload, []byte("[DONE]")) {
		if !gjson.ValidBytes(payload) {
			return nil
		}
		// ① 还原工具名；② 改写 model 为客户端请求名。
		if restoreToolNames && len(renamed) > 0 {
			if restored, changed := restoreJSON(payload, renamed); changed {
				payload = restored
			}
		}
		payload = rewritePayloadModel(payload, clientModel)
	}

	if _, err := io.WriteString(out, "data: "); err != nil {
		return err
	}
	if _, err := out.Write(payload); err != nil {
		return err
	}
	if _, err := io.WriteString(out, "\n\n"); err != nil {
		return err
	}
	if flush != nil {
		flush()
	}
	return nil
}
