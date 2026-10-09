package main

import (
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

// logger 是独立服务的日志器（§4）：级别过滤 + 可选写文件 + 并发安全。
// 注意：proxy.go / proxy_test.go 会直接构造 &logger{level: "debug"}（out 为 nil），
// 此时输出走 os.Stdout —— 不能改那些调用点。
type logger struct {
	level string    // debug|info|warn|error
	out   io.Writer // stdout 或 log_file
	mu    sync.Mutex
	file  *os.File // 打开文件时持有，便于 Close
}

// newLogger 构造日志器。file 为空 → os.Stdout；否则以 O_APPEND|O_CREATE|O_WRONLY、
// 0644 打开（§4）。level 非法 → 返回中文 error（配置层已校验过，这里兜底）。
func newLogger(level, file string) (*logger, error) {
	lv := strings.ToLower(strings.TrimSpace(level))
	if lv == "" {
		lv = "info"
	}
	if _, ok := levelRank[lv]; !ok {
		return nil, fmt.Errorf("日志级别非法: %q（只支持 debug|info|warn|error）", level)
	}
	l := &logger{level: lv, out: os.Stdout}
	if file != "" {
		f, err := os.OpenFile(file, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			return nil, fmt.Errorf("打开日志文件 %q 失败: %w", file, err)
		}
		l.out = f
		l.file = f
	}
	return l, nil
}

// enabled 按级别过滤（§4）：levelRank[msgLevel] < levelRank[l.level] 则丢弃。
func (l *logger) enabled(level string) bool {
	lv, ok := levelRank[strings.ToLower(l.level)]
	if !ok {
		lv = 1
	}
	rv, ok := levelRank[strings.ToLower(level)]
	if !ok {
		rv = 1
	}
	return rv >= lv
}

// log 输出一条日志。格式（§4）：
//
//	2026-10-09T16:00:00+08:00 INFO  消息 key=value key2="带 空格"
//
// fields 为 nil 时省略尾部；字段按 key 排序保证输出稳定。
func (l *logger) log(level, msg string, fields map[string]any) {
	if l == nil || !l.enabled(level) {
		return
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s %-5s %s", time.Now().Format(time.RFC3339), strings.ToUpper(level), msg)
	if len(fields) > 0 {
		keys := make([]string, 0, len(fields))
		for k := range fields {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			b.WriteString(" ")
			b.WriteString(k)
			b.WriteString("=")
			b.WriteString(formatLogValue(fields[k]))
		}
	}
	b.WriteString("\n")

	l.mu.Lock()
	defer l.mu.Unlock()
	out := l.out
	if out == nil { // 直接构造的 logger（&logger{level: "debug"}）→ stdout
		out = os.Stdout
	}
	_, _ = io.WriteString(out, b.String())
}

// formatLogValue 把字段值渲染成日志行里的 value：值里有空白/引号用 %q，其余原样（§4）。
func formatLogValue(v any) string {
	var s string
	switch t := v.(type) {
	case string:
		s = t
	case time.Duration:
		s = t.String()
	default:
		s = fmt.Sprintf("%v", t)
	}
	if strings.ContainsAny(s, " \t\n\r\"") {
		return fmt.Sprintf("%q", s)
	}
	return s
}

func (l *logger) debug(msg string, fields map[string]any) { l.log("debug", msg, fields) }
func (l *logger) info(msg string, fields map[string]any)  { l.log("info", msg, fields) }
func (l *logger) warn(msg string, fields map[string]any)  { l.log("warn", msg, fields) }
func (l *logger) error(msg string, fields map[string]any) { l.log("error", msg, fields) }

// Close 关闭日志文件（stdout 模式下是空操作，§4）。
func (l *logger) Close() error {
	if l == nil || l.file == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	err := l.file.Close()
	l.file = nil
	return err
}
