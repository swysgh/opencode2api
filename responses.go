package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/tidwall/gjson"
)

// responsesKnownKeys 是 chat.completions → /responses 翻译时认识的顶层键；
// 不在其中的键一律丢弃并告警（上游对 unknown parameter 直接 400）。
var responsesKnownKeys = map[string]struct{}{
	"model": {}, "messages": {}, "tools": {}, "tool_choice": {},
	"temperature": {}, "top_p": {}, "presence_penalty": {}, "frequency_penalty": {},
	"user": {}, "store": {}, "metadata": {}, "reasoning": {}, "parallel_tool_calls": {},
	"truncation": {}, "service_tier": {},
	"max_tokens": {}, "max_completion_tokens": {}, "max_output_tokens": {},
	"stream": {}, "stream_options": {},
	"stop": {}, "seed": {}, "response_format": {}, "logprobs": {}, "top_logprobs": {},
	"n": {},
}

// responsesPassthroughKeys 是原样直通的顶层字段（§3.4）。
var responsesPassthroughKeys = []string{
	"temperature", "top_p", "presence_penalty", "frequency_penalty", "user",
	"store", "metadata", "reasoning", "parallel_tool_calls", "truncation", "service_tier",
}

// responsesDroppedWarnKeys 是明确要求「丢弃 + 告警」的顶层字段（§3.4）。
var responsesDroppedWarnKeys = []string{"stop", "seed", "response_format", "logprobs", "top_logprobs"}

// chatMessage 是翻译阶段读取的客户端消息形状。
type chatMessage struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content"`
	ToolCalls  []chatToolCall  `json:"tool_calls"`
	ToolCallID string          `json:"tool_call_id"`
}

type chatToolCall struct {
	ID       string `json:"id"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// chatToResponsesBody 把客户端的 chat.completions 请求体翻译成上游 /responses 的请求体。
// 返回翻译后的 body、以及所有"被丢弃/被改写"的中文告警（供日志用，可为空）。
func chatToResponsesBody(body []byte, upstreamModel string) (out []byte, warnings []string, err error) {
	var top map[string]json.RawMessage
	if uerr := json.Unmarshal(body, &top); uerr != nil {
		return nil, nil, fmt.Errorf("请求体不是合法 JSON: %w", uerr)
	}

	var input []map[string]any
	var instructions []string

	if raw, ok := top["messages"]; ok {
		var msgs []chatMessage
		if uerr := json.Unmarshal(raw, &msgs); uerr != nil {
			return nil, nil, fmt.Errorf("messages 解析失败: %w", uerr)
		}
		for _, m := range msgs {
			switch m.Role {
			case "system", "developer":
				if t := contentToText(m.Content); t != "" {
					instructions = append(instructions, t)
				}
			case "user":
				parts := userContentParts(m.Content, &warnings)
				input = append(input, map[string]any{
					"type":    "message",
					"role":    "user",
					"content": parts,
				})
			case "assistant":
				if t := contentToText(m.Content); t != "" {
					input = append(input, map[string]any{
						"type":    "message",
						"role":    "assistant",
						"content": []map[string]any{{"type": "output_text", "text": t}},
					})
				}
				for _, tc := range m.ToolCalls {
					input = append(input, map[string]any{
						"type":      "function_call",
						"call_id":   tc.ID,
						"name":      tc.Function.Name,
						"arguments": tc.Function.Arguments,
					})
				}
			case "tool":
				if m.ToolCallID == "" {
					warnings = append(warnings, "丢弃缺少 call_id 的 tool 消息")
					continue
				}
				input = append(input, map[string]any{
					"type":    "function_call_output",
					"call_id": m.ToolCallID,
					"output":  contentToText(m.Content),
				})
			default:
				warnings = append(warnings, "丢弃未知 role "+m.Role)
			}
		}
	}
	if len(input) == 0 {
		return nil, nil, fmt.Errorf("messages 翻译后为空")
	}

	outMap := map[string]any{
		"model":  upstreamModel,
		"stream": true,
		"input":  input,
	}
	if len(instructions) > 0 {
		outMap["instructions"] = strings.Join(instructions, "\n\n")
	}

	// tools：chat 形状 → responses 形状（§3.3）。
	if raw, ok := top["tools"]; ok {
		var items []json.RawMessage
		if uerr := json.Unmarshal(raw, &items); uerr == nil {
			tools := make([]map[string]any, 0, len(items))
			for _, it := range items {
				var entry struct {
					Function *struct {
						Name        string          `json:"name"`
						Description string          `json:"description"`
						Parameters  json.RawMessage `json:"parameters"`
					} `json:"function"`
				}
				if uerr := json.Unmarshal(it, &entry); uerr != nil || entry.Function == nil {
					warnings = append(warnings, "丢弃缺少 function 的工具声明")
					continue
				}
				tool := map[string]any{
					"type": "function",
					"name": entry.Function.Name,
				}
				if entry.Function.Description != "" {
					tool["description"] = entry.Function.Description
				}
				if len(entry.Function.Parameters) > 0 {
					tool["parameters"] = entry.Function.Parameters
				}
				tools = append(tools, tool)
			}
			if len(tools) > 0 {
				outMap["tools"] = tools
			}
		}
	}

	// 原样直通字段（§3.4）。
	for _, k := range responsesPassthroughKeys {
		if v, ok := top[k]; ok {
			outMap[k] = v
		}
	}

	// max_tokens / max_completion_tokens → max_output_tokens；客户端已给 max_output_tokens 以它为准。
	if v, ok := top["max_output_tokens"]; ok {
		outMap["max_output_tokens"] = v
	} else if v, ok := top["max_completion_tokens"]; ok {
		outMap["max_output_tokens"] = v
	} else if v, ok := top["max_tokens"]; ok {
		outMap["max_output_tokens"] = v
	}

	// tool_choice：上游只接受 "auto"（§3.4）。
	if raw, ok := top["tool_choice"]; ok {
		var s string
		if json.Unmarshal(raw, &s) == nil && s == "auto" {
			outMap["tool_choice"] = "auto"
		} else {
			warnings = append(warnings, "tool_choice 仅支持 auto，已丢弃")
		}
	}

	// n：n<=1 静默丢弃，n>1 告警（§3.4）。
	if raw, ok := top["n"]; ok {
		var n float64
		if json.Unmarshal(raw, &n) == nil && n > 1 {
			warnings = append(warnings, "不支持多候选（n>1），已按单候选处理")
		}
	}

	// 明确要求告警的丢弃项。
	for _, k := range responsesDroppedWarnKeys {
		if _, ok := top[k]; ok {
			warnings = append(warnings, "丢弃不支持的字段 "+k)
		}
	}
	// 白名单外的未知键。
	for k := range top {
		if _, ok := responsesKnownKeys[k]; !ok {
			warnings = append(warnings, "丢弃不支持的字段 "+k)
		}
	}

	out, merr := json.Marshal(outMap)
	if merr != nil {
		return nil, nil, fmt.Errorf("翻译请求体失败: %w", merr)
	}
	return out, warnings, nil
}

// contentToText 把 chat 消息 content 归一成纯文本：字符串直接返回，
// 数组则拼接其中的文本 part（text / input_text / output_text）。
func contentToText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &parts); err != nil {
		return ""
	}
	var b strings.Builder
	for _, p := range parts {
		switch p.Type {
		case "text", "input_text", "output_text":
			b.WriteString(p.Text)
		}
	}
	return b.String()
}

// userContentParts 把 user 消息的 content 翻成 responses 的 content part 数组：
// 字符串 → input_text；数组里 text → input_text、image_url → input_image；
// 其它 part 类型跳过并告警。
func userContentParts(raw json.RawMessage, warnings *[]string) []map[string]any {
	if len(raw) == 0 {
		return nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return []map[string]any{{"type": "input_text", "text": s}}
	}
	var parts []struct {
		Type     string `json:"type"`
		Text     string `json:"text"`
		ImageURL struct {
			URL string `json:"url"`
		} `json:"image_url"`
	}
	if err := json.Unmarshal(raw, &parts); err != nil {
		return nil
	}
	out := make([]map[string]any, 0, len(parts))
	for _, p := range parts {
		switch p.Type {
		case "text":
			out = append(out, map[string]any{"type": "input_text", "text": p.Text})
		case "image_url":
			out = append(out, map[string]any{"type": "input_image", "image_url": p.ImageURL.URL})
		default:
			*warnings = append(*warnings, "丢弃不支持的消息内容类型 "+p.Type)
		}
	}
	return out
}

// buildResponsesUpstreamRequest 与 buildUpstreamRequest 同形，只把路径换成 /responses、
// body 换成翻译后的 responses 请求体。
func buildResponsesUpstreamRequest(cfg *config, log *logger, clientReq *http.Request, clientModel, upstreamModel string) (*http.Request, map[string]string, []byte, error) {
	if clientReq == nil || clientReq.Body == nil {
		return nil, nil, nil, fmt.Errorf("客户端请求体为空")
	}
	raw, err := readAll(clientReq.Body)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("读取客户端请求体失败: %w", err)
	}
	if len(raw) == 0 {
		return nil, nil, nil, fmt.Errorf("客户端请求体为空")
	}

	body, warnings, terr := chatToResponsesBody(raw, upstreamModel)
	if terr != nil {
		return nil, nil, nil, terr
	}
	// 翻译期丢弃/改写的字段必须留下痕迹，否则客户端会以为自己的参数生效了。
	if log != nil {
		for _, w := range warnings {
			log.warn("responses 请求翻译: "+w, nil)
		}
	}

	// 指纹改造（§3.5）：sourceFormat 必须是 "responses"，工具注入走 tools[i].name 形状。
	var renamed map[string]string
	if cfg.injectTools() {
		result, ferr := applyFingerprint(body, "responses", fingerprintOptions{
			InjectTools:   true,
			ForceStream:   true,
			ClientStreams: true,
		})
		if ferr == nil {
			body = result.Body
			if len(result.Renamed) > 0 && cfg.restoreToolNames() {
				renamed = result.Renamed
			}
		}
	}

	seed := clientModel
	if v := clientReq.Header.Get("X-Request-Id"); v != "" {
		seed = v
	}
	sessionID := sessionIDFor(seed, cfg.ttl, time.Now())

	url := strings.TrimRight(cfg.baseURL(), "/") + "/responses"
	httpReq, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, nil, nil, fmt.Errorf("构造上游请求失败: %w", err)
	}
	// 2026-10-14 起 V2 门禁：会话四头同值 + project=global（见 upstream.go）。
	httpReq.Header.Set("Content-Type", "application/json")
	// V2 要求 Authorization 存在，匿名免费档固定 Bearer public（见 upstream.go）。
	httpReq.Header.Set("Authorization", "Bearer public")
	httpReq.Header.Set("User-Agent", cfg.Upstream.UserAgent)
	httpReq.Header.Set("x-opencode-client", cfg.Upstream.ClientName)
	httpReq.Header.Set("x-opencode-project", "global")
	httpReq.Header.Set("x-opencode-session", sessionID)
	httpReq.Header.Set("x-session-id", sessionID)
	httpReq.Header.Set("x-session-affinity", sessionID)
	httpReq.Header.Set("x-opencode-session-id", sessionID)

	return httpReq, renamed, body, nil
}

// executeStreamUpstreamResponses 只负责拿到上游 /responses 响应体（§4.6），
// 结构与 executeStreamUpstream 一致。
func executeStreamUpstreamResponses(ctx context.Context, cfg *config, log *logger, pool *proxyPool, clientReq *http.Request, body []byte, clientModel, upstreamModel string) (io.ReadCloser, map[string]string, *apiError) {
	if ctx == nil {
		ctx = context.Background()
	}
	buildReq := clientReq.Clone(ctx)
	buildReq.Body = io.NopCloser(bytes.NewReader(body))
	httpReq, renamed, upBody, err := buildResponsesUpstreamRequest(cfg, log, buildReq, clientModel, upstreamModel)
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
		_ = resp.Body.Close()
		return nil, nil, ae
	}

	return resp.Body, renamed, nil
}

// executeNonStreamResponses 执行一次非流式请求（§4.6）：上游免费档不接受 stream:false，
// 所以恒走「强制 stream + 聚合」，把上游 responses SSE 聚合成 chat.completion。
func executeNonStreamResponses(ctx context.Context, cfg *config, log *logger, pool *proxyPool, clientReq *http.Request, body []byte, clientModel, upstreamModel string) ([]byte, string, *apiError) {
	if ctx == nil {
		ctx = context.Background()
	}
	buildReq := clientReq.Clone(ctx)
	buildReq.Body = io.NopCloser(bytes.NewReader(body))
	httpReq, renamed, upBody, err := buildResponsesUpstreamRequest(cfg, log, buildReq, clientModel, upstreamModel)
	if err != nil {
		return nil, "", &apiError{Status: http.StatusInternalServerError, Code: "internal_error", Message: err.Error(), Type: "internal_error"}
	}

	reqCtx, cancel := context.WithTimeout(ctx, cfg.Upstream.RequestTimeout.value(defaultRequestTimeout))
	defer cancel()

	resp, _, ok, lastErrBody, attempts := pool.do(reqCtx, httpReq, upBody, log)
	if !ok {
		return nil, "", &apiError{Status: http.StatusServiceUnavailable, Code: "upstream_unreachable", Message: proxyFailureSummary(attempts), Type: "upstream_unreachable"}
	}
	defer resp.Body.Close()

	status := resp.StatusCode
	if status >= 400 {
		return nil, "application/json", apiErrorFromUpstream(status, lastErrBody)
	}

	bodyBytes, _ := readAll(resp.Body)
	aggregated := aggregateResponsesSSE(bodyBytes, renamed, clientModel)
	return aggregated, "application/json", nil
}

// responsesTranslator 把上游 /responses 的 SSE 事件流翻成 chat.completion.chunk。
// 只做协议翻译：工具名还原与 model 改写由 relayResponsesSSE 复用 relaySSELine 完成。
type responsesTranslator struct {
	clientModel string
	id          string
	created     int64

	firstSent       bool
	sawFunctionCall bool
	incomplete      bool
	terminal        bool
	finalized       bool

	usage    gjson.Result
	hasUsage bool

	outputIndexToTool map[int]int
	nextToolIndex     int
	warnings          []string
}

func newResponsesTranslator(clientModel string) *responsesTranslator {
	return &responsesTranslator{
		clientModel:       clientModel,
		outputIndexToTool: map[int]int{},
	}
}

// feed 处理一条上游 data 载荷（已去掉 "data: " 前缀），返回 0..n 条 chat chunk 载荷
// （JSON 字节，不含 "data: " 前缀与换行）。
func (t *responsesTranslator) feed(payload []byte) [][]byte {
	if !gjson.ValidBytes(payload) {
		return nil
	}
	root := gjson.ParseBytes(payload)
	switch root.Get("type").String() {
	case "response.created":
		t.applyResponseMeta(root.Get("response"))
		return t.ensureFirst()
	case "response.in_progress":
		t.applyResponseMeta(root.Get("response"))
		return nil
	case "response.output_item.added":
		item := root.Get("item")
		if item.Get("type").String() != "function_call" {
			return nil // reasoning / message 不产生帧。
		}
		idx := int(root.Get("output_index").Int())
		i := t.nextToolIndex
		t.nextToolIndex++
		t.outputIndexToTool[idx] = i
		t.sawFunctionCall = true
		tc := map[string]any{
			"index": i,
			"id":    item.Get("call_id").String(),
			"type":  "function",
			"function": map[string]any{
				"name":      item.Get("name").String(),
				"arguments": "",
			},
		}
		return [][]byte{t.deltaChunk(map[string]any{"tool_calls": []any{tc}})}
	case "response.output_text.delta":
		d := root.Get("delta").String()
		if d == "" {
			return nil
		}
		return [][]byte{t.deltaChunk(map[string]any{"content": d})}
	case "response.function_call_arguments.delta":
		idx := int(root.Get("output_index").Int())
		i, ok := t.outputIndexToTool[idx]
		if !ok {
			return nil
		}
		tc := map[string]any{
			"index":    i,
			"function": map[string]any{"arguments": root.Get("delta").String()},
		}
		return [][]byte{t.deltaChunk(map[string]any{"tool_calls": []any{tc}})}
	case "response.completed":
		t.terminal = true
		t.applyResponseMeta(root.Get("response"))
		t.setUsage(root.Get("response.usage"))
		return nil
	case "response.incomplete":
		t.terminal = true
		t.incomplete = true
		t.applyResponseMeta(root.Get("response"))
		t.setUsage(root.Get("response.usage"))
		return nil
	case "response.failed":
		t.terminal = true
		t.warnings = append(t.warnings, "上游返回 response.failed")
		return nil
	case "error":
		t.terminal = true
		t.warnings = append(t.warnings, "上游返回 error 事件")
		return nil
	default:
		return nil
	}
}

// finalize 产出收尾帧（finish_reason + usage）、usage-only 帧，返回的载荷里不含 [DONE]。
func (t *responsesTranslator) finalize() [][]byte {
	if t.finalized {
		return nil
	}
	t.finalized = true

	fr := "stop"
	if t.sawFunctionCall {
		fr = "tool_calls"
	} else if t.incomplete {
		fr = "length"
	}
	usage := t.usageMap()
	out := [][]byte{t.chunk(map[string]any{}, &fr, usage)}
	if usage != nil {
		out = append(out, t.usageChunk(usage))
	}
	return out
}

// drainWarnings 取走并清空累积的告警（供中继层记日志）。
func (t *responsesTranslator) drainWarnings() []string {
	if len(t.warnings) == 0 {
		return nil
	}
	w := t.warnings
	t.warnings = nil
	return w
}

func (t *responsesTranslator) ensureFirst() [][]byte {
	if t.firstSent {
		return nil
	}
	t.firstSent = true
	return [][]byte{t.deltaChunk(map[string]any{"role": "assistant"})}
}

func (t *responsesTranslator) deltaChunk(delta map[string]any) []byte {
	return t.chunk(delta, nil, nil)
}

func (t *responsesTranslator) applyResponseMeta(resp gjson.Result) {
	if !resp.Exists() {
		return
	}
	if id := resp.Get("id").String(); id != "" {
		t.id = id
	}
	if c := resp.Get("created_at"); c.Exists() && c.Int() > 0 {
		t.created = c.Int()
	}
}

func (t *responsesTranslator) setUsage(u gjson.Result) {
	if u.Exists() && u.IsObject() {
		t.usage = u
		t.hasUsage = true
	}
}

func (t *responsesTranslator) usageMap() map[string]any {
	if !t.hasUsage {
		return nil
	}
	u := t.usage
	usage := map[string]any{}
	promptSet, completionSet := false, false
	if v := u.Get("input_tokens"); v.Exists() {
		usage["prompt_tokens"] = v.Int()
		promptSet = true
	}
	if v := u.Get("output_tokens"); v.Exists() {
		usage["completion_tokens"] = v.Int()
		completionSet = true
	}
	if v := u.Get("total_tokens"); v.Exists() {
		usage["total_tokens"] = v.Int()
	} else if promptSet && completionSet {
		usage["total_tokens"] = u.Get("input_tokens").Int() + u.Get("output_tokens").Int()
	}
	if v := u.Get("input_tokens_details.cached_tokens"); v.Exists() {
		usage["prompt_tokens_details"] = map[string]any{"cached_tokens": v.Int()}
	}
	if v := u.Get("output_tokens_details.reasoning_tokens"); v.Exists() {
		usage["completion_tokens_details"] = map[string]any{"reasoning_tokens": v.Int()}
	}
	return usage
}

func (t *responsesTranslator) chunkMeta() map[string]any {
	id := t.id
	if id == "" {
		id = "chatcmpl-zen"
	}
	created := t.created
	if created == 0 {
		created = time.Now().Unix()
	}
	return map[string]any{
		"id":      id,
		"object":  "chat.completion.chunk",
		"created": created,
		"model":   t.clientModel,
	}
}

func (t *responsesTranslator) chunk(delta map[string]any, finish *string, usage map[string]any) []byte {
	m := t.chunkMeta()
	m["choices"] = []any{
		map[string]any{"index": 0, "finish_reason": finish, "delta": delta},
	}
	if usage != nil {
		m["usage"] = usage
	}
	b, _ := json.Marshal(m)
	return b
}

func (t *responsesTranslator) usageChunk(usage map[string]any) []byte {
	m := t.chunkMeta()
	m["choices"] = []any{}
	m["usage"] = usage
	b, _ := json.Marshal(m)
	return b
}

// relayResponsesSSE 按行读上游 responses SSE，逐帧翻译后用 relaySSELine 写出
// （复用工具名还原 + model 改写 + flush 语义），流结束后写出 "data: [DONE]"。
func relayResponsesSSE(ctx context.Context, in io.Reader, out io.Writer, flush func(), renamed map[string]string, clientModel string, restore bool, log *logger) error {
	reader := bufioReader(in)
	t := newResponsesTranslator(clientModel)

	writeChunks := func(chunks [][]byte) error {
		for _, c := range chunks {
			if werr := relaySSELine(append([]byte("data: "), c...), out, flush, renamed, clientModel, restore); werr != nil {
				return werr
			}
		}
		return nil
	}
	logWarnings := func() {
		if log == nil {
			return
		}
		for _, w := range t.drainWarnings() {
			log.warn(w, nil)
		}
	}

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
			trimmed := bytes.TrimSpace(line)
			if bytes.HasPrefix(trimmed, []byte("data:")) {
				payload := bytes.TrimSpace(trimmed[len("data:"):])
				if len(payload) > 0 && !bytes.Equal(payload, []byte("[DONE]")) && gjson.ValidBytes(payload) {
					if werr := writeChunks(t.feed(payload)); werr != nil {
						if log != nil {
							log.warn("responses SSE 中继写出失败: "+werr.Error(), nil)
						}
						return werr
					}
					logWarnings()
				}
			}
		}
		if err != nil {
			if err == io.EOF {
				break
			}
			return err
		}
	}

	// 上游在终结事件前 EOF 时也要收尾（按已有信息给 finish_reason），并记一条 warn。
	if !t.terminal && log != nil {
		log.warn("上游 responses 流在终结事件前结束，按已有信息收尾", nil)
	}
	if werr := writeChunks(t.finalize()); werr != nil {
		return werr
	}
	if _, werr := io.WriteString(out, "data: [DONE]\n\n"); werr != nil {
		return werr
	}
	if flush != nil {
		flush()
	}
	return nil
}

// aggregateResponsesSSE 把完整的 responses SSE 响应体翻成 chat.completion（复用 aggregateSSE）。
func aggregateResponsesSSE(sse []byte, renamed map[string]string, clientModel string) []byte {
	t := newResponsesTranslator(clientModel)
	var buf bytes.Buffer
	appendChunks := func(chunks [][]byte) {
		for _, c := range chunks {
			buf.WriteString("data: ")
			buf.Write(c)
			buf.WriteString("\n\n")
		}
	}
	for _, line := range strings.Split(string(sse), "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(trimmed, "data:"))
		if payload == "" || payload == "[DONE]" || !gjson.Valid(payload) {
			continue
		}
		appendChunks(t.feed([]byte(payload)))
	}
	appendChunks(t.finalize())
	return aggregateSSE(buf.Bytes(), renamed, clientModel)
}
