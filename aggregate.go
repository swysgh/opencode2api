package main

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/tidwall/gjson"
)

func rewriteModel(body []byte, model string) []byte {
	if len(body) == 0 || model == "" {
		return body
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(body, &top); err != nil {
		return body
	}
	raw, ok := top["model"]
	if !ok {
		return body
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil || s == "" {
		return body
	}
	top["model"] = json.RawMessage(`"` + strings.ReplaceAll(model, `"`, `\"`) + `"`)
	out, err := json.Marshal(top)
	if err != nil {
		return body
	}
	return out
}

// parseUpstreamError 从上游错误 JSON 提取 code / message。
func parseUpstreamError(body []byte) (code, message string) {
	var env struct {
		Error struct {
			Type    string `json:"type"`
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return "upstream_error", ""
	}
	code = env.Error.Code
	if code == "" {
		code = env.Error.Type
	}
	if code == "" {
		code = "upstream_error"
	}
	return code, env.Error.Message
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

func proxyFailureSummary(attempts []attemptResult) string {
	var parts []string
	for _, a := range attempts {
		if a.err != nil {
			parts = append(parts, fmt.Sprintf("%s 失败(%v)", a.display, a.err))
		} else {
			parts = append(parts, fmt.Sprintf("%s 状态=%d", a.display, a.status))
		}
	}
	return "所有出口代理均失败: " + strings.Join(parts, "; ")
}

func aggregateSSE(sse []byte, renamed map[string]string, clientModel string) []byte {
	var (
		content        strings.Builder
		reasoning      strings.Builder
		id             string
		created        int64
		finishReason   string
		usageRaw       json.RawMessage
		toolCallsByIdx = map[int]*toolCallAcc{}
	)
	created = time.Now().Unix()

	lines := strings.Split(string(sse), "\n")
	for _, line := range lines {
		line = strings.TrimRight(line, "\r")
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(trimmed, "data:"))
		if payload == "" || payload == "[DONE]" {
			continue
		}
		if !gjson.Valid(payload) {
			continue
		}
		// 工具名还原（脱敏反向）。
		if renamed != nil && len(renamed) > 0 {
			restored, changed := restoreJSON([]byte(payload), renamed)
			if changed {
				payload = string(restored)
			}
		}
		root := gjson.Parse(payload)
		if v := root.Get("id"); v.Exists() && v.String() != "" {
			id = v.String()
		}
		if v := root.Get("created"); v.Exists() {
			if c := v.Int(); c > 0 {
				created = c
			}
		}
		// choices[0].delta
		for _, ch := range root.Get("choices").Array() {
			delta := ch.Get("delta")
			if c := delta.Get("content").String(); c != "" {
				content.WriteString(c)
			}
			// 上游有两种形状：`reasoning_content`（OpenAI 系）与 `reasoning`
			// + `reasoning_details`（OpenRouter 系，实测 zen 的 mimo 系列用这种）。
			// 只认前者会让聚合响应丢掉思维链（实测：mimo 花掉 34 个 reasoning token，
			// 非流式响应里一个字段都没有）。
			if r := delta.Get("reasoning_content").String(); r != "" {
				reasoning.WriteString(r)
			} else if r := delta.Get("reasoning").String(); r != "" {
				reasoning.WriteString(r)
			}
			if fr := ch.Get("finish_reason").String(); fr != "" {
				finishReason = fr
			}
			for _, tc := range delta.Get("tool_calls").Array() {
				idx := int(tc.Get("index").Int())
				acc := toolCallsByIdx[idx]
				if acc == nil {
					acc = &toolCallAcc{}
					toolCallsByIdx[idx] = acc
				}
				if idv := tc.Get("id").String(); idv != "" {
					acc.id = idv
				}
				if nv := tc.Get("function.name").String(); nv != "" {
					acc.name = nv
				}
				acc.args += tc.Get("function.arguments").String()
			}
		}
		if u := root.Get("usage"); u.Exists() && u.Type != gjson.Null {
			usageRaw = json.RawMessage(u.Raw)
		}
	}

	// 构造 tool_calls 数组（按 index 排序）。
	idxs := make([]int, 0, len(toolCallsByIdx))
	for i := range toolCallsByIdx {
		idxs = append(idxs, i)
	}
	sort.Ints(idxs)
	var toolCalls []map[string]any
	for _, i := range idxs {
		acc := toolCallsByIdx[i]
		tc := map[string]any{
			"id":   orDefault(acc.id, fmt.Sprintf("call_%d", i)),
			"type": "function",
			"function": map[string]any{
				"name":      orDefault(acc.name, "function"),
				"arguments": acc.args,
			},
		}
		toolCalls = append(toolCalls, tc)
	}

	message := map[string]any{
		"role":    "assistant",
		"content": content.String(),
	}
	if reasoning.Len() > 0 {
		message["reasoning_content"] = reasoning.String()
	}
	if len(toolCalls) > 0 {
		message["tool_calls"] = toolCalls
	}

	out := map[string]any{
		"id":      orDefault(id, "chatcmpl-cpa-zen"),
		"object":  "chat.completion",
		"created": created,
		"model":   clientModel,
		"choices": []map[string]any{
			{
				"index":         0,
				"message":       message,
				"finish_reason": orDefault(finishReason, "stop"),
			},
		},
	}
	if usageRaw != nil {
		out["usage"] = usageRaw
	}
	raw, _ := json.Marshal(out)
	return raw
}

type toolCallAcc struct {
	id   string
	name string
	args string
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

func rewritePayloadModel(payload []byte, clientModel string) []byte {
	if clientModel == "" || !gjson.ValidBytes(payload) {
		return payload
	}
	if !gjson.GetBytes(payload, "model").Exists() {
		return payload
	}
	out, err := sjsonSet(payload, "model", clientModel)
	if err != nil {
		return payload
	}
	return out
}
