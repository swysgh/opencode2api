package main

import (
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

func TestRestoreChatNonStreaming(t *testing.T) {
	body := []byte(`{"id":"1","choices":[{"index":0,"message":{"role":"assistant","tool_calls":[{"id":"c1","type":"function","function":{"name":"bash","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`)
	out := restoreToolNames(body, map[string]string{"bash": "Bash"})
	if got := gjson.GetBytes(out, "choices.0.message.tool_calls.0.function.name").String(); got != "Bash" {
		t.Fatalf("工具名 = %q, 期望 Bash", got)
	}
	if !strings.Contains(string(out), `"arguments":"{}"`) {
		t.Fatalf("其它字段被破坏: %s", out)
	}
}

func TestRestoreChatStreamChunkKeepsFraming(t *testing.T) {
	chunk := []byte("data: {\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"name\":\"glob\",\"arguments\":\"\"}}]}}]}\n\n")
	out := restoreToolNames(chunk, map[string]string{"glob": "Glob"})
	if !strings.HasPrefix(string(out), "data: ") {
		t.Fatalf("SSE 前缀丢失: %q", out)
	}
	if !strings.HasSuffix(string(out), "\n\n") {
		t.Fatalf("SSE 结尾丢失: %q", out)
	}
	if got := gjson.Get(strings.TrimSpace(strings.TrimPrefix(string(out), "data: ")), "choices.0.delta.tool_calls.0.function.name").String(); got != "Glob" {
		t.Fatalf("工具名 = %q, 期望 Glob", got)
	}
}

func TestRestoreStreamKeepsDoneAndOtherLines(t *testing.T) {
	chunk := []byte("event: ping\ndata: [DONE]\n\ndata: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n")
	out := restoreToolNames(chunk, map[string]string{"bash": "Bash"})
	if string(out) != string(chunk) {
		t.Fatalf("无工具名可还原时不应改动分片:\n%q\n%q", chunk, out)
	}
}

func TestRestoreClaudeShapes(t *testing.T) {
	nonStream := []byte(`{"type":"message","content":[{"type":"text","text":"x"},{"type":"tool_use","id":"t1","name":"read","input":{}}]}`)
	out := restoreToolNames(nonStream, map[string]string{"read": "Read"})
	if got := gjson.GetBytes(out, "content.1.name").String(); got != "Read" {
		t.Fatalf("claude content 工具名 = %q, 期望 Read", got)
	}

	stream := []byte("data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":\"t1\",\"name\":\"grep\",\"input\":{}}}\n\n")
	out2 := restoreToolNames(stream, map[string]string{"grep": "Grep"})
	if got := gjson.Get(strings.TrimSpace(strings.TrimPrefix(string(out2), "data: ")), "content_block.name").String(); got != "Grep" {
		t.Fatalf("claude 流式工具名 = %q, 期望 Grep", got)
	}
}

func TestRestoreResponsesShapes(t *testing.T) {
	nonStream := []byte(`{"type":"response","output":[{"type":"function_call","name":"bash","call_id":"c","arguments":"{}"}]}`)
	out := restoreToolNames(nonStream, map[string]string{"bash": "Bash"})
	if got := gjson.GetBytes(out, "output.0.name").String(); got != "Bash" {
		t.Fatalf("responses output 工具名 = %q, 期望 Bash", got)
	}

	stream := []byte("data: {\"type\":\"response.output_item.added\",\"item\":{\"type\":\"function_call\",\"name\":\"glob\"}}\n\n")
	out2 := restoreToolNames(stream, map[string]string{"glob": "Glob"})
	if got := gjson.Get(strings.TrimSpace(strings.TrimPrefix(string(out2), "data: ")), "item.name").String(); got != "Glob" {
		t.Fatalf("responses 流式工具名 = %q, 期望 Glob", got)
	}
}

func TestRestoreNoopCases(t *testing.T) {
	body := []byte(`{"choices":[{"message":{"tool_calls":[{"function":{"name":"other"}}]}}]}`)
	if out := restoreToolNames(body, map[string]string{"bash": "Bash"}); string(out) != string(body) {
		t.Fatalf("无关工具名不应改动: %s", out)
	}
	if out := restoreToolNames(body, nil); string(out) != string(body) {
		t.Fatal("空映射应原样返回")
	}
	if out := restoreToolNames([]byte("plain text"), map[string]string{"bash": "Bash"}); string(out) != "plain text" {
		t.Fatalf("非 JSON 应原样返回: %q", out)
	}
}
