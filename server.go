package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"
)

// maxBodyBytesFallback 是请求体上限的兜底值（配置层已保证 > 0，这里只防手搓 cfg）。
const maxBodyBytesFallback = int64(67108864) // 64MiB

// server 汇总 HTTP 层依赖（§9）。
type server struct {
	cfg       *config
	log       *logger
	pool      *proxyPool
	rt        *router
	cat       *catalog
	startedAt time.Time
}

// routes 构造对外 HTTP 路由（§9.1）：
//   - GET  /healthz               公开，最先短路，不鉴权；
//   - GET  /v1/models             鉴权后返回 OpenAI 列表形状；
//   - POST /v1/chat/completions   鉴权后代理上游；
//   - 其它 /v1/*                  鉴权后 404 错误信封；
//   - 方法不符                    405 错误信封。
func routes(cfg *config, log *logger, pool *proxyPool, rt *router, cat *catalog, startedAt time.Time) http.Handler {
	s := &server{cfg: cfg, log: log, pool: pool, rt: rt, cat: cat, startedAt: startedAt}
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.handleHealthz)
	mux.HandleFunc("/v1/models", s.handleModels)
	mux.HandleFunc("/v1/chat/completions", s.handleChatCompletions)
	mux.HandleFunc("/v1/", s.handleV1Fallback)
	mux.HandleFunc("/v1", s.handleV1Fallback)
	return mux
}

// recoverTo500 把 handler 里的 panic 变成 500 错误信封，绝不让进程崩（§9.3）。
func (s *server) recoverTo500(w http.ResponseWriter) {
	if rec := recover(); rec != nil {
		if s.log != nil {
			s.log.error(fmt.Sprintf("请求处理 panic: %v", rec), nil)
		}
		writeError(w, http.StatusInternalServerError, "internal_error", "服务内部错误", "internal_error")
	}
}

// handleHealthz 是公开健康检查（§9.1）：不鉴权、不落访问日志。
func (s *server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	defer s.recoverTo500(w)
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "方法不允许: "+r.Method, "invalid_request_error")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	b, _ := json.Marshal(map[string]any{
		"status":  "ok",
		"version": version,
		"models":  len(s.rt.exposedList()),
		"proxies": s.poolSize(),
	})
	_, _ = w.Write(b)
}

// handleModels 返回 OpenAI 列表形状（§9.1）：id 是客户端调用名（前缀+id 与全部别名），
// 排序去重；object=model；created=进程启动 unix 秒；owned_by=opencode。
func (s *server) handleModels(w http.ResponseWriter, r *http.Request) {
	defer s.recoverTo500(w)
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "方法不允许: "+r.Method, "invalid_request_error")
		return
	}
	if !s.authorized(r) {
		writeError(w, http.StatusUnauthorized, "invalid_api_key", "缺少或无效的 API key", "invalid_request_error")
		return
	}

	// §5.3：/v1/models 请求时若缓存超过 catalog_refresh 则同步重拉，失败沿用缓存。
	s.maybeRefreshCatalog(r.Context())

	type modelEntry struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		Created int64  `json:"created"`
		OwnedBy string `json:"owned_by"`
	}
	names := s.clientModelNames()
	data := make([]modelEntry, 0, len(names))
	for _, n := range names {
		data = append(data, modelEntry{ID: n, Object: "model", Created: s.startedAt.Unix(), OwnedBy: "opencode"})
	}
	w.Header().Set("Content-Type", "application/json")
	b, _ := json.Marshal(map[string]any{"object": "list", "data": data})
	_, _ = w.Write(b)
}

// maybeRefreshCatalog 在目录缓存过期时同步重拉（§5.3）。catalog_via_proxy=true 时用代理池
// 当出口，否则直连；失败只 warn，沿用缓存。
func (s *server) maybeRefreshCatalog(ctx context.Context) {
	if s.cat == nil {
		return
	}
	refresh := s.cfg.Upstream.CatalogRefresh.value(defaultCatalogRefresh)
	if s.cat.age() <= refresh {
		return
	}
	var doer httpDoer
	if s.cfg.catalogViaProxy() {
		doer = s.pool
	}
	if err := s.cat.fetch(ctx, s.cfg.baseURL(), s.cfg.Upstream.CatalogTimeout.value(defaultCatalogTimeout), doer); err != nil {
		if s.log != nil {
			s.log.warn("目录刷新失败，沿用缓存: "+err.Error(), nil)
		}
		return
	}
	s.rt.setExposed(exposedFromCatalog(s.cfg, s.cat))
}

// clientModelNames 汇总暴露集合里每个上游 id 的客户端调用名（前缀名 + 全部别名），排序去重。
func (s *server) clientModelNames() []string {
	prefix := s.rt.modelPrefix
	set := map[string]struct{}{}
	for _, id := range s.rt.exposedList() {
		set[prefix+id] = struct{}{}
		for _, a := range s.cfg.Upstream.ModelAliases[id] {
			a = strings.TrimSpace(a)
			if a == "" {
				continue
			}
			// 别名不带前缀；若带了也剥掉，避免双重前缀（与 router.newRouter 一致）。
			if strings.HasPrefix(a, prefix) {
				a = strings.TrimPrefix(a, prefix)
			}
			set[a] = struct{}{}
		}
	}
	out := make([]string, 0, len(set))
	for n := range set {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// exposedFromCatalog 计算暴露集合（§5.1）：目录里命中 matchesUpstream 的 id ∪ 显式 models。
func exposedFromCatalog(cfg *config, cat *catalog) []string {
	set := map[string]struct{}{}
	for _, id := range cfg.Upstream.Models {
		if id != "" {
			set[id] = struct{}{}
		}
	}
	if cat != nil {
		for _, id := range cat.list() {
			if cfg.matchesUpstream(id) {
				set[id] = struct{}{}
			}
		}
	}
	out := make([]string, 0, len(set))
	for id := range set {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// handleV1Fallback 处理其它 /v1/*：鉴权后 404 错误信封（§9.1）。
func (s *server) handleV1Fallback(w http.ResponseWriter, r *http.Request) {
	defer s.recoverTo500(w)
	if !s.authorized(r) {
		writeError(w, http.StatusUnauthorized, "invalid_api_key", "缺少或无效的 API key", "invalid_request_error")
		return
	}
	writeError(w, http.StatusNotFound, "not_found", "未知路径: "+r.URL.Path, "invalid_request_error")
}

// handleChatCompletions 代理一次 chat.completions（§6/§7/§8），每请求一条 info 日志（§9.4）。
func (s *server) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	tr := &tracker{ResponseWriter: w}
	start := time.Now()
	var clientModel, upstreamModel string
	var stream bool

	defer func() {
		if rec := recover(); rec != nil {
			if s.log != nil {
				s.log.error(fmt.Sprintf("请求处理 panic: %v", rec), nil)
			}
			writeError(tr, http.StatusInternalServerError, "internal_error", "服务内部错误", "internal_error")
		}
		s.logRequest(clientModel, upstreamModel, stream, tr, start)
	}()

	if r.Method != http.MethodPost {
		writeError(tr, http.StatusMethodNotAllowed, "method_not_allowed", "方法不允许: "+r.Method, "invalid_request_error")
		return
	}
	if !s.authorized(r) {
		writeError(tr, http.StatusUnauthorized, "invalid_api_key", "缺少或无效的 API key", "invalid_request_error")
		return
	}

	// §9.4：按 max_body_bytes 限制请求体，超限 → 413。
	maxBody := maxBodyBytesFallback
	if s.cfg.Server.MaxBodyBytes != nil {
		maxBody = *s.cfg.Server.MaxBodyBytes
	}
	r.Body = http.MaxBytesReader(tr, r.Body, maxBody)
	data, err := io.ReadAll(r.Body)
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			writeError(tr, http.StatusRequestEntityTooLarge, "request_too_large",
				fmt.Sprintf("请求体超过上限 %d 字节", maxBody), "invalid_request_error")
		} else {
			writeError(tr, http.StatusBadRequest, "invalid_request", "读取请求体失败: "+err.Error(), "invalid_request_error")
		}
		return
	}

	// §9.3：JSON 解不开 / 没有字符串 model → 400 invalid_request。
	var top map[string]json.RawMessage
	if err := json.Unmarshal(data, &top); err != nil {
		writeError(tr, http.StatusBadRequest, "invalid_request", "请求体不是合法 JSON: "+err.Error(), "invalid_request_error")
		return
	}
	rawModel, ok := top["model"]
	if !ok {
		writeError(tr, http.StatusBadRequest, "invalid_request", "请求体缺少字符串字段 model", "invalid_request_error")
		return
	}
	if err := json.Unmarshal(rawModel, &clientModel); err != nil {
		writeError(tr, http.StatusBadRequest, "invalid_request", "请求体字段 model 必须是字符串", "invalid_request_error")
		return
	}
	if rawStream, ok := top["stream"]; ok {
		_ = json.Unmarshal(rawStream, &stream)
	}

	// §9.3：解析不到上游 id → 400 model_not_found（message 含请求名与可用模型数量）。
	upstreamModel = s.rt.resolveUpstreamModel(clientModel)
	if upstreamModel == "" {
		writeError(tr, http.StatusBadRequest, "model_not_found",
			fmt.Sprintf("未找到模型 %q，当前可用模型 %d 个", clientModel, len(s.rt.exposedList())),
			"invalid_request_error")
		return
	}

	if stream {
		s.streamChat(tr, r, data, clientModel, upstreamModel)
		return
	}
	s.nonStreamChat(tr, r, data, clientModel, upstreamModel)
}

// streamChat 处理 stream:true（§8）：设 SSE 响应头，拿上游 body 后逐帧中继。
func (s *server) streamChat(w http.ResponseWriter, r *http.Request, data []byte, clientModel, upstreamModel string) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	ctx, cancel := context.WithTimeout(r.Context(), s.cfg.Upstream.RequestTimeout.value(defaultRequestTimeout))
	defer cancel()

	in, renamed, ae := executeStreamUpstream(ctx, s.cfg, s.log, s.pool, r, data, clientModel, upstreamModel)
	if ae != nil {
		writeError(w, ae.Status, ae.Code, ae.Message, ae.Type)
		return
	}
	defer in.Close()

	flush := func() {
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}
	if err := relaySSE(ctx, in, w, flush, renamed, clientModel, s.cfg.restoreToolNames(), s.log); err != nil && s.log != nil {
		s.log.warn("SSE 中继中断: "+err.Error(), nil)
	}
}

// nonStreamChat 处理 stream:false（§7）：reject 直接 400；force/pass 交给 executeNonStream。
func (s *server) nonStreamChat(w http.ResponseWriter, r *http.Request, data []byte, clientModel, upstreamModel string) {
	if s.cfg.Upstream.NonStreamPolicy == nonStreamReject {
		writeError(w, http.StatusBadRequest, "nonstream_rejected",
			"OpenCode Zen 免费档只接受流式请求（stream:true）。请让客户端开启流式，或把配置 upstream.nonstream_policy 改为 force。",
			"invalid_request_error")
		return
	}
	out, ct, ae := executeNonStream(r.Context(), s.cfg, s.log, s.pool, r, data, clientModel, upstreamModel)
	if ae != nil {
		writeError(w, ae.Status, ae.Code, ae.Message, ae.Type)
		return
	}
	if ct == "" {
		ct = "application/json"
	}
	w.Header().Set("Content-Type", ct)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(out)
}

// authorized 校验 /v1/* 的 API key（§9.2）：api_keys 为空直接放行；
// 非空时接受 Authorization: Bearer <key>、裸 key 与 x-api-key，比较用常量时间。
func (s *server) authorized(r *http.Request) bool {
	keys := s.cfg.Server.APIKeys
	if len(keys) == 0 {
		return true
	}
	got := extractAPIKey(r)
	if got == "" {
		return false
	}
	for _, k := range keys {
		if subtle.ConstantTimeCompare([]byte(got), []byte(k)) == 1 {
			return true
		}
	}
	return false
}

// extractAPIKey 从请求头里取客户端提供的 key（§9.2）。
func extractAPIKey(r *http.Request) string {
	if v := strings.TrimSpace(r.Header.Get("Authorization")); v != "" {
		if len(v) >= 7 && strings.EqualFold(v[:7], "Bearer ") {
			return strings.TrimSpace(v[7:])
		}
		return v
	}
	if v := strings.TrimSpace(r.Header.Get("x-api-key")); v != "" {
		return v
	}
	return ""
}

// writeError 写出统一 OpenAI 错误信封（§9.3）：
// {"error":{"message":"...","type":"...","code":"...","param":null}}。
func writeError(w http.ResponseWriter, status int, code, message, errType string) {
	if errType == "" {
		errType = "invalid_request_error"
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	b, err := json.Marshal(map[string]any{
		"error": map[string]any{
			"message": message,
			"type":    errType,
			"code":    code,
			"param":   nil,
		},
	})
	if err != nil {
		b = []byte(`{"error":{"message":"服务内部错误","type":"internal_error","code":"internal_error","param":null}}`)
	}
	_, _ = w.Write(b)
}

// logRequest 每请求一条 info 日志（§9.4）。
func (s *server) logRequest(clientModel, upstreamModel string, stream bool, tr *tracker, start time.Time) {
	if s.log == nil {
		return
	}
	status := tr.status
	if status == 0 {
		status = http.StatusOK
	}
	firstByteMs := int64(-1)
	if !tr.firstByte.IsZero() {
		firstByteMs = tr.firstByte.Sub(start).Milliseconds()
	}
	msg := fmt.Sprintf("请求: client=%s upstream=%s stream=%t proxy=%s status=%d ms=%d first_byte_ms=%d",
		clientModel, upstreamModel, stream, s.proxyLabel(), status, time.Since(start).Milliseconds(), firstByteMs)
	s.log.info(msg, nil)
}

// proxyLabel 返回日志里的出口标识：单条池用其展示串（direct / host:port），多条池记 pool。
func (s *server) proxyLabel() string {
	if s.pool == nil || len(s.pool.entries) == 0 {
		return "direct"
	}
	if len(s.pool.entries) == 1 {
		return s.pool.entries[0].display
	}
	return "pool"
}

// poolSize 返回代理池条数（/healthz 用）。
func (s *server) poolSize() int {
	if s.pool == nil {
		return 0
	}
	return len(s.pool.entries)
}

// tracker 包装 ResponseWriter，记录状态码与首字节时间，供访问日志用；并透传 Flush。
type tracker struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
	firstByte   time.Time
}

func (t *tracker) WriteHeader(code int) {
	if !t.wroteHeader {
		t.status = code
		t.wroteHeader = true
	}
	t.ResponseWriter.WriteHeader(code)
}

func (t *tracker) Write(b []byte) (int, error) {
	if t.firstByte.IsZero() {
		t.firstByte = time.Now()
	}
	if !t.wroteHeader {
		t.status = http.StatusOK
		t.wroteHeader = true
	}
	return t.ResponseWriter.Write(b)
}

func (t *tracker) Flush() {
	if f, ok := t.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
