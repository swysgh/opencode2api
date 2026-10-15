package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/tidwall/gjson"
)

// testLogger 返回只报 error 的日志器，避免测试输出被 info 噪声淹没。
func testLogger() *logger { return &logger{level: "error"} }

// testUpstreamConfig 用 decodeConfig 构造一份已归一化、指向假上游的配置（§12.2）。
func testUpstreamConfig(t *testing.T, baseURL, policy string) *config {
	t.Helper()
	raw := fmt.Sprintf(`
upstream:
  base_url: %q
  user_agent: "opencode/9.9.9"
  client_name: "cli"
  session_ttl: "30m"
  request_timeout: "5s"
  first_byte_timeout: "5s"
  inject_tools: true
  force_stream: true
  restore_tool_names: true
  nonstream_policy: %q
`, baseURL, policy)
	cfg, err := decodeConfig([]byte(raw))
	if err != nil {
		t.Fatalf("decodeConfig 失败: %v", err)
	}
	return &cfg
}

// sampleUpstreamSSE 是一段真实形状的上游 SSE：content 分片 + finish_reason + usage + [DONE]。
const sampleUpstreamSSE = "" +
	"data: {\"id\":\"chatcmpl-test\",\"object\":\"chat.completion.chunk\",\"created\":1700000000,\"model\":\"mimo-free\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"Hel\"},\"finish_reason\":null}]}\n\n" +
	"data: {\"id\":\"chatcmpl-test\",\"object\":\"chat.completion.chunk\",\"created\":1700000000,\"model\":\"mimo-free\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"lo\"},\"finish_reason\":null}]}\n\n" +
	"data: {\"id\":\"chatcmpl-test\",\"object\":\"chat.completion.chunk\",\"created\":1700000000,\"model\":\"mimo-free\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":2,\"total_tokens\":5}}\n\n" +
	"data: [DONE]\n\n"

// TestBuildUpstreamRequestFingerprint 覆盖 §12.2 #4/#10 的请求侧：
// UA / x-opencode-client / x-opencode-session 指纹、不透传 Authorization、
// 客户端大写工具名 Bash 被改成小写且 bash/glob/grep/read 四件套齐全。
func TestBuildUpstreamRequestFingerprint(t *testing.T) {
	cfg := testUpstreamConfig(t, "http://upstream.invalid", nonStreamForce)

	clientBody := `{
		"model": "oc/mimo-free",
		"stream": true,
		"messages": [{"role": "user", "content": "hi"}],
		"tools": [
			{"type": "function", "function": {"name": "Bash", "description": "", "parameters": {"type": "object", "properties": {}}}},
			{"type": "function", "function": {"name": "Glob", "description": "", "parameters": {"type": "object", "properties": {}}}}
		]
	}`
	clientReq := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(clientBody))
	clientReq.Header.Set("Authorization", "Bearer sk-client-secret")
	clientReq.Header.Set("X-Request-Id", "req-abc-123")

	upReq, renamed, body, err := buildUpstreamRequest(cfg, clientReq, "oc/mimo-free", "mimo-free", true)
	if err != nil {
		t.Fatalf("buildUpstreamRequest 失败: %v", err)
	}

	if upReq.Method != http.MethodPost {
		t.Errorf("方法 = %s，期望 POST", upReq.Method)
	}
	if got := upReq.URL.Path; got != "/chat/completions" {
		t.Errorf("路径 = %q，期望 /chat/completions", got)
	}
	if got := upReq.Header.Get("User-Agent"); got != cfg.Upstream.UserAgent {
		t.Errorf("User-Agent = %q，期望 %q", got, cfg.Upstream.UserAgent)
	}
	if got := upReq.Header.Get("x-opencode-client"); got != "cli" {
		t.Errorf("x-opencode-client = %q，期望 cli", got)
	}
	sid := upReq.Header.Get("x-opencode-session")
	if !regexp.MustCompile(`^ses_[0-9a-f]{12}[0-9a-zA-Z]{14}$`).MatchString(sid) {
		t.Errorf("x-opencode-session = %q，不符合 ses_[0-9a-f]{12}[0-9a-zA-Z]{14}", sid)
	}
	// V2 门禁：客户端 Authorization 绝不透传，但必须显式发 Bearer public（匿名免费档）。
	if got := upReq.Header.Get("Authorization"); got != "Bearer public" {
		t.Errorf("Authorization = %q，期望固定 Bearer public（绝不透传客户端值）", got)
	}
	// V2 门禁：会话四头同值 + project=global。
	for _, h := range []string{"x-session-id", "x-session-affinity", "x-opencode-session-id"} {
		if got := upReq.Header.Get(h); got != sid {
			t.Errorf("%s = %q，期望与 x-opencode-session 同值 %q", h, got, sid)
		}
	}
	if got := upReq.Header.Get("x-opencode-project"); got != "global" {
		t.Errorf("x-opencode-project = %q，期望 global", got)
	}
	if got := upReq.Header.Get("Accept-Encoding"); got != "" {
		t.Errorf("不应设置 Accept-Encoding，实际 = %q", got)
	}
	if got := upReq.Header.Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q，期望 application/json", got)
	}

	if got := gjson.GetBytes(body, "model").String(); got != "mimo-free" {
		t.Errorf("上游 body model = %q，期望 mimo-free", got)
	}

	// 客户端大写工具名被改成小写。
	if got := gjson.GetBytes(body, "tools.0.function.name").String(); got != "bash" {
		t.Errorf("tools[0] 名 = %q，期望小写 bash", got)
	}
	if got := gjson.GetBytes(body, "tools.1.function.name").String(); got != "glob" {
		t.Errorf("tools[1] 名 = %q，期望小写 glob", got)
	}
	// 四件套齐全。
	have := map[string]bool{}
	for _, tc := range gjson.GetBytes(body, "tools").Array() {
		have[tc.Get("function.name").String()] = true
	}
	for _, want := range quartetTools {
		if !have[want] {
			t.Errorf("上游 body 缺少四件套工具 %q，实际 = %v", want, have)
		}
	}
	// 改名映射应记录小写名 → 客户端原拼写。
	if renamed["bash"] != "Bash" || renamed["glob"] != "Glob" {
		t.Errorf("改名映射 = %v，期望 bash→Bash、glob→Glob", renamed)
	}
}

// TestExecuteNonStreamForceAggregate 覆盖 §12.2 #5（在 policy=force 下）：
// 假上游返回真实形状 SSE → 聚合成 chat.completion、content 拼接、model=客户端请求名、usage 保留。
func TestExecuteNonStreamForceAggregate(t *testing.T) {
	var (
		mu       sync.Mutex
		gotBody  []byte
		gotPath  string
		gotForce bool
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		gotBody = b
		gotPath = r.URL.Path
		gotForce = gjson.GetBytes(b, "stream").Bool()
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, sampleUpstreamSSE)
	}))
	defer srv.Close()

	cfg := testUpstreamConfig(t, srv.URL, nonStreamForce)
	pool, err := buildProxyPool(cfg, testLogger())
	if err != nil {
		t.Fatalf("buildProxyPool 失败: %v", err)
	}

	clientBody := `{"model":"oc/mimo-free","stream":false,"messages":[{"role":"user","content":"hi"}]}`
	clientReq := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(clientBody))

	out, ct, ae := executeNonStream(context.Background(), cfg, testLogger(), pool, clientReq, []byte(clientBody), "oc/mimo-free", "mimo-free")
	if ae != nil {
		t.Fatalf("executeNonStream 返回错误: %+v", ae)
	}
	if ct != "application/json" {
		t.Errorf("Content-Type = %q，期望 application/json", ct)
	}

	mu.Lock()
	defer mu.Unlock()
	if gotPath != "/chat/completions" {
		t.Errorf("上游收到路径 = %q，期望 /chat/completions", gotPath)
	}
	if !gotForce {
		t.Errorf("policy=force 应强制上游 stream:true，实际上游 body = %s", gotBody)
	}

	var parsed struct {
		Object  string `json:"object"`
		Model   string `json:"model"`
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			TotalTokens int `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatalf("聚合结果不是合法 JSON: %v\n%s", err, out)
	}
	if parsed.Object != "chat.completion" {
		t.Errorf("object = %q，期望 chat.completion", parsed.Object)
	}
	if parsed.Model != "oc/mimo-free" {
		t.Errorf("model = %q，期望客户端请求名 oc/mimo-free", parsed.Model)
	}
	if len(parsed.Choices) != 1 || parsed.Choices[0].Message.Content != "Hello" {
		t.Errorf("content 聚合错误: %+v", parsed.Choices)
	}
	if parsed.Choices[0].FinishReason != "stop" {
		t.Errorf("finish_reason = %q，期望 stop", parsed.Choices[0].FinishReason)
	}
	if parsed.Usage.TotalTokens != 5 {
		t.Errorf("usage.total_tokens = %d，期望 5", parsed.Usage.TotalTokens)
	}
}

// TestExecuteNonStreamUpstreamError 覆盖 §12.2 #7：上游 403 FreeTierError →
// executeNonStream 返回 *apiError{Status:403, Code:"FreeTierError"}。
func TestExecuteNonStreamUpstreamError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"error":{"type":"FreeTierError","message":"nope"}}`)
	}))
	defer srv.Close()

	cfg := testUpstreamConfig(t, srv.URL, nonStreamForce)
	pool, err := buildProxyPool(cfg, testLogger())
	if err != nil {
		t.Fatalf("buildProxyPool 失败: %v", err)
	}

	clientBody := `{"model":"oc/mimo-free","stream":false,"messages":[{"role":"user","content":"hi"}]}`
	clientReq := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(clientBody))

	_, _, ae := executeNonStream(context.Background(), cfg, testLogger(), pool, clientReq, []byte(clientBody), "oc/mimo-free", "mimo-free")
	if ae == nil {
		t.Fatal("期望 *apiError，实际 nil")
	}
	if ae.Status != http.StatusForbidden {
		t.Errorf("Status = %d，期望 403", ae.Status)
	}
	if ae.Code != "FreeTierError" {
		t.Errorf("Code = %q，期望 FreeTierError", ae.Code)
	}
	if ae.Message != "nope" {
		t.Errorf("Message = %q，期望 nope", ae.Message)
	}
}

// TestRelaySSE 覆盖 §12.2 #4/#10 的响应侧：丢弃心跳/event/空行、每帧恒为
// "data: <payload>\n\n"、无 data: data:、[DONE] 转发、工具名还原、model 改写成客户端请求名。
func TestRelaySSE(t *testing.T) {
	input := "" +
		": 心跳\n" +
		"\n" +
		"event: message\n" +
		"data: {\"id\":\"chatcmpl-test\",\"model\":\"mimo-free\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"a\"},\"finish_reason\":null}]}\n" +
		"\n" +
		"data: {\"id\":\"chatcmpl-test\",\"model\":\"mimo-free\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"name\":\"bash\",\"arguments\":\"{}\"}}]},\"finish_reason\":null}]}\n" +
		"data: {\"id\":\"chatcmpl-test\",\"model\":\"mimo-free\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"b\"},\"finish_reason\":\"stop\"}]}\n" +
		"data: [DONE]\n" +
		"\n"

	var buf bytes.Buffer
	flushes := 0
	flush := func() { flushes++ }

	renamed := map[string]string{"bash": "Bash"}
	if err := relaySSE(context.Background(), strings.NewReader(input), &buf, flush, renamed, "oc/mimo-free", true, testLogger()); err != nil {
		t.Fatalf("relaySSE 返回错误: %v", err)
	}

	out := buf.String()
	if strings.Contains(out, "data: data:") {
		t.Fatalf("输出出现双前缀 data: data:\n%s", out)
	}

	// 逐帧校验：每帧必须是 "data: <payload>\n\n"。
	frames := make([]string, 0)
	for _, chunk := range strings.Split(out, "\n\n") {
		if chunk == "" {
			continue
		}
		frames = append(frames, chunk)
	}
	if len(frames) != 4 {
		t.Fatalf("帧数 = %d，期望 4\n输出:\n%s", len(frames), out)
	}
	for i, frame := range frames {
		if !strings.HasPrefix(frame, "data: ") {
			t.Errorf("第 %d 帧不是 data: 前缀: %q", i, frame)
		}
	}

	// 第 1 帧：model 改写成客户端请求名。
	var f0 struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal([]byte(strings.TrimPrefix(frames[0], "data: ")), &f0); err != nil {
		t.Fatalf("第 1 帧不是合法 JSON: %v", err)
	}
	if f0.Model != "oc/mimo-free" {
		t.Errorf("第 1 帧 model = %q，期望 oc/mimo-free", f0.Model)
	}

	// 第 2 帧：工具名 bash → Bash。
	if got := gjson.Get(strings.TrimPrefix(frames[1], "data: "), "choices.0.delta.tool_calls.0.function.name").String(); got != "Bash" {
		t.Errorf("第 2 帧工具名 = %q，期望还原为 Bash", got)
	}

	// 第 4 帧：[DONE] 原样转发。
	if frames[3] != "data: [DONE]" {
		t.Errorf("最后一帧 = %q，期望 data: [DONE]", frames[3])
	}
	if flushes != 4 {
		t.Errorf("flush 次数 = %d，期望 4", flushes)
	}
}
