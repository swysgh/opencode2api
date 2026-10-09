package main

import (
	"bytes"
	"context"
	"io"
	"net/http"
)

type apiError struct {
	Status  int
	Code    string
	Message string
	Type    string
}

func (e *apiError) Error() string {
	return e.Message
}

// cloneWithBody 在每次重试时构造一个带新 body reader 的请求（避免 *bytes.Reader 被读到 EOF 后无法重发）。
func cloneWithBody(ctx context.Context, base *http.Request, body []byte) *http.Request {
	r := base.Clone(ctx)
	r.Body = io.NopCloser(bytes.NewReader(body))
	r.ContentLength = int64(len(body))
	return r
}
