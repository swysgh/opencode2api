package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
)

// newTestPool 用给定配置构造代理池（不触发网络）。
func newTestPool(t *testing.T, cfgYAML string) *proxyPool {
	t.Helper()
	cfg, err := decodeConfig([]byte(cfgYAML))
	if err != nil {
		t.Fatalf("解析配置失败: %v", err)
	}
	pool, err := buildProxyPool(&cfg, &logger{level: "debug"})
	if err != nil {
		t.Fatalf("构建代理池失败: %v", err)
	}
	return pool
}

// configureExpectError 验证非法代理配置在 reconfigure（即构建代理池）时报错（§13.1）。
func configureExpectError(t *testing.T, cfgYAML, want string) {
	t.Helper()
	p := newPlugin(&fakeHost{})
	raw, _ := json.Marshal(map[string]any{"config_yaml": []byte(cfgYAML)})
	_, err := p.HandleMethod(pluginabi.MethodPluginReconfigure, raw)
	if err == nil {
		t.Fatalf("期望报错（%s）", want)
	}
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("错误信息 %q 未包含 %q", err.Error(), want)
	}
}

// TestProxyPoolRoundRobin 验证轮转顺序（§13.1）。
func TestProxyPoolRoundRobin(t *testing.T) {
	pool := newTestPool(t, "proxies:\n  - http://10.0.0.1:8080\n  - http://10.0.0.2:8080\n  - http://10.0.0.3:8080\n")
	if len(pool.entries) != 3 {
		t.Fatalf("池大小 = %d", len(pool.entries))
	}
	now := time.Now()
	got := []string{}
	for i := 0; i < 3; i++ {
		e := pool.pick(now)
		got = append(got, e.display)
	}
	// round_robin 应 1→2→3 顺序（无冷却）。
	if got[0] == got[1] || got[1] == got[2] || got[0] == got[2] {
		t.Fatalf("轮转未分散: %v", got)
	}
}

// TestProxyPoolCooledSkipped 验证冷却中的代理被跳过（§13.1）。
func TestProxyPoolCooledSkipped(t *testing.T) {
	pool := newTestPool(t, "proxies:\n  - http://10.0.0.1:8080\n  - http://10.0.0.2:8080\n")
	now := time.Now()
	// 让第一个代理进入冷却。
	pool.entries[0].markCooled(now, 30*time.Second, 2.0, 10*time.Minute, 0, 1)
	e := pool.pick(now)
	if e == pool.entries[0] {
		t.Fatal("冷却中的代理不应被选中")
	}
}

// TestProxyPoolAllCooledFallback 验证全部冷却时回退到剩余冷却最短者（§13.1）。
func TestProxyPoolAllCooledFallback(t *testing.T) {
	pool := newTestPool(t, "proxies:\n  - http://10.0.0.1:8080\n  - http://10.0.0.2:8080\n")
	now := time.Now()
	pool.entries[0].markCooled(now, 30*time.Second, 2.0, 10*time.Minute, 0, 1)
	pool.entries[1].markCooled(now, 5*time.Minute, 2.0, 10*time.Minute, 0, 3)
	// 第一个剩余冷却更短 → 应被选中。
	e := pool.pick(now)
	if e != pool.entries[0] {
		t.Fatalf("应回退到剩余冷却最短者 %s，实际 %s", pool.entries[0].display, e.display)
	}
}

// TestProxyPoolInvalidEntry 验证非法代理条目 → 配置报错（含中文原因与条目位置，§13.1）。
func TestProxyPoolInvalidEntry(t *testing.T) {
	configureExpectError(t, "proxies:\n  - http://10.0.0.1:8080\n  - ftp://10.0.0.2:8080\n", "proxies[1]")
	// 错误信息需含中文原因。
	configureExpectError(t, "proxies:\n  - ftp://x\n", "不支持的代理地址")
}

// TestProxyPoolMissingFile 验证声明了 proxy_file 但文件读不了 → 报错（§13.1）。
func TestProxyPoolMissingFile(t *testing.T) {
	configureExpectError(t, "proxy_file: /nonexistent/path/proxies.txt\n", "proxy_file")
}

// TestProxyFileParsing 验证 proxy_file 支持注释与空行（§13.1）。
func TestProxyFileParsing(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "proxies.txt")
	content := "# 注释行\n\nhttp://10.0.0.5:8080\n  # 再来一条\nsocks5://10.0.0.6:1080\n\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	pool := newTestPool(t, "proxy_file: "+path+"\n")
	if len(pool.entries) != 2 {
		t.Fatalf("解析后应为 2 条，实际 %d: %v", len(pool.entries), pool.displays())
	}
}

// TestProxyPoolMixedSchemes 验证 direct 与 http/socks5 混合（§13.1）。
func TestProxyPoolMixedSchemes(t *testing.T) {
	pool := newTestPool(t, "proxies:\n  - direct\n  - http://10.0.0.1:8080\n  - socks5://user:pass@10.0.0.2:1080\n")
	if len(pool.entries) != 3 {
		t.Fatalf("混合池大小 = %d", len(pool.entries))
	}
	// 凭据应脱敏成 user:***@。
	for _, e := range pool.entries {
		if strings.Contains(e.display, "pass") {
			t.Fatalf("凭据未脱敏: %s", e.display)
		}
	}
}

// TestProxyPoolImplicitDirect 验证空配置 → 隐式直连（§6.2）。
func TestProxyPoolImplicitDirect(t *testing.T) {
	pool := newTestPool(t, "")
	if len(pool.entries) != 1 || pool.entries[0].display != "direct" {
		t.Fatalf("隐式直连应有 1 条 direct，实际 %v", pool.displays())
	}
}

// TestProxyPoolRetriesSingleEntry 回归测试：池里只有一条（默认 direct）时，
// 一次失败仍应重试到 proxy_max_attempts 次，而不是立刻放弃。
// 实测踩过：默认单条池遇到一次瞬时传输层错误 → 整个请求 503。
func TestProxyPoolRetriesSingleEntry(t *testing.T) {
	calls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(500)
		w.Write([]byte(`{"error":{"type":"server_error","message":"boom"}}`))
	}))
	defer upstream.Close()

	cfg, err := decodeConfig([]byte("proxies: [direct]\nproxy_max_attempts: 3\nproxy_cooldown:\n  statuses: [500]\n  base: 1s\n"))
	if err != nil {
		t.Fatal(err)
	}
	pool, err := buildProxyPool(&cfg, &logger{level: "debug"})
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest(http.MethodPost, upstream.URL, strings.NewReader(`{"model":"x"}`))
	resp, _, ok, lastBody, attempts := pool.do(noTimeoutCtx(), req, []byte(`{"model":"x"}`), &logger{level: "debug"})
	if len(attempts) != 3 {
		t.Fatalf("单条池应重试 3 次，实际 attempts=%d（%v）", len(attempts), attempts)
	}
	if calls != 3 {
		t.Fatalf("上游应收到 3 次请求，实际 %d", calls)
	}
	// do 的 ok 语义是「拿到了 HTTP 响应」（状态码由调用方判定），所以这里 ok=true 且带 500。
	if !ok || resp == nil || resp.StatusCode != 500 {
		t.Fatalf("应返回上游响应（ok=%v resp=%v）", ok, resp)
	}
	if !strings.Contains(string(lastBody), "boom") {
		t.Fatalf("应带回最后一个上游错误体，实际 %q", lastBody)
	}
}

// TestProxyFailover 验证第一个代理连不上时换下一个并成功（§13.2）。
func TestProxyFailover(t *testing.T) {
	// 假上游。
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`data: {"choices":[{"delta":{"content":"hi"}}]}` + "\n\n" + `data: [DONE]` + "\n\n"))
	}))
	defer upstream.Close()

	// 第一个代理指向必连不上的地址，第二个是直连（相对 URL 用 httptest 的 host 需要绝对，
	// 这里用 direct 但把 upstream 作为 base_url 重写请求）。
	// 为简单且可控：构造 pool 让第一个是坏代理、第二个是进程内直连到上游。
	cfg, err := decodeConfig([]byte("proxies:\n  - http://127.0.0.1:1\n  - direct\nproxy_max_attempts: 2\n"))
	if err != nil {
		t.Fatal(err)
	}
	pool, err := buildProxyPool(&cfg, &logger{level: "debug"})
	if err != nil {
		t.Fatal(err)
	}

	// 用 httptest 的 server URL 直接构造请求；direct 代理会真正打到它。
	req, _ := http.NewRequest(http.MethodPost, upstream.URL, strings.NewReader(`{"model":"x","stream":true}`))
	resp, used, ok, _, attempts := pool.do(noTimeoutCtx(), req, []byte(`{"model":"x","stream":true}`), &logger{level: "debug"})
	if !ok {
		t.Fatalf("应最终成功，实际失败: %v", attempts)
	}
	defer resp.Body.Close()
	if used == pool.entries[0] {
		t.Fatal("失败的代理不应被最终使用")
	}
	// 第一个代理应进入冷却。
	cooling, _ := pool.entries[0].cooldownInfo(time.Now())
	if !cooling {
		t.Fatal("第一个失败代理应进入冷却")
	}
	body, _ := readAll(resp.Body)
	if !strings.Contains(string(body), "hi") {
		t.Fatalf("响应体不对: %s", body)
	}
}
