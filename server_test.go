package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tidwall/gjson"
)

// srvTestKey 是测试配置里统一的合法 API key。
const srvTestKey = "sk-test-1234"

// srvTestOpts 描述一段测试配置。每个用例各自 decodeConfig，互不共用可变全局状态（§12.2）。
type srvTestOpts struct {
	BaseURL      string // 上游基址，通常是假上游的 URL
	APIKeys      string // YAML 内联，如 `["sk-test-1234"]`；空则默认合法 key
	MaxBodyBytes string // 如 "100"；空则不写
	Models       string // 如 `["mimo-free"]`；空则默认 `["mimo-free", "big-pickle"]`
	Policy       string // nonstream_policy，如 "pass"/"reject"；空则不写（默认 force）
	Aliases      string // 缩进好的 model_aliases 块（4 空格）
	Proxies      string // 顶层 proxy 块
}

// srvTestConfig 解析一段指向假上游的测试配置。
func srvTestConfig(t *testing.T, o srvTestOpts) *config {
	t.Helper()
	if o.BaseURL == "" {
		o.BaseURL = "http://upstream.invalid"
	}
	if o.APIKeys == "" {
		o.APIKeys = `["` + srvTestKey + `"]`
	}
	if o.Models == "" {
		o.Models = `["mimo-free", "big-pickle"]`
	}

	var b strings.Builder
	b.WriteString("server:\n")
	fmt.Fprintf(&b, "  api_keys: %s\n", o.APIKeys)
	b.WriteString("  log_level: error\n")
	if o.MaxBodyBytes != "" {
		fmt.Fprintf(&b, "  max_body_bytes: %s\n", o.MaxBodyBytes)
	}
	b.WriteString("upstream:\n")
	fmt.Fprintf(&b, "  base_url: %q\n", o.BaseURL)
	fmt.Fprintf(&b, "  models: %s\n", o.Models)
	b.WriteString("  user_agent: \"opencode/9.9.9\"\n")
	b.WriteString("  request_timeout: \"5s\"\n")
	b.WriteString("  first_byte_timeout: \"5s\"\n")
	if o.Policy != "" {
		fmt.Fprintf(&b, "  nonstream_policy: %s\n", o.Policy)
	}
	if o.Aliases != "" {
		b.WriteString("  model_aliases:\n")
		b.WriteString(o.Aliases)
	}
	if o.Proxies != "" {
		b.WriteString(o.Proxies)
	}

	cfg, err := decodeConfig([]byte(b.String()))
	if err != nil {
		t.Fatalf("解析测试配置失败: %v\n配置:\n%s", err, b.String())
	}
	return &cfg
}

// srvTestHandler 用 routes(...) 构造对外 handler。catalog 传 nil：测试不触发目录拉取，
// 完全依赖 rt.setExposed 注入的暴露集合。
func srvTestHandler(t *testing.T, cfg *config, exposed []string) (http.Handler, *proxyPool) {
	t.Helper()
	pool, err := buildProxyPool(cfg, testLogger())
	if err != nil {
		t.Fatalf("buildProxyPool 失败: %v", err)
	}
	rt := newRouter(cfg.Upstream.ModelPrefix, cfg.Upstream.ModelAliases)
	rt.setExposed(exposed)
	return routes(cfg, testLogger(), pool, rt, nil, time.Now()), pool
}

// srvDo 打一次 handler；key 非空则带 Bearer。
func srvDo(h http.Handler, method, path, key, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// srvErrCode 从错误信封里取 code。
func srvErrCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var env struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("响应不是错误信封: %v\n%s", err, rec.Body.String())
	}
	return env.Error.Code
}

// srvSSEFrames 把响应体按空行切成帧（忽略空白帧）。
func srvSSEFrames(body string) []string {
	var frames []string
	for _, part := range strings.Split(body, "\n\n") {
		if strings.TrimSpace(part) == "" {
			continue
		}
		frames = append(frames, part)
	}
	return frames
}

// srvStreamSSE 是假上游吐出的真实形状 SSE：content 分片 + tool_calls 分片 +
// finish_reason + usage + data: [DONE]（§12.2 #4）。
const srvStreamSSE = "" +
	`data: {"id":"chatcmpl-test","object":"chat.completion.chunk","created":1700000000,"model":"mimo-free","choices":[{"index":0,"delta":{"role":"assistant","content":"Hel"},"finish_reason":null}]}` + "\n\n" +
	`data: {"id":"chatcmpl-test","object":"chat.completion.chunk","created":1700000000,"model":"mimo-free","choices":[{"index":0,"delta":{"content":"lo"},"finish_reason":null}]}` + "\n\n" +
	`data: {"id":"chatcmpl-test","object":"chat.completion.chunk","created":1700000000,"model":"mimo-free","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"bash","arguments":"{\"cmd\":\"ls\"}"}}]},"finish_reason":null}]}` + "\n\n" +
	`data: {"id":"chatcmpl-test","object":"chat.completion.chunk","created":1700000000,"model":"mimo-free","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}` + "\n\n" +
	`data: [DONE]` + "\n\n"

// TestServerAuth 覆盖 §12.2 #3：无 key → 401 invalid_api_key；错 key → 401；
// 对 key → 200；/healthz 无 key → 200。
func TestServerAuth(t *testing.T) {
	cfg := srvTestConfig(t, srvTestOpts{})
	h, _ := srvTestHandler(t, cfg, []string{"mimo-free"})

	// 无 key → 401。
	rec := srvDo(h, http.MethodGet, "/v1/models", "", "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("无 key /v1/models 状态 = %d，期望 401", rec.Code)
	}
	if code := srvErrCode(t, rec); code != "invalid_api_key" {
		t.Fatalf("无 key code = %q，期望 invalid_api_key", code)
	}

	// 错 key → 401。
	rec = srvDo(h, http.MethodGet, "/v1/models", "sk-wrong", "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("错 key /v1/models 状态 = %d，期望 401", rec.Code)
	}
	if code := srvErrCode(t, rec); code != "invalid_api_key" {
		t.Fatalf("错 key code = %q，期望 invalid_api_key", code)
	}

	// 对 key → 200，且列出客户端调用名。
	rec = srvDo(h, http.MethodGet, "/v1/models", srvTestKey, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("对 key /v1/models 状态 = %d，期望 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"oc/mimo-free"`) {
		t.Fatalf("/v1/models 未列出 oc/mimo-free: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"object":"list"`) {
		t.Fatalf("/v1/models 不是列表形状: %s", rec.Body.String())
	}

	// /healthz 无 key → 200。
	rec = srvDo(h, http.MethodGet, "/healthz", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("/healthz 无 key 状态 = %d，期望 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"status":"ok"`) {
		t.Fatalf("/healthz body = %s，期望含 status=ok", rec.Body.String())
	}
}

// TestServerModelResolution 覆盖 §12.2 #2：oc/<id>、别名、裸上游 id 都解析到同一
// 上游 id；未暴露的名字 → 400 model_not_found。
func TestServerModelResolution(t *testing.T) {
	var (
		mu       sync.Mutex
		gotModel string
	)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		gotModel = gjson.GetBytes(b, "model").String()
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer upstream.Close()

	cfg := srvTestConfig(t, srvTestOpts{
		BaseURL: upstream.URL,
		Policy:  "pass",
		Aliases: "    mimo-free: [\"zen-mimo\"]\n",
	})
	h, _ := srvTestHandler(t, cfg, []string{"mimo-free"})

	for _, name := range []string{"oc/mimo-free", "zen-mimo", "mimo-free"} {
		body := fmt.Sprintf(`{"model":%q,"stream":false,"messages":[{"role":"user","content":"hi"}]}`, name)
		rec := srvDo(h, http.MethodPost, "/v1/chat/completions", srvTestKey, body)
		if rec.Code != http.StatusOK {
			t.Fatalf("请求名 %q 状态 = %d，期望 200；body=%s", name, rec.Code, rec.Body.String())
		}
		mu.Lock()
		m := gotModel
		mu.Unlock()
		if m != "mimo-free" {
			t.Fatalf("请求名 %q 应解析到上游 mimo-free，实际 %q", name, m)
		}
	}

	// 未暴露 → 400 model_not_found。
	body := `{"model":"oc/nope","stream":false,"messages":[{"role":"user","content":"hi"}]}`
	rec := srvDo(h, http.MethodPost, "/v1/chat/completions", srvTestKey, body)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("未暴露模型状态 = %d，期望 400", rec.Code)
	}
	if code := srvErrCode(t, rec); code != "model_not_found" {
		t.Fatalf("未暴露模型 code = %q，期望 model_not_found", code)
	}
}

// TestServerNonStreamReject 覆盖 §12.2 #6 的 reject 分支：直接 400 code=nonstream_rejected，
// 且不发上游请求。
func TestServerNonStreamReject(t *testing.T) {
	calls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	cfg := srvTestConfig(t, srvTestOpts{BaseURL: upstream.URL, Policy: "reject"})
	h, _ := srvTestHandler(t, cfg, []string{"mimo-free"})

	body := `{"model":"oc/mimo-free","stream":false,"messages":[{"role":"user","content":"hi"}]}`
	rec := srvDo(h, http.MethodPost, "/v1/chat/completions", srvTestKey, body)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("reject 状态 = %d，期望 400", rec.Code)
	}
	if code := srvErrCode(t, rec); code != "nonstream_rejected" {
		t.Fatalf("reject code = %q，期望 nonstream_rejected", code)
	}
	if calls != 0 {
		t.Fatalf("reject 不应发上游请求，实际 %d 次", calls)
	}
}

// TestServerNonStreamPassUpstreamError 覆盖 §12.2 #6/#7：policy=pass 时上游 403 +
// FreeTierError 原样透出，客户端看到 403 与上游 code。
func TestServerNonStreamPassUpstreamError(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"error":{"type":"FreeTierError","message":"免费档不可用"}}`)
	}))
	defer upstream.Close()

	cfg := srvTestConfig(t, srvTestOpts{BaseURL: upstream.URL, Policy: "pass"})
	h, _ := srvTestHandler(t, cfg, []string{"mimo-free"})

	body := `{"model":"oc/mimo-free","stream":false,"messages":[{"role":"user","content":"hi"}]}`
	rec := srvDo(h, http.MethodPost, "/v1/chat/completions", srvTestKey, body)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("pass 上游 403 状态 = %d，期望 403；body=%s", rec.Code, rec.Body.String())
	}
	if code := srvErrCode(t, rec); code != "FreeTierError" {
		t.Fatalf("pass 上游 code = %q，期望 FreeTierError", code)
	}
	if !strings.Contains(rec.Body.String(), "免费档不可用") {
		t.Fatalf("pass 未透出上游 message: %s", rec.Body.String())
	}
}

// TestServerOversizedBody 覆盖 §12.2 #9：body 超过 max_body_bytes → 413 request_too_large。
func TestServerOversizedBody(t *testing.T) {
	cfg := srvTestConfig(t, srvTestOpts{MaxBodyBytes: "100", Policy: "pass"})
	h, _ := srvTestHandler(t, cfg, []string{"mimo-free"})

	big := `{"model":"oc/mimo-free","stream":false,"messages":[{"role":"user","content":"` +
		strings.Repeat("x", 500) + `"}]}`
	rec := srvDo(h, http.MethodPost, "/v1/chat/completions", srvTestKey, big)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("超大 body 状态 = %d，期望 413；body=%s", rec.Code, rec.Body.String())
	}
	if code := srvErrCode(t, rec); code != "request_too_large" {
		t.Fatalf("超大 body code = %q，期望 request_too_large", code)
	}
}

// TestServerStreamRelayEndToEnd 覆盖 §12.2 #4/#10：假上游吐真实形状 SSE →
// 客户端收到 text/event-stream、每帧 data:、无 data: data:、[DONE] 转发、
// 帧内 model 是客户端请求名、工具名从上游小写 bash 还原成客户端声明的 Bash；
// 同时断言发往上游的 body 里工具名是小写 bash。
func TestServerStreamRelayEndToEnd(t *testing.T) {
	var (
		mu      sync.Mutex
		gotBody []byte
	)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		gotBody = b
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, srvStreamSSE)
	}))
	defer upstream.Close()

	cfg := srvTestConfig(t, srvTestOpts{BaseURL: upstream.URL})
	h, _ := srvTestHandler(t, cfg, []string{"mimo-free"})

	clientBody := `{"model":"oc/mimo-free","stream":true,"messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"Bash","description":"","parameters":{"type":"object","properties":{}}}}]}`
	rec := srvDo(h, http.MethodPost, "/v1/chat/completions", srvTestKey, clientBody)
	if rec.Code != http.StatusOK {
		t.Fatalf("流式状态 = %d，期望 200；body=%s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("Content-Type = %q，期望 text/event-stream", ct)
	}

	out := rec.Body.String()
	if strings.Contains(out, "data: data:") {
		t.Fatalf("响应出现双前缀 data: data:\n%s", out)
	}
	frames := srvSSEFrames(out)
	if len(frames) != 5 {
		t.Fatalf("帧数 = %d，期望 5\n输出:\n%s", len(frames), out)
	}
	for i, frame := range frames {
		if !strings.HasPrefix(frame, "data: ") {
			t.Fatalf("第 %d 帧不是 data: 前缀: %q", i, frame)
		}
	}
	// 前四帧是 JSON：model 必须是客户端请求名。
	for i := 0; i < 4; i++ {
		payload := strings.TrimPrefix(frames[i], "data: ")
		if got := gjson.Get(payload, "model").String(); got != "oc/mimo-free" {
			t.Fatalf("第 %d 帧 model = %q，期望 oc/mimo-free", i, got)
		}
	}
	// content 分片拼接形状保留。
	if got := gjson.Get(strings.TrimPrefix(frames[0], "data: "), "choices.0.delta.content").String(); got != "Hel" {
		t.Fatalf("第 0 帧 content = %q，期望 Hel", got)
	}
	// 工具名还原：上游 bash → 客户端 Bash。
	if got := gjson.Get(strings.TrimPrefix(frames[2], "data: "), "choices.0.delta.tool_calls.0.function.name").String(); got != "Bash" {
		t.Fatalf("第 2 帧工具名 = %q，期望还原为 Bash", got)
	}
	// [DONE] 被转发。
	if frames[4] != "data: [DONE]" {
		t.Fatalf("最后一帧 = %q，期望 data: [DONE]", frames[4])
	}

	// 请求侧：客户端声明的 Bash 被改写成上游要求的小写 bash。
	mu.Lock()
	defer mu.Unlock()
	if got := gjson.GetBytes(gotBody, "tools.0.function.name").String(); got != "bash" {
		t.Fatalf("发往上游的工具名 = %q，期望小写 bash", got)
	}
}

// TestServerProxyFailover 覆盖 §12.2 #8（端到端）：池里第一条指向必连不上的地址，
// 第二条 direct 打到假上游 → 请求最终成功，且第一条进入冷却。
func TestServerProxyFailover(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, srvStreamSSE)
	}))
	defer upstream.Close()

	cfg := srvTestConfig(t, srvTestOpts{
		BaseURL: upstream.URL,
		Proxies: "proxy:\n  proxies: [\"http://127.0.0.1:1\", direct]\n  max_attempts: 2\n",
	})
	h, pool := srvTestHandler(t, cfg, []string{"mimo-free"})

	clientBody := `{"model":"oc/mimo-free","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	rec := srvDo(h, http.MethodPost, "/v1/chat/completions", srvTestKey, clientBody)
	if rec.Code != http.StatusOK {
		t.Fatalf("代理失败换下一个后状态 = %d，期望 200；body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "data: [DONE]") {
		t.Fatalf("响应未包含 [DONE]，代理未成功打到假上游: %s", rec.Body.String())
	}
	if len(pool.entries) != 2 {
		t.Fatalf("代理池大小 = %d，期望 2", len(pool.entries))
	}
	if cooling, _ := pool.entries[0].cooldownInfo(time.Now()); !cooling {
		t.Fatal("第一条失败代理应进入冷却")
	}
}

// TestServerRouting 覆盖 §9.1：方法不符 → 405；其它 /v1/* → 404。
func TestServerRouting(t *testing.T) {
	cfg := srvTestConfig(t, srvTestOpts{})
	h, _ := srvTestHandler(t, cfg, []string{"mimo-free"})

	// GET /v1/chat/completions → 405。
	rec := srvDo(h, http.MethodGet, "/v1/chat/completions", srvTestKey, "")
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET /v1/chat/completions 状态 = %d，期望 405", rec.Code)
	}
	if code := srvErrCode(t, rec); code != "method_not_allowed" {
		t.Fatalf("方法不符 code = %q，期望 method_not_allowed", code)
	}

	// POST /v1/models → 405。
	rec = srvDo(h, http.MethodPost, "/v1/models", srvTestKey, `{}`)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST /v1/models 状态 = %d，期望 405", rec.Code)
	}

	// 其它 /v1/* → 404。
	rec = srvDo(h, http.MethodGet, "/v1/whatever", srvTestKey, "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("GET /v1/whatever 状态 = %d，期望 404", rec.Code)
	}
	if code := srvErrCode(t, rec); code != "not_found" {
		t.Fatalf("未知路径 code = %q，期望 not_found", code)
	}

	// GET /v1 → 404。
	rec = srvDo(h, http.MethodGet, "/v1", srvTestKey, "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("GET /v1 状态 = %d，期望 404", rec.Code)
	}
}
