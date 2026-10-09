package main

import (
	"encoding/json"
	"testing"

	"github.com/tidwall/gjson"
)

func mustJSON(t *testing.T, s string) []byte {
	t.Helper()
	if !gjson.Valid(s) {
		t.Fatalf("测试数据不是合法 JSON: %s", s)
	}
	return []byte(s)
}

func toolNames(t *testing.T, body []byte) []string {
	t.Helper()
	var out []string
	for _, item := range gjson.GetBytes(body, "tools").Array() {
		name := item.Get("function.name").String()
		if name == "" {
			name = item.Get("name").String()
		}
		out = append(out, name)
	}
	return out
}

func TestApplyFingerprintRenamesCapitalizedQuartet(t *testing.T) {
	body := mustJSON(t, `{
		"model":"zen-mimo-v2.5",
		"stream":true,
		"messages":[{"role":"user","content":"hi"}],
		"tools":[
			{"type":"function","function":{"name":"Bash","description":"run","parameters":{"type":"object","properties":{}}}},
			{"type":"function","function":{"name":"Read","description":"read","parameters":{"type":"object","properties":{}}}},
			{"type":"function","function":{"name":"Glob","description":"glob","parameters":{"type":"object","properties":{}}}},
			{"type":"function","function":{"name":"Grep","description":"grep","parameters":{"type":"object","properties":{}}}}
		],
		"tool_choice":{"type":"function","function":{"name":"Bash"}}
	}`)

	res, err := applyFingerprint(body, "openai", fingerprintOptions{InjectTools: true, ForceStream: true, ClientStreams: true})
	if err != nil {
		t.Fatalf("applyFingerprint: %v", err)
	}
	if !res.Changed {
		t.Fatal("期望 body 被改写")
	}
	names := toolNames(t, res.Body)
	want := []string{"bash", "read", "glob", "grep"}
	if len(names) != len(want) {
		t.Fatalf("工具数 = %v, 期望 %v", names, want)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("工具名 = %v, 期望 %v", names, want)
		}
	}
	if len(res.Injected) != 0 {
		t.Fatalf("不应注入新工具，实际 %v", res.Injected)
	}
	if res.Renamed["bash"] != "Bash" || res.Renamed["read"] != "Read" {
		t.Fatalf("改名映射 = %v", res.Renamed)
	}
	if got := gjson.GetBytes(res.Body, "tool_choice.function.name").String(); got != "bash" {
		t.Fatalf("tool_choice = %q, 期望 bash", got)
	}
}

func TestApplyFingerprintInjectsQuartetAndDisablesToolChoice(t *testing.T) {
	body := mustJSON(t, `{"model":"zen-space-bunny","stream":true,"messages":[{"role":"user","content":"hi"}]}`)

	res, err := applyFingerprint(body, "openai", fingerprintOptions{InjectTools: true})
	if err != nil {
		t.Fatalf("applyFingerprint: %v", err)
	}
	names := toolNames(t, res.Body)
	if len(names) != 4 {
		t.Fatalf("工具数 = %v, 期望四个", names)
	}
	for _, q := range quartetTools {
		if !contains(names, q) {
			t.Fatalf("缺少 %s: %v", q, names)
		}
	}
	if got := gjson.GetBytes(res.Body, "tool_choice").String(); got != "none" {
		t.Fatalf("tool_choice = %q, 期望 none", got)
	}
	if len(res.Injected) != 4 {
		t.Fatalf("注入列表 = %v", res.Injected)
	}
}

func TestApplyFingerprintFillsMissingMember(t *testing.T) {
	body := mustJSON(t, `{"model":"m","stream":true,"messages":[],"tools":[
		{"type":"function","function":{"name":"bash","description":"d","parameters":{"type":"object","properties":{}}}},
		{"type":"function","function":{"name":"glob","description":"d","parameters":{"type":"object","properties":{}}}},
		{"type":"function","function":{"name":"grep","description":"d","parameters":{"type":"object","properties":{}}}}
	]}`)

	res, err := applyFingerprint(body, "openai", fingerprintOptions{InjectTools: true})
	if err != nil {
		t.Fatalf("applyFingerprint: %v", err)
	}
	if len(res.Injected) != 1 || res.Injected[0] != "read" {
		t.Fatalf("注入列表 = %v, 期望 [read]", res.Injected)
	}
	if len(res.Renamed) != 0 {
		t.Fatalf("不应有改名，实际 %v", res.Renamed)
	}
}

func TestApplyFingerprintDropsDuplicateQuartet(t *testing.T) {
	body := mustJSON(t, `{"model":"m","stream":true,"messages":[],"tools":[
		{"type":"function","function":{"name":"Bash","description":"d","parameters":{"type":"object","properties":{}}}},
		{"type":"function","function":{"name":"bash","description":"d","parameters":{"type":"object","properties":{}}}},
		{"type":"function","function":{"name":"glob","description":"d","parameters":{"type":"object","properties":{}}}},
		{"type":"function","function":{"name":"grep","description":"d","parameters":{"type":"object","properties":{}}}},
		{"type":"function","function":{"name":"read","description":"d","parameters":{"type":"object","properties":{}}}}
	]}`)

	res, err := applyFingerprint(body, "openai", fingerprintOptions{InjectTools: true})
	if err != nil {
		t.Fatalf("applyFingerprint: %v", err)
	}
	names := toolNames(t, res.Body)
	if len(names) != 4 {
		t.Fatalf("工具名 = %v, 期望去重后四个", names)
	}
}

func TestApplyFingerprintClaudeShape(t *testing.T) {
	body := mustJSON(t, `{"model":"zen-x","stream":true,"system":"s","messages":[{"role":"user","content":"hi"}]}`)

	res, err := applyFingerprint(body, "claude", fingerprintOptions{InjectTools: true})
	if err != nil {
		t.Fatalf("applyFingerprint: %v", err)
	}
	tools := gjson.GetBytes(res.Body, "tools").Array()
	if len(tools) != 4 {
		t.Fatalf("工具数 = %d, 期望 4", len(tools))
	}
	for _, tool := range tools {
		if !tool.Get("input_schema").Exists() {
			t.Fatalf("claude 形状应带 input_schema: %s", tool.Raw)
		}
		if tool.Get("function").Exists() {
			t.Fatalf("claude 形状不应有 function 包裹: %s", tool.Raw)
		}
	}
	// claude 形状不设置 tool_choice（避免塞入上游不认的值）。
	if gjson.GetBytes(res.Body, "tool_choice").Exists() {
		t.Fatalf("claude 形状不应改 tool_choice: %s", gjson.GetBytes(res.Body, "tool_choice").Raw)
	}
}

func TestApplyFingerprintResponsesShape(t *testing.T) {
	body := mustJSON(t, `{"model":"zen-x","stream":true,"input":[{"type":"message","role":"user","content":"hi"}]}`)

	res, err := applyFingerprint(body, "openai-responses", fingerprintOptions{InjectTools: true})
	if err != nil {
		t.Fatalf("applyFingerprint: %v", err)
	}
	tools := gjson.GetBytes(res.Body, "tools").Array()
	if len(tools) != 4 {
		t.Fatalf("工具数 = %d, 期望 4", len(tools))
	}
	if got := tools[0].Get("name").String(); got == "" {
		t.Fatalf("responses 形状工具名应在顶层: %s", tools[0].Raw)
	}
}

func TestApplyFingerprintForcesStreamOnlyWhenClientStreams(t *testing.T) {
	body := mustJSON(t, `{"model":"zen-x","messages":[],"stream":false}`)

	res, err := applyFingerprint(body, "openai", fingerprintOptions{InjectTools: false, ForceStream: true, ClientStreams: false})
	if err != nil {
		t.Fatalf("applyFingerprint: %v", err)
	}
	if res.ForcedStream || gjson.GetBytes(res.Body, "stream").Bool() {
		t.Fatal("非流式客户端不应被强制 stream")
	}

	res2, err := applyFingerprint(body, "openai", fingerprintOptions{InjectTools: false, ForceStream: true, ClientStreams: true})
	if err != nil {
		t.Fatalf("applyFingerprint: %v", err)
	}
	if !res2.ForcedStream || !gjson.GetBytes(res2.Body, "stream").Bool() {
		t.Fatal("流式客户端应被强制 stream:true")
	}
}

func TestApplyFingerprintPassesNonJSONThrough(t *testing.T) {
	body := []byte("not json at all")
	res, err := applyFingerprint(body, "openai", fingerprintOptions{InjectTools: true, ForceStream: true, ClientStreams: true})
	if err != nil {
		t.Fatalf("applyFingerprint: %v", err)
	}
	if res.Changed {
		t.Fatal("非 JSON 体不应被改写")
	}
	if string(res.Body) != "not json at all" {
		t.Fatalf("体被改动: %q", res.Body)
	}
}

func TestApplyFingerprintKeepsOtherToolsUntouched(t *testing.T) {
	body := mustJSON(t, `{"model":"m","stream":true,"messages":[],"tools":[
		{"type":"function","function":{"name":"MyTool","description":"x","parameters":{"type":"object","properties":{"a":{"type":"string"}}}}}
	]}`)

	res, err := applyFingerprint(body, "openai", fingerprintOptions{InjectTools: true})
	if err != nil {
		t.Fatalf("applyFingerprint: %v", err)
	}
	var parsed struct {
		Tools []map[string]any `json:"tools"`
	}
	if err := json.Unmarshal(res.Body, &parsed); err != nil {
		t.Fatalf("结果不是合法 JSON: %v", err)
	}
	found := false
	for _, tool := range parsed.Tools {
		fn, _ := tool["function"].(map[string]any)
		if fn == nil {
			continue
		}
		if fn["name"] == "MyTool" {
			found = true
			props, _ := fn["parameters"].(map[string]any)
			if props == nil || props["properties"] == nil {
				t.Fatalf("自定义工具的 schema 被破坏: %v", fn)
			}
		}
	}
	if !found {
		t.Fatalf("自定义工具丢失: %s", res.Body)
	}
}

func contains(list []string, v string) bool {
	for _, item := range list {
		if item == v {
			return true
		}
	}
	return false
}
