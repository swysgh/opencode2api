package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

// httpDoer 是目录拉取的出口抽象（§5.3）：*http.Client 与 *proxyPool 都满足。
type httpDoer interface {
	Do(*http.Request) (*http.Response, error)
}

// catalogFileShape 是 GET <base_url>/models 返回的形状（公开接口，无需鉴权）。
type catalogFileShape struct {
	Object string `json:"object"`
	Data   []struct {
		ID string `json:"id"`
	} `json:"data"`
}

// catalog 缓存上游目录（落盘 catalog_file，§3.5）。
type catalog struct {
	mu          sync.RWMutex
	path        string
	lastRefresh time.Time
	upstreamIDs []string // 目录里出现的全部上游 id（已去重、排序）
}

// loadCatalog 读取落盘缓存（若文件存在）。失败返回空 catalog（不报错）。
func loadCatalog(path string) *catalog {
	c := &catalog{path: path}
	if path == "" {
		return c
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return c
	}
	var shape catalogFileShape
	if err := json.Unmarshal(data, &shape); err != nil {
		return c
	}
	ids := make([]string, 0, len(shape.Data))
	seen := map[string]struct{}{}
	for _, d := range shape.Data {
		if d.ID == "" {
			continue
		}
		if _, dup := seen[d.ID]; dup {
			continue
		}
		seen[d.ID] = struct{}{}
		ids = append(ids, d.ID)
	}
	sort.Strings(ids)
	c.upstreamIDs = ids
	// 文件 mtime 当作上次刷新时间。
	if fi, err := os.Stat(path); err == nil {
		c.lastRefresh = fi.ModTime()
	}
	return c
}

// fetch 同步重拉目录。超时由 ctx + timeout 控制（catalog_timeout）。拉取失败返回 error，
// 调用方应使用缓存兜底（不要让启动失败，见 §5.3）。
// doer 为 nil → 默认直连 &http.Client{Timeout: timeout}（catalog_via_proxy=false 时，
// 避免代理配置错误连目录都拉不到）；非 nil → 用注入的出口（如代理池的 Do）。
// UA 用 zen2api-catalog/<version>（§5.3）。
func (c *catalog) fetch(ctx context.Context, baseURL string, timeout time.Duration, doer httpDoer) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(baseURL, "/")+"/models", nil)
	if err != nil {
		return fmt.Errorf("构造目录请求失败: %w", err)
	}
	req.Header.Set("User-Agent", "zen2api-catalog/"+version)
	if doer == nil {
		// 默认直连（catalog 拉取不走代理池，避免代理配置错误时连不上目录）。
		doer = &http.Client{Timeout: timeout}
	}
	resp, err := doer.Do(req)
	if err != nil {
		return fmt.Errorf("拉取目录失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("拉取目录返回 %d", resp.StatusCode)
	}
	var shape catalogFileShape
	if err := json.NewDecoder(resp.Body).Decode(&shape); err != nil {
		return fmt.Errorf("解析目录失败: %w", err)
	}
	ids := make([]string, 0, len(shape.Data))
	seen := map[string]struct{}{}
	for _, d := range shape.Data {
		if d.ID == "" {
			continue
		}
		if _, dup := seen[d.ID]; dup {
			continue
		}
		seen[d.ID] = struct{}{}
		ids = append(ids, d.ID)
	}
	sort.Strings(ids)

	c.mu.Lock()
	c.upstreamIDs = ids
	c.lastRefresh = time.Now()
	c.mu.Unlock()

	// 落盘。
	if c.path != "" {
		if data, mErr := json.Marshal(shape); mErr == nil {
			_ = os.WriteFile(c.path, data, 0o644)
		}
	}
	return nil
}

// list 返回当前缓存的上游 id 列表（拷贝）。
func (c *catalog) list() []string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]string, len(c.upstreamIDs))
	copy(out, c.upstreamIDs)
	return out
}

// age 返回自上次刷新过去的时长。
func (c *catalog) age() time.Duration {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.lastRefresh.IsZero() {
		return 1<<63 - 1
	}
	return time.Since(c.lastRefresh)
}

// ensure context import used.
var _ = context.Background
