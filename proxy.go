package main

import (
	"context"
	"crypto/rand"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/net/proxy"
)

// proxyEntry 是代理池里的一条代理（每个代理一个复用的 http.Client），
// 全部内存态（重启清空，不做持久化，见 §6.3）。
type proxyEntry struct {
	raw     string       // 规范化后的地址（凭据已脱敏在 display 里）
	display string       // 脱敏后的展示串（host:port 或 direct）
	dialer  proxy.Dialer // 用于 socks5 / socks5h
	client  *http.Client // 复用的 HTTP 客户端

	mu        sync.Mutex
	inflight  int64
	ok        int64
	fail      int64
	coolUntil time.Time
	coolLevel int
}

// cooldownInfo 返回当前是否在冷却中以及剩余冷却时长。
func (e *proxyEntry) cooldownInfo(now time.Time) (cooling bool, remain time.Duration) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if now.Before(e.coolUntil) {
		return true, e.coolUntil.Sub(now)
	}
	return false, 0
}

// markCooled 进入/加深冷却（指数退避 + 抖动）。
func (e *proxyEntry) markCooled(now time.Time, base time.Duration, factor float64, max time.Duration, jitter float64, level int) time.Duration {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.coolLevel = level
	d := base
	for i := 1; i < level; i++ {
		d = time.Duration(float64(d) * factor)
	}
	if d > max {
		d = max
	}
	if jitter > 0 {
		// (1 ± jitter)
		span := int64(float64(d) * jitter)
		delta, _ := rand.Int(rand.Reader, big.NewInt(span*2+1))
		d = d - time.Duration(span) + time.Duration(delta.Int64())
		if d < 0 {
			d = 0
		}
	}
	e.coolUntil = now.Add(d)
	return d
}

// markOK 成功后清冷却、重置退避层级。
func (e *proxyEntry) markOK() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.coolLevel = 0
	e.coolUntil = time.Time{}
}

func (e *proxyEntry) addInflight(delta int64) int64 {
	return atomic.AddInt64(&e.inflight, delta)
}

// proxyPool 是出口代理池（§6）：解析、校验、选择、冷却、重试。
type proxyPool struct {
	cfg     *config
	entries []*proxyEntry

	strategy string
	rr       uint64 // round_robin 原子计数

	mu      sync.RWMutex
	builtAt time.Time
}

// parseProxyEntry 解析一条代理配置；非法项 → 中文 error（带位置索引）。
func parseProxyEntry(item string, idx int) (*proxyEntry, error) {
	raw := strings.TrimSpace(item)
	if raw == "" {
		return nil, fmt.Errorf("配置错误: proxies[%d]: 空代理地址", idx)
	}
	lower := strings.ToLower(raw)
	if lower == "direct" || lower == "none" || lower == "off" {
		return newDirectEntry(raw), nil
	}

	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("配置错误: proxies[%d]: 无法解析代理地址 %q: %v", idx, raw, err)
	}
	scheme := strings.ToLower(u.Scheme)
	switch scheme {
	case "http", "https":
		return newHTTPProxyEntry(raw, u, scheme)
	case "socks5", "socks5h":
		return newSOCKS5Entry(raw, u, scheme)
	default:
		return nil, fmt.Errorf("配置错误: proxies[%d]: 不支持的代理地址 %q（只支持 http/https/socks5/socks5h/direct）", idx, raw)
	}
}

// newDirectEntry 构造直连代理。
func newDirectEntry(raw string) *proxyEntry {
	tr := newDirectTransport()
	return &proxyEntry{
		raw:     "direct",
		display: "direct",
		dialer:  proxy.Direct,
		client:  &http.Client{Transport: tr},
	}
}

// newHTTPProxyEntry 构造 http/https 代理。
func newHTTPProxyEntry(raw string, u *url.URL, scheme string) (*proxyEntry, error) {
	// 脱敏：user:***@host:port
	display := scheme + "://" + maskUser(u) + u.Host
	tr := newDirectTransport()
	tr.Proxy = http.ProxyURL(u)
	return &proxyEntry{
		raw:     raw,
		display: display,
		dialer:  proxy.Direct, // 由 Transport.Proxy 负责，不走 dialer
		client:  &http.Client{Transport: tr},
	}, nil
}

// newSOCKS5Entry 构造 socks5 / socks5h 代理。
func newSOCKS5Entry(raw string, u *url.URL, scheme string) (*proxyEntry, error) {
	display := scheme + "://" + maskUser(u) + u.Host
	var auth *proxy.Auth
	if u.User != nil {
		pw, _ := u.User.Password()
		auth = &proxy.Auth{User: u.User.Username(), Password: pw}
	}
	// socks5h：DNS 也走代理（用 proxy.Direct 作 forward 让 x/net 自己走代理解析）；
	// socks5：x/net 默认也走代理解析目标，二者在此统一处理（注释说明差异）。
	dialer, err := proxy.SOCKS5("tcp", u.Host, auth, proxy.Direct)
	if err != nil {
		return nil, fmt.Errorf("配置错误: 无法构造 SOCKS5 代理 %q: %v", display, err)
	}
	tr := &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			return dialer.Dial(network, addr)
		},
		DialTLSContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			c, err := dialer.Dial(network, addr)
			if err != nil {
				return nil, err
			}
			// 升级为 TLS（socks5 只负责 TCP 隧道）。
			tlsCfg := cloneTLSConfig()
			return tlsClient(c, addr, tlsCfg)
		},
		MaxIdleConnsPerHost: 32,
		IdleConnTimeout:     90 * time.Second,
		ForceAttemptHTTP2:   false,
	}
	return &proxyEntry{
		raw:     raw,
		display: display,
		dialer:  dialer,
		client:  &http.Client{Transport: tr},
	}, nil
}

// maskUser 把 user:pass@ 脱敏成 user:***@。
func maskUser(u *url.URL) string {
	if u.User == nil {
		return ""
	}
	return u.User.Username() + ":***@"
}

// newDirectTransport 构造直连 Transport（不设 Proxy 字段，即直连）。
// 超时统一由 context 管（§6.4），这里不设整体 Timeout。
func newDirectTransport() *http.Transport {
	return &http.Transport{
		DialContext: (&net.Dialer{
			Timeout:   10 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 60 * time.Second, // 默认，§5 会用 first_byte_timeout 覆盖
		MaxIdleConnsPerHost:   32,
		IdleConnTimeout:       90 * time.Second,
		ForceAttemptHTTP2:     false,
	}
}

// buildProxyPool 解析并校验代理配置（§6.2）。proxies 与 proxy_file 合并
// （先内联，后文件）；都不存在 → 池里只有一个隐式 direct。
// 失败 fast：非法条目 / proxy_file 声明了但读不了 → 返回中文 error。
func buildProxyPool(cfg *config, logger_ *logger) (*proxyPool, error) {
	var items []string
	for i, p := range cfg.Proxy.Proxies {
		items = append(items, p)
		_ = i
	}

	if cfg.Proxy.ProxyFile != "" {
		lines, err := readProxyFile(cfg.Proxy.ProxyFile)
		if err != nil {
			return nil, fmt.Errorf("配置错误: proxy_file: %v", err)
		}
		items = append(items, lines...)
	}

	if len(items) == 0 {
		// 隐式直连，行为与改造前一致。
		items = []string{"direct"}
	}

	// 去重（按规范化 URL 字符串）。
	seen := map[string]int{}
	entries := make([]*proxyEntry, 0, len(items))
	for idx, it := range items {
		e, err := parseProxyEntry(it, idx)
		if err != nil {
			return nil, err
		}
		if _, dup := seen[e.raw]; dup {
			continue
		}
		seen[e.raw] = len(entries)
		entries = append(entries, e)
	}

	pool := &proxyPool{cfg: cfg, entries: entries, strategy: cfg.Proxy.Strategy, builtAt: time.Now()}

	// 每个代理 transport 的 ResponseHeaderTimeout = first_byte_timeout（§5.6 / §6.4）。
	fbt := cfg.Upstream.FirstByteTimeout.value(60 * time.Second)
	for _, e := range entries {
		if tr, ok := e.client.Transport.(*http.Transport); ok {
			tr.ResponseHeaderTimeout = fbt
		}
	}

	if logger_ != nil {
		displays := make([]string, 0, len(entries))
		for _, e := range entries {
			displays = append(displays, e.display)
		}
		// ★ 宿主只打印 message（fields 会被丢掉），所以池摘要必须拼进 message。
		logger_.info(fmt.Sprintf("代理池已构建: %d 条 strategy=%s proxies=[%s]",
			len(entries), pool.strategy, strings.Join(displays, ", ")), nil)
	}
	return pool, nil
}

// readProxyFile 读取代理文件，支持 # 注释与空行。
func readProxyFile(path string) ([]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取代理文件 %q 失败: %w", path, err)
	}
	var out []string
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		out = append(out, line)
	}
	return out, nil
}

// displays 返回所有代理的脱敏展示串（测试用）。
func (p *proxyPool) displays() []string {
	out := make([]string, 0, len(p.entries))
	for _, e := range p.entries {
		out = append(out, e.display)
	}
	return out
}

// pick 选择一个代理（§6.3）。冷却中的默认跳过；全冷 → 选剩余冷却最短的那个。
func (p *proxyPool) pick(now time.Time) *proxyEntry {
	p.mu.RLock()
	entries := p.entries
	p.mu.RUnlock()
	if len(entries) == 0 {
		return nil
	}

	var avail []*proxyEntry
	var shortest *proxyEntry
	var shortestRemain time.Duration = -1
	for _, e := range entries {
		cooling, remain := e.cooldownInfo(now)
		if !cooling {
			avail = append(avail, e)
			continue
		}
		if shortestRemain < 0 || remain < shortestRemain {
			shortestRemain = remain
			shortest = e
		}
	}

	if len(avail) > 0 {
		return p.selectFrom(avail, now)
	}
	// 全部冷却：退化到剩余冷却最短者（绝不能让请求卡住或直接失败）。
	return shortest
}

// selectFrom 在可用代理里按策略选一个（§6.3）。
func (p *proxyPool) selectFrom(avail []*proxyEntry, now time.Time) *proxyEntry {
	switch p.strategy {
	case "random":
		n, _ := rand.Int(rand.Reader, big.NewInt(int64(len(avail))))
		return avail[n.Int64()]
	case "least_inflight":
		best := avail[0]
		bestIn := best.addInflight(0)
		for _, e := range avail[1:] {
			if in := e.addInflight(0); in < bestIn {
				best, bestIn = e, in
			}
		}
		return best
	default: // round_robin
		i := atomic.AddUint64(&p.rr, 1) - 1
		return avail[int(i)%len(avail)]
	}
}

// do 用代理池执行一次上游请求：按 max_attempts 逐代理重试，每次换一个不同的
// 代理；全部失败 → 返回聚合错误。success 判定：2xx/3xx。
// 返回最后一个响应、使用的代理、以及是否成功。
func (p *proxyPool) do(ctx context.Context, base *http.Request, body []byte, logger_ *logger) (resp *http.Response, used *proxyEntry, ok bool, lastErrBody []byte, attempts []attemptResult) {
	maxAttempts := p.cfg.proxyMaxAttempts()
	now := time.Now()
	usedSet := map[*proxyEntry]bool{}
	var lastResp *http.Response
	var lastUsed *proxyEntry
	var lastBody []byte

	for attempt := 0; attempt < maxAttempts; attempt++ {
		// 优先挑一个本次没用过的代理（每次换不同的）。
		candidate := p.pickDifferent(now, usedSet)
		if candidate == nil {
			// 池里所有代理都用过了（例如只配了一条 direct）：允许重试同一条，
			// 总次数仍受 proxy_max_attempts 约束。否则单条池遇到一次瞬时传输层
			// 错误就直接 503（实测踩过：默认直连池一次抖动 → 整个请求失败）。
			candidate = p.pick(now)
		}
		if candidate == nil {
			break
		}
		usedSet[candidate] = true

		req := cloneWithBody(ctx, base, body)

		candidate.addInflight(1)
		start := time.Now()
		r, doErr := candidate.client.Do(req)
		elapsed := time.Since(start)
		candidate.addInflight(-1)
		if doErr != nil {
			// 传输层错误 → 当作失败（err 用于失败摘要）。
			ar := attemptResult{display: candidate.display, status: 0, err: doErr, elapsed: elapsed}
			attempts = append(attempts, ar)
			atomic.AddInt64(&candidate.fail, 1)
			p.coolProxy(candidate, now, 0, logger_)
			continue
		}

		// 成功（2xx/3xx）。
		status := 0
		if r != nil {
			status = r.StatusCode
		}

		ar := attemptResult{display: candidate.display, status: status, elapsed: elapsed}
		attempts = append(attempts, ar)

		if r != nil && status < 400 {
			candidate.markOK()
			atomic.AddInt64(&candidate.ok, 1)
			if logger_ != nil {
				logger_.debug("请求: proxy="+candidate.display+" status="+fmt.Sprintf("%d", status)+" ms="+fmt.Sprintf("%d", elapsed.Milliseconds()), nil)
			}
			return r, candidate, true, nil, attempts
		}

		// 失败判定：状态码命中 statuses（无论是否冷却，都读取并缓存最后一个上游响应体，
		// 以便调用方把真实上游状态码/错误体带出，见 §6.3 / §9）。
		if r != nil && r.Body != nil {
			lastBody, _ = readAll(r.Body)
			_ = r.Body.Close()
		}
		lastResp = r
		lastUsed = candidate
		shouldCool := false
		for _, s := range p.cfg.Proxy.Cooldown.Statuses {
			if status == s {
				shouldCool = true
				break
			}
		}
		atomic.AddInt64(&candidate.fail, 1)
		if shouldCool {
			p.coolProxy(candidate, now, status, logger_)
		}
	}

	// 所有尝试都命中冷却状态（上游真的返回了错误码）：返回最后一个响应与错误体，
	// 让调用方以真实状态码上报；ok=false 仅在纯传输层全失败时。
	return lastResp, lastUsed, lastResp != nil, lastBody, attempts
}

// Do 选一条代理发一次请求（§10）：失败（传输层错误或状态码命中冷却表）→
// 冷却该代理并返回错误；成功（2xx/3xx）→ 清冷却并返回响应。不做多代理重试。
// 它满足 §5.3 的 httpDoer 接口（目录拉取的出口注入）。
// 非 2xx/3xx 且未命中冷却表的状态码同样按失败返回错误，但不冷却。
func (p *proxyPool) Do(req *http.Request) (*http.Response, error) {
	e := p.pick(time.Now())
	if e == nil {
		return nil, fmt.Errorf("代理池为空，无法发出请求")
	}
	e.addInflight(1)
	start := time.Now()
	resp, err := e.client.Do(req)
	e.addInflight(-1)
	if err != nil {
		// 传输层错误 → 失败并冷却。
		atomic.AddInt64(&e.fail, 1)
		p.coolProxy(e, start, 0, nil)
		return nil, err
	}
	if resp.StatusCode >= 400 {
		// 读完并关掉错误体，避免连接泄漏；命中冷却表才冷却。
		_, _ = readAll(resp.Body)
		_ = resp.Body.Close()
		atomic.AddInt64(&e.fail, 1)
		for _, s := range p.cfg.Proxy.Cooldown.Statuses {
			if resp.StatusCode == s {
				p.coolProxy(e, start, resp.StatusCode, nil)
				break
			}
		}
		return nil, fmt.Errorf("代理 %s 返回状态 %d", e.display, resp.StatusCode)
	}
	// 成功（2xx/3xx）→ 清冷却。
	e.markOK()
	atomic.AddInt64(&e.ok, 1)
	return resp, nil
}

type attemptResult struct {
	display string
	status  int
	err     error
	elapsed time.Duration
}

// pickDifferent 在未被使用过的代理里挑一个（优先可用的；全用过则挑任意）。
func (p *proxyPool) pickDifferent(now time.Time, used map[*proxyEntry]bool) *proxyEntry {
	p.mu.RLock()
	entries := p.entries
	p.mu.RUnlock()
	var avail []*proxyEntry
	for _, e := range entries {
		if used[e] {
			continue
		}
		cooling, _ := e.cooldownInfo(now)
		if !cooling {
			avail = append(avail, e)
		}
	}
	if len(avail) > 0 {
		return p.selectFrom(avail, now)
	}
	for _, e := range entries {
		if !used[e] {
			return e
		}
	}
	return nil
}

// coolProxy 让一个代理进入/加深冷却（§6.3）。status 仅用于日志（0 = 传输层错误）。
func (p *proxyPool) coolProxy(e *proxyEntry, now time.Time, status int, logger_ *logger) {
	cd := p.cfg.Proxy.Cooldown
	level := e.coolLevel + 1
	d := e.markCooled(now, cd.Base.V, *cd.Factor, cd.Max.V, *cd.Jitter, level)
	if logger_ != nil {
		if status == 0 {
			logger_.info(fmt.Sprintf("代理冷却: %s %s（第 %d 次，传输层错误）", e.display, d.Round(time.Second), level), nil)
		} else {
			logger_.info(fmt.Sprintf("代理冷却: %s %s（第 %d 次，状态 %d）", e.display, d.Round(time.Second), level, status), nil)
		}
	}
}

var _ = context.Background
