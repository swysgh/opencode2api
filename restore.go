package main

import (
	"bytes"
	"fmt"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// restoreToolNames 把响应里被我们改成小写的工具名还原成客户端原本的拼写。
// 支持三种协议的整包 JSON 与 SSE 分片：
//   - OpenAI chat：choices[].delta|message.tool_calls[].function.name
//   - Claude：content[]（type=tool_use）.name 与 SSE content_block_start.content_block.name
//   - OpenAI Responses：output[]（type=function_call）.name 与 SSE item.name
func restoreToolNames(payload []byte, renamed map[string]string) []byte {
	if len(renamed) == 0 || len(payload) == 0 {
		return payload
	}
	if looksLikeSSE(payload) {
		return restoreSSE(payload, renamed)
	}
	if !gjson.ValidBytes(payload) {
		return payload
	}
	out, changed := restoreJSON(payload, renamed)
	if !changed {
		return payload
	}
	return out
}

// looksLikeSSE 判断分片是不是带 data: 前缀的 SSE 原始字节。
func looksLikeSSE(payload []byte) bool {
	trimmed := bytes.TrimLeft(payload, " \t")
	return bytes.HasPrefix(trimmed, []byte("data:")) || bytes.HasPrefix(trimmed, []byte("event:"))
}

// restoreSSE 逐行处理 SSE 分片，只替换 data: 行的 JSON 部分，其余字节原样保留。
func restoreSSE(payload []byte, renamed map[string]string) []byte {
	var buf bytes.Buffer
	for len(payload) > 0 {
		idx := bytes.IndexByte(payload, '\n')
		var line, rest []byte
		if idx < 0 {
			line, rest = payload, nil
		} else {
			line, rest = payload[:idx], payload[idx+1:]
		}

		body := bytes.TrimRight(line, "\r")
		suffix := line[len(body):]

		trimmed := bytes.TrimLeft(body, " \t")
		prefixLen := len(body) - len(trimmed)
		if !bytes.HasPrefix(trimmed, []byte("data:")) {
			buf.Write(line)
			if idx >= 0 {
				buf.WriteByte('\n')
			}
			payload = rest
			continue
		}

		raw := bytes.TrimSpace(trimmed[len("data:"):])
		if len(raw) == 0 || bytes.Equal(raw, []byte("[DONE]")) || !gjson.ValidBytes(raw) {
			buf.Write(line)
			if idx >= 0 {
				buf.WriteByte('\n')
			}
			payload = rest
			continue
		}

		updated, changed := restoreJSON(raw, renamed)
		if !changed {
			buf.Write(line)
			if idx >= 0 {
				buf.WriteByte('\n')
			}
			payload = rest
			continue
		}

		buf.Write(body[:prefixLen])
		buf.WriteString("data: ")
		buf.Write(updated)
		buf.Write(suffix)
		if idx >= 0 {
			buf.WriteByte('\n')
		}
		payload = rest
	}
	return buf.Bytes()
}

// restoreJSON 在单个 JSON 对象里还原工具名，返回新字节与是否改动。
func restoreJSON(obj []byte, renamed map[string]string) ([]byte, bool) {
	out := obj
	changed := false

	setName := func(path string) {
		v := gjson.GetBytes(out, path)
		if v.Type != gjson.String {
			return
		}
		orig, ok := renamed[v.String()]
		if !ok {
			return
		}
		updated, err := sjson.SetBytes(out, path, orig)
		if err != nil {
			return
		}
		out = updated
		changed = true
	}

	// OpenAI chat：流式 delta 与非流式 message 两种持有者。
	for i := range gjson.GetBytes(out, "choices").Array() {
		for _, holder := range []string{"delta", "message"} {
			calls := gjson.GetBytes(out, fmt.Sprintf("choices.%d.%s.tool_calls", i, holder)).Array()
			for j := range calls {
				setName(fmt.Sprintf("choices.%d.%s.tool_calls.%d.function.name", i, holder, j))
			}
		}
	}

	// Claude：整包 content[] 与流式 content_block_start。
	for i, block := range gjson.GetBytes(out, "content").Array() {
		if block.Get("type").String() == "tool_use" {
			setName(fmt.Sprintf("content.%d.name", i))
		}
	}
	if cb := gjson.GetBytes(out, "content_block"); cb.IsObject() && cb.Get("type").String() == "tool_use" {
		setName("content_block.name")
	}

	// OpenAI Responses：整包 output[] 与流式 item。
	for i, item := range gjson.GetBytes(out, "output").Array() {
		if item.Get("type").String() == "function_call" {
			setName(fmt.Sprintf("output.%d.name", i))
		}
	}
	if item := gjson.GetBytes(out, "item"); item.IsObject() && item.Get("type").String() == "function_call" {
		setName("item.name")
	}

	return out, changed
}

// canonicalToolName 返回小写规范名（供日志使用）。
func canonicalToolName(name string) string { return strings.ToLower(strings.TrimSpace(name)) }
