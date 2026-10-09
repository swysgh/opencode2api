package main

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"net/http"

	"github.com/tidwall/sjson"
)

// errReadEOF 是 io.EOF 的别名，方便流式泵判定流结束。
var errReadEOF = io.EOF

// bufioReader 返回带缓冲的 reader（按行读上游 SSE，见 §7.3）。
func bufioReader(r io.Reader) *bufio.Reader {
	if br, ok := r.(*bufio.Reader); ok {
		return br
	}
	return bufio.NewReaderSize(r, 64*1024)
}

// readAll 读取整个响应体（非流式用）。
func readAll(r io.Reader) ([]byte, error) {
	if ra, ok := r.(interface{ ReadAll() ([]byte, error) }); ok {
		return ra.ReadAll()
	}
	var buf bytes.Buffer
	if _, err := io.Copy(&buf, r); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// cloneHeader 浅拷贝响应头（非流式 pass 用）。
func cloneHeader(h http.Header) http.Header {
	out := make(http.Header, len(h))
	for k, vv := range h {
		out[k] = append([]string(nil), vv...)
	}
	return out
}

// sjsonSet 是 sjson.SetBytes 的薄封装，便于在 plugin.go 里改写 JSON 字段。
func sjsonSet(body []byte, path, value string) ([]byte, error) {
	return sjson.SetBytes(body, path, value)
}

// noTimeoutCtx 返回无超时上下文（测试用：避免 300s 预算影响单测）。
func noTimeoutCtx() context.Context {
	return context.Background()
}
