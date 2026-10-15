package main

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tidwall/gjson"
)

// loadResponsesFixture 读取只读的上游抓包（testdata/responses_*.sse）。
func loadResponsesFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("读取 fixture %q 失败: %v", name, err)
	}
	return b
}

// responsesDataPayload 返回 fixture 里所有 data: 行的载荷（去前缀、去空白）。
func responsesDataPayloads(fixture []byte) []string {
	var out []string
	for _, line := range strings.Split(string(fixture), "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "data:") {
			continue
		}
		p := strings.TrimSpace(strings.TrimPrefix(trimmed, "data:"))
		if p == "" || p == "[DONE]" || !gjson.Valid(p) {
			continue
		}
		out = append(out, p)
	}
	return out
}

// terminalResponse 返回 fixture 里 response.completed / response.incomplete 的 response 对象。
func terminalResponse(t *testing.T, fixture []byte) gjson.Result {
	t.Helper()
	for _, p := range responsesDataPayloads(fixture) {
		switch gjson.Get(p, "type").String() {
		case "response.completed", "response.incomplete":
			return gjson.Get(p, "response")
		}
	}
	t.Fatalf("fixture 里找不到终结事件")
	return gjson.Result{}
}

// translateFixture 用 responsesTranslator 把 fixture 翻成 chunk 帧（feed + finalize）。
func translateFixture(fixture []byte, clientModel string) []gjson.Result {
	tr := newResponsesTranslator(clientModel)
	var frames [][]byte
	for _, p := range responsesDataPayloads(fixture) {
		frames = append(frames, tr.feed([]byte(p))...)
	}
	frames = append(frames, tr.finalize()...)
	out := make([]gjson.Result, len(frames))
	for i, f := range frames {
		out[i] = gjson.ParseBytes(f)
	}
	return out
}

// joinDeltaContent 拼接所有帧里的 delta.content。
func joinDeltaContent(frames []gjson.Result) string {
	var b strings.Builder
	for _, f := range frames {
		b.WriteString(f.Get("choices.0.delta.content").String())
	}
	return b.String()
}

// joinToolArguments 拼接某下标 tool_calls 的 arguments 增量。
func joinToolArguments(frames []gjson.Result, index int) string {
	var b strings.Builder
	for _, f := range frames {
		for _, tc := range f.Get("choices.0.delta.tool_calls").Array() {
			if int(tc.Get("index").Int()) == index {
				b.WriteString(tc.Get("function.arguments").String())
			}
		}
	}
	return b.String()
}

// lastFinishReason 返回最后一个带非空 finish_reason 的帧的值。
func lastFinishReason(frames []gjson.Result) string {
	out := ""
	for _, f := range frames {
		if fr := f.Get("choices.0.finish_reason"); fr.Exists() && fr.Type != gjson.Null {
			out = fr.String()
		}
	}
	return out
}

// TestChatToResponsesBody 表驱动覆盖 §3.1~§3.4 的字段翻译与丢弃规则。
func TestChatToResponsesBody(t *testing.T) {
	const up = "muse-spark-1.3-contributor-free"

	cases := []struct {
		name string
		body string
		// check 返回错误描述；空串表示通过。
		check func(t *testing.T, out []byte, warnings []string)
	}{
		{
			name: "system 与 user 字符串",
			body: `{"model":"oc/muse","messages":[{"role":"system","content":"SYS"},{"role":"user","content":"hi"}]}`,
			check: func(t *testing.T, out []byte, _ []string) {
				if got := gjson.GetBytes(out, "model").String(); got != up {
					t.Fatalf("model = %q，期望 %q", got, up)
				}
				if !gjson.GetBytes(out, "stream").Bool() {
					t.Fatal("stream 应被强制为 true")
				}
				if got := gjson.GetBytes(out, "instructions").String(); got != "SYS" {
					t.Fatalf("instructions = %q，期望 SYS", got)
				}
				if got := gjson.GetBytes(out, "input.0.type").String(); got != "message" {
					t.Fatalf("input.0.type = %q", got)
				}
				if got := gjson.GetBytes(out, "input.0.role").String(); got != "user" {
					t.Fatalf("input.0.role = %q", got)
				}
				if got := gjson.GetBytes(out, "input.0.content.0.type").String(); got != "input_text" {
					t.Fatalf("input.0.content.0.type = %q", got)
				}
				if got := gjson.GetBytes(out, "input.0.content.0.text").String(); got != "hi" {
					t.Fatalf("input.0.content.0.text = %q", got)
				}
			},
		},
		{
			name: "多 system 合并",
			body: `{"model":"oc/muse","messages":[{"role":"system","content":"A"},{"role":"developer","content":"B"},{"role":"user","content":"hi"}]}`,
			check: func(t *testing.T, out []byte, _ []string) {
				if got := gjson.GetBytes(out, "instructions").String(); got != "A\n\nB" {
					t.Fatalf("instructions = %q，期望 A\\n\\nB", got)
				}
			},
		},
		{
			name: "user parts 与 image_url",
			body: `{"model":"oc/muse","messages":[{"role":"user","content":[{"type":"text","text":"hello"},{"type":"image_url","image_url":{"url":"http://x/y.png"}}]}]}`,
			check: func(t *testing.T, out []byte, _ []string) {
				if got := gjson.GetBytes(out, "input.0.content.0.type").String(); got != "input_text" {
					t.Fatalf("part0.type = %q", got)
				}
				if got := gjson.GetBytes(out, "input.0.content.1.type").String(); got != "input_image" {
					t.Fatalf("part1.type = %q", got)
				}
				if got := gjson.GetBytes(out, "input.0.content.1.image_url").String(); got != "http://x/y.png" {
					t.Fatalf("part1.image_url = %q", got)
				}
			},
		},
		{
			name: "user 不支持 part 告警",
			body: `{"model":"oc/muse","messages":[{"role":"user","content":[{"type":"input_audio","input_audio":{}}]}]}`,
			check: func(t *testing.T, out []byte, warnings []string) {
				if !hasWarning(warnings, "丢弃不支持的消息内容类型 input_audio") {
					t.Fatalf("缺少告警，实际 = %v", warnings)
				}
			},
		},
		{
			name: "assistant content 与 tool_calls",
			body: `{"model":"oc/muse","messages":[{"role":"assistant","content":"thinking","tool_calls":[{"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"Beijing\"}"}}]}]}`,
			check: func(t *testing.T, out []byte, _ []string) {
				if got := gjson.GetBytes(out, "input.0.type").String(); got != "message" {
					t.Fatalf("input.0.type = %q", got)
				}
				if got := gjson.GetBytes(out, "input.0.content.0.type").String(); got != "output_text" {
					t.Fatalf("assistant content type = %q", got)
				}
				if got := gjson.GetBytes(out, "input.1.type").String(); got != "function_call" {
					t.Fatalf("input.1.type = %q", got)
				}
				if got := gjson.GetBytes(out, "input.1.call_id").String(); got != "call_1" {
					t.Fatalf("call_id = %q", got)
				}
				if got := gjson.GetBytes(out, "input.1.name").String(); got != "get_weather" {
					t.Fatalf("name = %q", got)
				}
				if got := gjson.GetBytes(out, "input.1.arguments").String(); got != `{"city":"Beijing"}` {
					t.Fatalf("arguments = %q", got)
				}
			},
		},
		{
			name: "assistant 空 content 不产出 message",
			body: `{"model":"oc/muse","messages":[{"role":"assistant","content":"","tool_calls":[{"id":"call_1","function":{"name":"f","arguments":"{}"}}]}]}`,
			check: func(t *testing.T, out []byte, _ []string) {
				if got := gjson.GetBytes(out, "input.0.type").String(); got != "function_call" {
					t.Fatalf("input.0.type = %q，期望 function_call", got)
				}
				if gjson.GetBytes(out, "input.1").Exists() {
					t.Fatal("空 content 不该额外产出 message item")
				}
			},
		},
		{
			name: "tool 消息",
			body: `{"model":"oc/muse","messages":[{"role":"user","content":"hi"},{"role":"tool","tool_call_id":"call_9","content":"result"}]}`,
			check: func(t *testing.T, out []byte, _ []string) {
				if got := gjson.GetBytes(out, "input.1.type").String(); got != "function_call_output" {
					t.Fatalf("input.1.type = %q", got)
				}
				if got := gjson.GetBytes(out, "input.1.call_id").String(); got != "call_9" {
					t.Fatalf("call_id = %q", got)
				}
				if got := gjson.GetBytes(out, "input.1.output").String(); got != "result" {
					t.Fatalf("output = %q", got)
				}
			},
		},
		{
			name: "tool 缺 call_id 告警并跳过",
			body: `{"model":"oc/muse","messages":[{"role":"user","content":"hi"},{"role":"tool","content":"result"}]}`,
			check: func(t *testing.T, out []byte, warnings []string) {
				if gjson.GetBytes(out, "input.1").Exists() {
					t.Fatal("缺 call_id 的 tool 消息应被跳过")
				}
				if !hasWarning(warnings, "丢弃缺少 call_id 的 tool 消息") {
					t.Fatalf("缺少告警，实际 = %v", warnings)
				}
			},
		},
		{
			name: "未知 role 告警",
			body: `{"model":"oc/muse","messages":[{"role":"user","content":"hi"},{"role":"weird","content":"x"}]}`,
			check: func(t *testing.T, out []byte, warnings []string) {
				if gjson.GetBytes(out, "input.1").Exists() {
					t.Fatal("未知 role 应被跳过")
				}
				if !hasWarning(warnings, "丢弃未知 role weird") {
					t.Fatalf("缺少告警，实际 = %v", warnings)
				}
			},
		},
		{
			name: "tools 形状转换",
			body: `{"model":"oc/muse","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"get_weather","description":"d","parameters":{"type":"object"}}}]}`,
			check: func(t *testing.T, out []byte, _ []string) {
				if got := gjson.GetBytes(out, "tools.0.type").String(); got != "function" {
					t.Fatalf("tools.0.type = %q", got)
				}
				if got := gjson.GetBytes(out, "tools.0.name").String(); got != "get_weather" {
					t.Fatalf("tools.0.name = %q", got)
				}
				if got := gjson.GetBytes(out, "tools.0.description").String(); got != "d" {
					t.Fatalf("tools.0.description = %q", got)
				}
				if got := gjson.GetBytes(out, "tools.0.parameters.type").String(); got != "object" {
					t.Fatalf("tools.0.parameters.type = %q", got)
				}
				if gjson.GetBytes(out, "tools.0.function").Exists() {
					t.Fatal("responses 形状不该有嵌套 function 键")
				}
			},
		},
		{
			name: "tools 缺 function 告警并跳过",
			body: `{"model":"oc/muse","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function"}]}`,
			check: func(t *testing.T, out []byte, warnings []string) {
				if gjson.GetBytes(out, "tools").Exists() {
					t.Fatal("空的 tools 不该输出该键")
				}
				if !hasWarning(warnings, "丢弃缺少 function 的工具声明") {
					t.Fatalf("缺少告警，实际 = %v", warnings)
				}
			},
		},
		{
			name: "tool_choice auto 保留",
			body: `{"model":"oc/muse","messages":[{"role":"user","content":"hi"}],"tool_choice":"auto"}`,
			check: func(t *testing.T, out []byte, _ []string) {
				if got := gjson.GetBytes(out, "tool_choice").String(); got != "auto" {
					t.Fatalf("tool_choice = %q，期望 auto", got)
				}
			},
		},
		{
			name: "tool_choice none 丢弃",
			body: `{"model":"oc/muse","messages":[{"role":"user","content":"hi"}],"tool_choice":"none"}`,
			check: func(t *testing.T, out []byte, warnings []string) {
				if gjson.GetBytes(out, "tool_choice").Exists() {
					t.Fatal("tool_choice=none 应被丢弃")
				}
				if !hasWarning(warnings, "tool_choice 仅支持 auto，已丢弃") {
					t.Fatalf("缺少告警，实际 = %v", warnings)
				}
			},
		},
		{
			name: "tool_choice 具名对象丢弃",
			body: `{"model":"oc/muse","messages":[{"role":"user","content":"hi"}],"tool_choice":{"type":"function","function":{"name":"x"}}}`,
			check: func(t *testing.T, out []byte, warnings []string) {
				if gjson.GetBytes(out, "tool_choice").Exists() {
					t.Fatal("具名 tool_choice 应被丢弃")
				}
				if !hasWarning(warnings, "tool_choice 仅支持 auto，已丢弃") {
					t.Fatalf("缺少告警，实际 = %v", warnings)
				}
			},
		},
		{
			name: "max_tokens 改名",
			body: `{"model":"oc/muse","messages":[{"role":"user","content":"hi"}],"max_tokens":123}`,
			check: func(t *testing.T, out []byte, _ []string) {
				if got := gjson.GetBytes(out, "max_output_tokens").Int(); got != 123 {
					t.Fatalf("max_output_tokens = %d，期望 123", got)
				}
				if gjson.GetBytes(out, "max_tokens").Exists() {
					t.Fatal("max_tokens 不该保留")
				}
			},
		},
		{
			name: "max_output_tokens 优先",
			body: `{"model":"oc/muse","messages":[{"role":"user","content":"hi"}],"max_tokens":1,"max_completion_tokens":2,"max_output_tokens":3}`,
			check: func(t *testing.T, out []byte, _ []string) {
				if got := gjson.GetBytes(out, "max_output_tokens").Int(); got != 3 {
					t.Fatalf("max_output_tokens = %d，期望 3", got)
				}
			},
		},
		{
			name: "max_completion_tokens 次选",
			body: `{"model":"oc/muse","messages":[{"role":"user","content":"hi"}],"max_tokens":1,"max_completion_tokens":2}`,
			check: func(t *testing.T, out []byte, _ []string) {
				if got := gjson.GetBytes(out, "max_output_tokens").Int(); got != 2 {
					t.Fatalf("max_output_tokens = %d，期望 2", got)
				}
			},
		},
		{
			name: "丢弃字段与未知键",
			body: `{"model":"oc/muse","messages":[{"role":"user","content":"hi"}],"stop":["x"],"seed":1,"response_format":{"type":"json_object"},"logprobs":true,"top_logprobs":2,"stream_options":{"include_usage":true},"unknown_key":1}`,
			check: func(t *testing.T, out []byte, warnings []string) {
				for _, k := range []string{"stop", "seed", "response_format", "logprobs", "top_logprobs", "stream_options", "unknown_key"} {
					if gjson.GetBytes(out, k).Exists() {
						t.Fatalf("%s 应被丢弃", k)
					}
				}
				for _, k := range []string{"stop", "seed", "response_format", "logprobs", "top_logprobs", "unknown_key"} {
					if !hasWarning(warnings, "丢弃不支持的字段 "+k) {
						t.Fatalf("缺少 %s 的告警，实际 = %v", k, warnings)
					}
				}
			},
		},
		{
			name: "n<=1 静默",
			body: `{"model":"oc/muse","messages":[{"role":"user","content":"hi"}],"n":1}`,
			check: func(t *testing.T, _ []byte, warnings []string) {
				if hasWarning(warnings, "不支持多候选（n>1），已按单候选处理") {
					t.Fatalf("n<=1 不该告警，实际 = %v", warnings)
				}
			},
		},
		{
			name: "n>1 告警",
			body: `{"model":"oc/muse","messages":[{"role":"user","content":"hi"}],"n":3}`,
			check: func(t *testing.T, out []byte, warnings []string) {
				if gjson.GetBytes(out, "n").Exists() {
					t.Fatal("n 应被丢弃")
				}
				if !hasWarning(warnings, "不支持多候选（n>1），已按单候选处理") {
					t.Fatalf("缺少告警，实际 = %v", warnings)
				}
			},
		},
		{
			name: "直通字段",
			body: `{"model":"oc/muse","messages":[{"role":"user","content":"hi"}],"temperature":0.2,"top_p":0.9,"presence_penalty":0.1,"frequency_penalty":0.3,"user":"u1","store":true,"metadata":{"a":1},"reasoning":{"effort":"high"},"parallel_tool_calls":false,"truncation":"auto","service_tier":"default"}`,
			check: func(t *testing.T, out []byte, _ []string) {
				if got := gjson.GetBytes(out, "temperature").Float(); got != 0.2 {
					t.Fatalf("temperature = %v", got)
				}
				if got := gjson.GetBytes(out, "top_p").Float(); got != 0.9 {
					t.Fatalf("top_p = %v", got)
				}
				if got := gjson.GetBytes(out, "presence_penalty").Float(); got != 0.1 {
					t.Fatalf("presence_penalty = %v", got)
				}
				if got := gjson.GetBytes(out, "frequency_penalty").Float(); got != 0.3 {
					t.Fatalf("frequency_penalty = %v", got)
				}
				if got := gjson.GetBytes(out, "user").String(); got != "u1" {
					t.Fatalf("user = %q", got)
				}
				if !gjson.GetBytes(out, "store").Bool() {
					t.Fatal("store 应直通")
				}
				if got := gjson.GetBytes(out, "metadata.a").Int(); got != 1 {
					t.Fatalf("metadata.a = %d", got)
				}
				if got := gjson.GetBytes(out, "reasoning.effort").String(); got != "high" {
					t.Fatalf("reasoning.effort = %q", got)
				}
				if gjson.GetBytes(out, "parallel_tool_calls").Bool() {
					t.Fatal("parallel_tool_calls 应直通 false")
				}
				if got := gjson.GetBytes(out, "truncation").String(); got != "auto" {
					t.Fatalf("truncation = %q", got)
				}
				if got := gjson.GetBytes(out, "service_tier").String(); got != "default" {
					t.Fatalf("service_tier = %q", got)
				}
			},
		},
		{
			name: "非法 JSON 报错",
			body: `{`,
			check: func(t *testing.T, _ []byte, _ []string) {
				// 错误由外层 err 断言处理；这里不应被调用。
			},
		},
		{
			name:  "messages 翻译后为空报错",
			body:  `{"model":"oc/muse","messages":[{"role":"system","content":"only sys"}]}`,
			check: func(t *testing.T, _ []byte, _ []string) {},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, warnings, err := chatToResponsesBody([]byte(tc.body), up)
			if tc.name == "非法 JSON 报错" || tc.name == "messages 翻译后为空报错" {
				if err == nil {
					t.Fatalf("期望 error，实际通过；out=%s", out)
				}
				return
			}
			if err != nil {
				t.Fatalf("chatToResponsesBody 失败: %v", err)
			}
			tc.check(t, out, warnings)
		})
	}
}

func hasWarning(warnings []string, want string) bool {
	for _, w := range warnings {
		if w == want {
			return true
		}
	}
	return false
}

// TestResponsesTranslatorText 覆盖 §8.2 文本 fixture：首帧、content、终帧、usage、reasoning 无帧。
func TestResponsesTranslatorText(t *testing.T) {
	fixture := loadResponsesFixture(t, "responses_text.sse")
	frames := translateFixture(fixture, "oc/muse")
	tr := terminalResponse(t, fixture)

	if len(frames) != 4 {
		t.Fatalf("帧数 = %d，期望 4（首帧 + 文本增量 + 终帧 + usage 帧；reasoning 不产生帧）", len(frames))
	}
	if got := frames[0].Get("choices.0.delta.role").String(); got != "assistant" {
		t.Fatalf("首帧 delta.role = %q，期望 assistant", got)
	}

	// content 增量拼接 == fixture 里 message item 的 output_text 全文。
	wantText := ""
	for _, item := range tr.Get("output").Array() {
		if item.Get("type").String() == "message" {
			wantText = item.Get("content.0.text").String()
		}
	}
	if got := joinDeltaContent(frames); got != wantText {
		t.Fatalf("content 拼接 = %q，期望 %q", got, wantText)
	}
	if wantText == "" {
		t.Fatal("fixture 里未找到 output_text 全文")
	}

	if got := lastFinishReason(frames); got != "stop" {
		t.Fatalf("finish_reason = %q，期望 stop", got)
	}
	// 终帧 usage。
	u := tr.Get("usage")
	last := frames[len(frames)-1]
	if len(last.Get("choices").Array()) != 0 {
		t.Fatalf("usage-only 帧 choices 应为空，实际 = %s", last.Raw)
	}
	if got := last.Get("usage.prompt_tokens").Int(); got != u.Get("input_tokens").Int() {
		t.Fatalf("prompt_tokens = %d，期望 %d", got, u.Get("input_tokens").Int())
	}
	if got := last.Get("usage.completion_tokens").Int(); got != u.Get("output_tokens").Int() {
		t.Fatalf("completion_tokens = %d，期望 %d", got, u.Get("output_tokens").Int())
	}
	if got := last.Get("usage.total_tokens").Int(); got != u.Get("total_tokens").Int() {
		t.Fatalf("total_tokens = %d，期望 %d", got, u.Get("total_tokens").Int())
	}
	if got := last.Get("usage.completion_tokens_details.reasoning_tokens").Int(); got != u.Get("output_tokens_details.reasoning_tokens").Int() {
		t.Fatalf("reasoning_tokens = %d，期望 %d", got, u.Get("output_tokens_details.reasoning_tokens").Int())
	}
}

// TestResponsesTranslatorToolcall 覆盖 §8.2 工具 fixture：tool_calls 增量、arguments 拼接、finish_reason。
func TestResponsesTranslatorToolcall(t *testing.T) {
	fixture := loadResponsesFixture(t, "responses_toolcall.sse")
	frames := translateFixture(fixture, "oc/muse")
	tr := terminalResponse(t, fixture)

	if got := frames[0].Get("choices.0.delta.role").String(); got != "assistant" {
		t.Fatalf("首帧 delta.role = %q", got)
	}
	if got := lastFinishReason(frames); got != "tool_calls" {
		t.Fatalf("finish_reason = %q，期望 tool_calls", got)
	}

	foundID, foundName := "", ""
	for _, f := range frames {
		for _, tc := range f.Get("choices.0.delta.tool_calls").Array() {
			if id := tc.Get("id").String(); id != "" {
				foundID = id
			}
			if n := tc.Get("function.name").String(); n != "" {
				foundName = n
			}
		}
	}
	if foundID != "call_01a12fb4a7497284ac15d6f01c874c58" {
		t.Fatalf("tool_call id = %q", foundID)
	}
	if foundName != "get_weather" {
		t.Fatalf("tool_call name = %q", foundName)
	}
	if got := joinToolArguments(frames, 0); got != `{"city":"Beijing"}` {
		t.Fatalf("arguments 拼接 = %q", got)
	}
	u := tr.Get("usage")
	if got := frames[len(frames)-1].Get("usage.prompt_tokens_details.cached_tokens").Int(); got != u.Get("input_tokens_details.cached_tokens").Int() {
		t.Fatalf("cached_tokens = %d，期望 %d", got, u.Get("input_tokens_details.cached_tokens").Int())
	}
}

// TestResponsesTranslatorIncomplete 覆盖 §8.2 incomplete fixture：finish_reason=length。
func TestResponsesTranslatorIncomplete(t *testing.T) {
	fixture := loadResponsesFixture(t, "responses_incomplete.sse")
	frames := translateFixture(fixture, "oc/muse")

	if got := lastFinishReason(frames); got != "length" {
		t.Fatalf("finish_reason = %q，期望 length", got)
	}
	if len(frames) != 3 {
		t.Fatalf("帧数 = %d，期望 3（首帧 + 终帧 + usage 帧）", len(frames))
	}
}

// TestResponsesTranslatorFinalizeIdempotent finalize 重复调用不重复产出（§4.2）。
func TestResponsesTranslatorFinalizeIdempotent(t *testing.T) {
	tr := newResponsesTranslator("oc/muse")
	if got := tr.finalize(); len(got) == 0 {
		t.Fatal("首次 finalize 应产出帧")
	}
	if got := tr.finalize(); len(got) != 0 {
		t.Fatalf("重复 finalize 不该产出帧，实际 %d 条", len(got))
	}
}

// TestAggregateResponsesSSE 覆盖 §8.3：三个 fixture 各聚合一次，断言 object/model/content/tool_calls/usage/finish_reason。
func TestAggregateResponsesSSE(t *testing.T) {
	const client = "oc/muse"

	t.Run("text", func(t *testing.T) {
		fixture := loadResponsesFixture(t, "responses_text.sse")
		agg := aggregateResponsesSSE(fixture, nil, client)
		if got := gjson.GetBytes(agg, "object").String(); got != "chat.completion" {
			t.Fatalf("object = %q", got)
		}
		if got := gjson.GetBytes(agg, "model").String(); got != client {
			t.Fatalf("model = %q", got)
		}
		if got := gjson.GetBytes(agg, "choices.0.message.content").String(); got != "OK" {
			t.Fatalf("content = %q", got)
		}
		if got := gjson.GetBytes(agg, "choices.0.finish_reason").String(); got != "stop" {
			t.Fatalf("finish_reason = %q", got)
		}
		if got := gjson.GetBytes(agg, "usage.prompt_tokens").Int(); got != 615 {
			t.Fatalf("usage.prompt_tokens = %d", got)
		}
		if got := gjson.GetBytes(agg, "usage.completion_tokens").Int(); got != 29 {
			t.Fatalf("usage.completion_tokens = %d", got)
		}
		if got := gjson.GetBytes(agg, "usage.total_tokens").Int(); got != 644 {
			t.Fatalf("usage.total_tokens = %d", got)
		}
		if got := gjson.GetBytes(agg, "usage.completion_tokens_details.reasoning_tokens").Int(); got != 18 {
			t.Fatalf("usage.reasoning_tokens = %d", got)
		}
	})

	t.Run("toolcall", func(t *testing.T) {
		fixture := loadResponsesFixture(t, "responses_toolcall.sse")
		agg := aggregateResponsesSSE(fixture, nil, client)
		if got := gjson.GetBytes(agg, "choices.0.finish_reason").String(); got != "tool_calls" {
			t.Fatalf("finish_reason = %q", got)
		}
		if got := gjson.GetBytes(agg, "choices.0.message.tool_calls.0.id").String(); got != "call_01a12fb4a7497284ac15d6f01c874c58" {
			t.Fatalf("tool_call id = %q", got)
		}
		if got := gjson.GetBytes(agg, "choices.0.message.tool_calls.0.function.name").String(); got != "get_weather" {
			t.Fatalf("tool name = %q", got)
		}
		if got := gjson.GetBytes(agg, "choices.0.message.tool_calls.0.function.arguments").String(); got != `{"city":"Beijing"}` {
			t.Fatalf("arguments = %q", got)
		}
		if got := gjson.GetBytes(agg, "usage.prompt_tokens_details.cached_tokens").Int(); got != 113 {
			t.Fatalf("cached_tokens = %d", got)
		}
		if got := gjson.GetBytes(agg, "usage.completion_tokens_details.reasoning_tokens").Int(); got != 132 {
			t.Fatalf("reasoning_tokens = %d", got)
		}
	})

	t.Run("incomplete", func(t *testing.T) {
		fixture := loadResponsesFixture(t, "responses_incomplete.sse")
		agg := aggregateResponsesSSE(fixture, nil, client)
		if got := gjson.GetBytes(agg, "choices.0.finish_reason").String(); got != "length" {
			t.Fatalf("finish_reason = %q", got)
		}
		if got := gjson.GetBytes(agg, "usage.total_tokens").Int(); got != 679 {
			t.Fatalf("usage.total_tokens = %d", got)
		}
	})
}

// TestRelayResponsesSSE 覆盖 §8.4：每行 data: 前缀、末行 [DONE]、工具名按 renamed 还原。
func TestRelayResponsesSSE(t *testing.T) {
	input := "" +
		"event: response.created\n" +
		`data: {"type":"response.created","response":{"id":"resp_x","created_at":1700000000}}` + "\n\n" +
		"event: response.output_item.added\n" +
		`data: {"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","call_id":"call_1","name":"bash"}}` + "\n\n" +
		"event: response.function_call_arguments.delta\n" +
		`data: {"type":"response.function_call_arguments.delta","output_index":0,"delta":"{}"}` + "\n\n" +
		"event: response.completed\n" +
		`data: {"type":"response.completed","response":{"id":"resp_x","created_at":1700000000,"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}` + "\n\n"

	var buf strings.Builder
	flush := func() {}
	if err := relayResponsesSSE(noTimeoutCtx(), strings.NewReader(input), &buf, flush, map[string]string{"bash": "Bash"}, "oc/muse", true, testLogger()); err != nil {
		t.Fatalf("relayResponsesSSE 返回错误: %v", err)
	}

	out := buf.String()
	if strings.Contains(out, "data: data:") {
		t.Fatalf("输出出现双前缀 data: data:\n%s", out)
	}
	frames := srvSSEFrames(out)
	if len(frames) == 0 {
		t.Fatalf("无输出")
	}
	for i, f := range frames {
		if !strings.HasPrefix(f, "data: ") {
			t.Fatalf("第 %d 帧不是 data: 前缀: %q", i, f)
		}
	}
	if frames[len(frames)-1] != "data: [DONE]" {
		t.Fatalf("末帧 = %q，期望 data: [DONE]", frames[len(frames)-1])
	}

	restored := false
	for _, f := range frames {
		payload := strings.TrimPrefix(f, "data: ")
		if payload == "[DONE]" {
			continue
		}
		if got := gjson.Get(payload, "choices.0.delta.tool_calls.0.function.name").String(); got == "Bash" {
			restored = true
		}
		if got := gjson.Get(payload, "model").String(); got != "oc/muse" {
			t.Fatalf("model = %q，期望 oc/muse", got)
		}
	}
	if !restored {
		t.Fatalf("工具名未按 renamed 还原成 Bash:\n%s", out)
	}
}

// TestBuildResponsesUpstreamRequest 覆盖 §8.5：路径 /responses、指纹头、强制 stream、四件套、无 tool_choice。
func TestBuildResponsesUpstreamRequest(t *testing.T) {
	cfg := testUpstreamConfig(t, "http://upstream.invalid", nonStreamForce)

	clientBody := `{"model":"oc/muse","stream":false,"messages":[{"role":"user","content":"hi"}]}`
	clientReq := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(clientBody))
	clientReq.Header.Set("Authorization", "Bearer sk-client-secret")
	clientReq.Header.Set("X-Request-Id", "req-resp-1")

	upReq, renamed, body, err := buildResponsesUpstreamRequest(cfg, nil, clientReq, "oc/muse", "muse-spark-1.3-contributor-free")
	if err != nil {
		t.Fatalf("buildResponsesUpstreamRequest 失败: %v", err)
	}
	if got := upReq.URL.Path; got != "/responses" {
		t.Fatalf("路径 = %q，期望 /responses", got)
	}
	if got := upReq.Header.Get("Content-Type"); got != "application/json" {
		t.Fatalf("Content-Type = %q", got)
	}
	if got := upReq.Header.Get("User-Agent"); got != cfg.Upstream.UserAgent {
		t.Fatalf("User-Agent = %q，期望 %q", got, cfg.Upstream.UserAgent)
	}
	if got := upReq.Header.Get("x-opencode-client"); got != "desktop" {
		t.Fatalf("x-opencode-client = %q", got)
	}
	sid := upReq.Header.Get("x-opencode-session")
	if !regexp.MustCompile(`^ses_[0-9a-f]{12}[0-9a-zA-Z]{14}$`).MatchString(sid) {
		t.Fatalf("x-opencode-session = %q 不符合形状", sid)
	}
	if got := upReq.Header.Get("Authorization"); got != "" {
		t.Fatalf("Authorization 不应透传，实际 = %q", got)
	}
	if got := upReq.Header.Get("Accept-Encoding"); got != "" {
		t.Fatalf("不应设置 Accept-Encoding，实际 = %q", got)
	}

	if !gjson.GetBytes(body, "stream").Bool() {
		t.Fatalf("body.stream 应强制为 true: %s", body)
	}
	if got := gjson.GetBytes(body, "model").String(); got != "muse-spark-1.3-contributor-free" {
		t.Fatalf("body.model = %q", got)
	}
	have := map[string]bool{}
	for _, tc := range gjson.GetBytes(body, "tools").Array() {
		have[tc.Get("name").String()] = true
	}
	for _, want := range quartetTools {
		if !have[want] {
			t.Fatalf("body 缺少四件套工具 %q，实际 = %v", want, have)
		}
	}
	if gjson.GetBytes(body, "tool_choice").Exists() {
		t.Fatalf("客户端没给工具时不该出现 tool_choice: %s", body)
	}
	if len(renamed) != 0 {
		t.Fatalf("客户端没给工具时 renamed 应为空，实际 = %v", renamed)
	}

	// 客户端给了大写 Bash → 上游小写 bash，且 renamed 记录反向映射。
	clientBody2 := `{"model":"oc/muse","stream":true,"messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"Bash","description":"","parameters":{"type":"object","properties":{}}}}]}`
	clientReq2 := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(clientBody2))
	_, renamed2, body2, err := buildResponsesUpstreamRequest(cfg, nil, clientReq2, "oc/muse", "muse-spark-1.3-contributor-free")
	if err != nil {
		t.Fatalf("buildResponsesUpstreamRequest(2) 失败: %v", err)
	}
	if got := gjson.GetBytes(body2, "tools.0.name").String(); got != "bash" {
		t.Fatalf("tools.0.name = %q，期望小写 bash", got)
	}
	if renamed2["bash"] != "Bash" {
		t.Fatalf("renamed = %v，期望 bash→Bash", renamed2)
	}
}

// TestServerResponsesEndToEnd 覆盖 §2/§6 接线：responses 模型在 chat.completions 接口下
// 走 /responses 上游并翻译；流式出 SSE、非流式聚合成 chat.completion。
func TestServerResponsesEndToEnd(t *testing.T) {
	fixture := loadResponsesFixture(t, "responses_text.sse")
	var (
		mu       sync.Mutex
		gotPath  string
		gotBody  []byte
		upstream = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			b, _ := io.ReadAll(r.Body)
			mu.Lock()
			gotPath = r.URL.Path
			gotBody = b
			mu.Unlock()
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write(fixture)
		}))
	)
	defer upstream.Close()

	raw := fmt.Sprintf(`
server:
  api_keys: []
upstream:
  base_url: %q
  models: ["muse-free"]
  responses_models: ["muse-free"]
  user_agent: "opencode/9.9.9"
  request_timeout: "5s"
  first_byte_timeout: "5s"
`, upstream.URL)
	cfg, err := decodeConfig([]byte(raw))
	if err != nil {
		t.Fatalf("decodeConfig 失败: %v", err)
	}
	pool, err := buildProxyPool(&cfg, testLogger())
	if err != nil {
		t.Fatalf("buildProxyPool 失败: %v", err)
	}
	rt := newRouter(cfg.Upstream.ModelPrefix, cfg.Upstream.ModelAliases)
	rt.setExposed([]string{"muse-free"})
	h := routes(&cfg, testLogger(), pool, rt, nil, nil, time.Now())

	// 流式。
	rec := srvDo(h, http.MethodPost, "/v1/chat/completions", "",
		`{"model":"oc/muse-free","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("流式状态 = %d，期望 200；body=%s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("Content-Type = %q", ct)
	}
	out := rec.Body.String()
	frames := srvSSEFrames(out)
	if len(frames) == 0 || frames[len(frames)-1] != "data: [DONE]" {
		t.Fatalf("末帧应为 data: [DONE]，实际:\n%s", out)
	}
	foundRole, foundContent := false, false
	for _, f := range frames {
		p := strings.TrimPrefix(f, "data: ")
		if p == "[DONE]" {
			continue
		}
		if gjson.Get(p, "choices.0.delta.role").String() == "assistant" {
			foundRole = true
		}
		if gjson.Get(p, "choices.0.delta.content").String() == "OK" {
			foundContent = true
		}
	}
	if !foundRole || !foundContent {
		t.Fatalf("流式翻译缺首帧/内容: role=%v content=%v\n%s", foundRole, foundContent, out)
	}
	mu.Lock()
	if gotPath != "/responses" {
		t.Fatalf("上游路径 = %q，期望 /responses", gotPath)
	}
	if !gjson.GetBytes(gotBody, "stream").Bool() {
		t.Fatalf("上游 body 应强制 stream:true: %s", gotBody)
	}
	mu.Unlock()

	// 非流式：聚合成 chat.completion。
	rec = srvDo(h, http.MethodPost, "/v1/chat/completions", "",
		`{"model":"oc/muse-free","stream":false,"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("非流式状态 = %d，期望 200；body=%s", rec.Code, rec.Body.String())
	}
	if got := gjson.GetBytes(rec.Body.Bytes(), "object").String(); got != "chat.completion" {
		t.Fatalf("非流式 object = %q，期望 chat.completion；body=%s", got, rec.Body.String())
	}
	if got := gjson.GetBytes(rec.Body.Bytes(), "model").String(); got != "oc/muse-free" {
		t.Fatalf("非流式 model = %q，期望 oc/muse-free", got)
	}
	if got := gjson.GetBytes(rec.Body.Bytes(), "choices.0.message.content").String(); got != "OK" {
		t.Fatalf("非流式 content = %q，期望 OK", got)
	}
	mu.Lock()
	if gotPath != "/responses" {
		t.Fatalf("非流式上游路径 = %q，期望 /responses", gotPath)
	}
	mu.Unlock()
}
