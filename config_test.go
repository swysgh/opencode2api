package main

import (
	"strings"
	"testing"
)

// TestDecodeConfigDefaults 空配置（什么都不写）→ 默认值全部正确（§12.2 第 1 条，逐字段断言）。
func TestDecodeConfigDefaults(t *testing.T) {
	cfg, err := decodeConfig(nil)
	if err != nil {
		t.Fatalf("空配置不应报错: %v", err)
	}

	// ---- server ----
	if cfg.Server.Listen != defaultListen {
		t.Fatalf("默认 server.listen = %q，期望 %q", cfg.Server.Listen, defaultListen)
	}
	if len(cfg.Server.APIKeys) != 0 {
		t.Fatalf("默认 server.api_keys 应为空，实际 %v", cfg.Server.APIKeys)
	}
	if cfg.Server.MaxBodyBytes == nil || *cfg.Server.MaxBodyBytes != defaultMaxBodyBytes {
		t.Fatalf("默认 server.max_body_bytes = %v", cfg.Server.MaxBodyBytes)
	}
	if cfg.Server.ShutdownTimeout.V != defaultShutdownTimeout {
		t.Fatalf("默认 server.shutdown_timeout = %s", cfg.Server.ShutdownTimeout.V)
	}
	if cfg.Server.LogLevel != "info" {
		t.Fatalf("默认 server.log_level = %q", cfg.Server.LogLevel)
	}
	if cfg.Server.LogFile != "" {
		t.Fatalf("默认 server.log_file = %q", cfg.Server.LogFile)
	}

	// ---- upstream ----
	u := cfg.Upstream
	if *u.BaseURL != defaultBaseURL {
		t.Fatalf("默认 upstream.base_url = %q", *u.BaseURL)
	}
	if u.ModelPrefix != defaultModelPrefix {
		t.Fatalf("默认 upstream.model_prefix = %q", u.ModelPrefix)
	}
	if len(u.Models) != 0 {
		t.Fatalf("默认 upstream.models 应为空")
	}
	if u.MatchRegex != defaultMatchRegex {
		t.Fatalf("默认 upstream.match_regex = %q", u.MatchRegex)
	}
	if len(u.ModelAliases) != 0 {
		t.Fatalf("默认 upstream.model_aliases 应为空")
	}
	if u.CatalogFile != defaultCatalogFile {
		t.Fatalf("默认 upstream.catalog_file = %q", u.CatalogFile)
	}
	if u.CatalogRefresh.V != defaultCatalogRefresh {
		t.Fatalf("默认 upstream.catalog_refresh = %s", u.CatalogRefresh.V)
	}
	if u.CatalogTimeout.V != defaultCatalogTimeout {
		t.Fatalf("默认 upstream.catalog_timeout = %s", u.CatalogTimeout.V)
	}
	if cfg.catalogViaProxy() {
		t.Fatal("默认 upstream.catalog_via_proxy 应为 false")
	}
	if !cfg.injectTools() {
		t.Fatal("默认 upstream.inject_tools 应为 true")
	}
	if !cfg.forceStream() {
		t.Fatal("默认 upstream.force_stream 应为 true")
	}
	if !cfg.restoreToolNames() {
		t.Fatal("默认 upstream.restore_tool_names 应为 true")
	}
	if u.NonStreamPolicy != nonStreamForce {
		t.Fatalf("默认 upstream.nonstream_policy = %q", u.NonStreamPolicy)
	}
	if u.UserAgent != defaultUserAgent {
		t.Fatalf("默认 upstream.user_agent = %q", u.UserAgent)
	}
	if u.ClientName != defaultClientName {
		t.Fatalf("默认 upstream.client_name = %q", u.ClientName)
	}
	if u.SessionTTL.V != defaultSessionTTL {
		t.Fatalf("默认 upstream.session_ttl = %s", u.SessionTTL.V)
	}
	if u.RequestTimeout.V != defaultRequestTimeout {
		t.Fatalf("默认 upstream.request_timeout = %s", u.RequestTimeout.V)
	}
	if u.FirstByteTimeout.V != defaultFirstByteTimeout {
		t.Fatalf("默认 upstream.first_byte_timeout = %s", u.FirstByteTimeout.V)
	}
	if cfg.ttl != defaultSessionTTL {
		t.Fatalf("派生 ttl = %s", cfg.ttl)
	}
	if !cfg.matchesUpstream("mimo-v2.6-flash-free") {
		t.Fatal("默认 match_regex 应命中 *-free")
	}

	// ---- proxy ----
	p := cfg.Proxy
	if len(p.Proxies) != 0 {
		t.Fatalf("默认 proxy.proxies 应为空")
	}
	if p.ProxyFile != "" {
		t.Fatalf("默认 proxy.proxy_file = %q", p.ProxyFile)
	}
	if p.Strategy != "round_robin" {
		t.Fatalf("默认 proxy.strategy = %q", p.Strategy)
	}
	if cfg.proxyMaxAttempts() != defaultProxyMaxAttempts {
		t.Fatalf("默认 proxy.max_attempts = %d", cfg.proxyMaxAttempts())
	}
	if p.Cooldown.Base.V != defaultCooldownBase {
		t.Fatalf("默认 proxy.cooldown.base = %s", p.Cooldown.Base.V)
	}
	if *p.Cooldown.Factor != defaultCooldownFactor {
		t.Fatalf("默认 proxy.cooldown.factor = %v", *p.Cooldown.Factor)
	}
	if p.Cooldown.Max.V != defaultCooldownMax {
		t.Fatalf("默认 proxy.cooldown.max = %s", p.Cooldown.Max.V)
	}
	if *p.Cooldown.Jitter != defaultCooldownJitter {
		t.Fatalf("默认 proxy.cooldown.jitter = %v", *p.Cooldown.Jitter)
	}
	if len(p.Cooldown.Statuses) != len(defaultCooldownStatuses) {
		t.Fatalf("默认 proxy.cooldown.statuses = %v", p.Cooldown.Statuses)
	}
}

// TestDecodeConfigMinimalYAMLKeepsDefaults 只写个别字段 → 其余保持默认（§12.2 第 1 条）。
func TestDecodeConfigMinimalYAMLKeepsDefaults(t *testing.T) {
	cfg, err := decodeConfig([]byte("server:\n  listen: \"127.0.0.1:18318\"\n"))
	if err != nil {
		t.Fatalf("部分覆盖应通过: %v", err)
	}
	if cfg.Server.Listen != "127.0.0.1:18318" {
		t.Fatalf("server.listen = %q", cfg.Server.Listen)
	}
	if *cfg.Upstream.BaseURL != defaultBaseURL {
		t.Fatal("未覆盖的字段应保持默认")
	}
	if !cfg.injectTools() {
		t.Fatal("inject_tools 应保持默认 true")
	}
	if cfg.Server.ShutdownTimeout.V != defaultShutdownTimeout {
		t.Fatal("shutdown_timeout 应保持默认")
	}
}

// TestDecodeConfigExplicitFalseZero 显式写 false / 0 → 覆盖默认值（§12.2 第 1 条）。
func TestDecodeConfigExplicitFalseZero(t *testing.T) {
	cfg, err := decodeConfig([]byte(`
upstream:
  inject_tools: false
  force_stream: false
  restore_tool_names: false
  catalog_via_proxy: false
  nonstream_policy: pass
proxy:
  strategy: random
  max_attempts: 0
  cooldown:
    factor: 1
    jitter: 0
    statuses: [429]
`))
	if err != nil {
		t.Fatalf("显式 false/0 应通过: %v", err)
	}
	if cfg.injectTools() {
		t.Fatal("inject_tools 应为 false")
	}
	if cfg.forceStream() {
		t.Fatal("force_stream 应为 false")
	}
	if cfg.restoreToolNames() {
		t.Fatal("restore_tool_names 应为 false")
	}
	if cfg.catalogViaProxy() {
		t.Fatal("catalog_via_proxy 应为 false")
	}
	if cfg.Upstream.NonStreamPolicy != nonStreamPass {
		t.Fatalf("nonstream_policy = %q", cfg.Upstream.NonStreamPolicy)
	}
	if cfg.Proxy.Strategy != "random" {
		t.Fatalf("strategy = %q", cfg.Proxy.Strategy)
	}
	// max_attempts: 0 → 视为 1（< 1 视为 1，§3.3）。
	if cfg.proxyMaxAttempts() != 1 {
		t.Fatalf("max_attempts: 0 应视为 1，实际 %d", cfg.proxyMaxAttempts())
	}
	if *cfg.Proxy.Cooldown.Factor != 1 {
		t.Fatalf("cooldown.factor = %v", *cfg.Proxy.Cooldown.Factor)
	}
	if *cfg.Proxy.Cooldown.Jitter != 0 {
		t.Fatalf("cooldown.jitter = %v", *cfg.Proxy.Cooldown.Jitter)
	}
	if len(cfg.Proxy.Cooldown.Statuses) != 1 || cfg.Proxy.Cooldown.Statuses[0] != 429 {
		t.Fatalf("cooldown.statuses = %v", cfg.Proxy.Cooldown.Statuses)
	}
}

// TestDecodeConfigValidationErrors 非法值 → 报错且信息含中文原因（§12.2 第 1 条）。
func TestDecodeConfigValidationErrors(t *testing.T) {
	cases := []struct {
		name    string
		yaml    string
		wantErr string
	}{
		{"非法 log_level", "server:\n  log_level: verbose\n", "server.log_level"},
		{"非法 nonstream_policy", "upstream:\n  nonstream_policy: whatever\n", "nonstream_policy"},
		{"非法 proxy.strategy", "proxy:\n  strategy: fastest\n", "strategy"},
		{"非法 base_url", "upstream:\n  base_url: ftp://x\n", "base_url"},
		{"base_url 空字符串", "upstream:\n  base_url: \"\"\n", "base_url"},
		{"listen 缺端口", "server:\n  listen: \"127.0.0.1\"\n", "listen"},
		{"listen 端口越界", "server:\n  listen: \"127.0.0.1:99999\"\n", "listen"},
		{"未知嵌套键", "upstream:\n  no_such_key: 1\n", "配置错误"},
		{"未知顶层键", "nope: 1\n", "配置错误"},
		{"非法时长格式", "server:\n  shutdown_timeout: 十秒\n", "解析失败"},
		{"max_body_bytes 非正", "server:\n  max_body_bytes: 0\n", "max_body_bytes"},
		{"shutdown_timeout 非正", "server:\n  shutdown_timeout: 0s\n", "shutdown_timeout"},
		{"session_ttl 非正", "upstream:\n  session_ttl: 0s\n", "session_ttl"},
		{"request_timeout 非正", "upstream:\n  request_timeout: 0s\n", "request_timeout"},
		{"first_byte_timeout 非正", "upstream:\n  first_byte_timeout: 0s\n", "first_byte_timeout"},
		{"catalog_refresh 非正", "upstream:\n  catalog_refresh: 0s\n", "catalog_refresh"},
		{"catalog_timeout 非正", "upstream:\n  catalog_timeout: 0s\n", "catalog_timeout"},
		{"UA 不含 opencode", "upstream:\n  user_agent: curl/8\n", "user_agent"},
		{"非法 match_regex", "upstream:\n  match_regex: \"([\"\n", "match_regex"},
		{"冷却 factor 太小", "proxy:\n  cooldown:\n    factor: 0.5\n", "factor"},
		{"冷却 jitter 越界", "proxy:\n  cooldown:\n    jitter: 2\n", "jitter"},
		{"冷却 statuses 非法", "proxy:\n  cooldown:\n    statuses: [99]\n", "statuses"},
		{"冷却 max 小于 base", "proxy:\n  cooldown:\n    base: 5m\n    max: 1m\n", "max"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := decodeConfig([]byte(tc.yaml))
			if err == nil {
				t.Fatalf("期望报错，实际通过")
			}
			// 信息必须含中文前缀「配置错误」（未知键 / 非法值都走这条路径）。
			if !strings.Contains(err.Error(), "配置错误") {
				t.Fatalf("错误信息应含中文前缀「配置错误」: %q", err.Error())
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("错误信息 %q 未包含 %q", err.Error(), tc.wantErr)
			}
		})
	}
}

// TestDecodeConfigAPIKeysNormalization api_keys 去空白、去空串、去重（§3.3）。
func TestDecodeConfigAPIKeysNormalization(t *testing.T) {
	cfg, err := decodeConfig([]byte("server:\n  api_keys: [\" sk-1 \", \"\", \"sk-1\", \"sk-2\", \"sk-2\"]\n"))
	if err != nil {
		t.Fatalf("配置: %v", err)
	}
	want := []string{"sk-1", "sk-2"}
	if len(cfg.Server.APIKeys) != len(want) {
		t.Fatalf("api_keys 归一化后 = %v，期望 %v", cfg.Server.APIKeys, want)
	}
	for i := range want {
		if cfg.Server.APIKeys[i] != want[i] {
			t.Fatalf("api_keys[%d] = %q，期望 %q", i, cfg.Server.APIKeys[i], want[i])
		}
	}
}

// TestDecodeConfigModelsAndAliases models 去重 + match_regex 显式空 = 关闭正则兜底。
func TestDecodeConfigModelsAndAliases(t *testing.T) {
	cfg, err := decodeConfig([]byte(`
upstream:
  models: ["mimo-v2.5-free", "mimo-v2.5-free", "  ", ""]
  match_regex: ""
  model_aliases:
    mimo-v2.5-free:
      - zen-mimo
      - zen-mimo
      - ""
`))
	if err != nil {
		t.Fatalf("配置: %v", err)
	}
	if len(cfg.Upstream.Models) != 1 || cfg.Upstream.Models[0] != "mimo-v2.5-free" {
		t.Fatalf("models 归一化后 = %v", cfg.Upstream.Models)
	}
	if !cfg.matchesUpstream("mimo-v2.5-free") {
		t.Fatal("精确名单应命中")
	}
	if cfg.matchesUpstream("anything-free") {
		t.Fatal("match_regex 显式空应关闭正则兜底，不该命中")
	}
	if len(cfg.Upstream.ModelAliases["mimo-v2.5-free"]) != 1 ||
		cfg.Upstream.ModelAliases["mimo-v2.5-free"][0] != "zen-mimo" {
		t.Fatalf("别名归一化后 = %v", cfg.Upstream.ModelAliases)
	}
}

// TestDecodeConfigNullBlocksDoNotPanic 回归测试：显式写 null 的块（YAML 会把指针块置 nil）
// 不能让 normalize 空指针 panic；应回退默认值（§3.3「不许 panic」）。
func TestDecodeConfigNullBlocksDoNotPanic(t *testing.T) {
	cases := []string{
		"server:\n  max_body_bytes: null\n",
		"server:\n  shutdown_timeout: null\n",
		"upstream:\n  catalog_refresh: null\n",
		"upstream:\n  catalog_timeout: null\n",
		"upstream:\n  session_ttl: null\n",
		"upstream:\n  request_timeout: null\n",
		"upstream:\n  first_byte_timeout: null\n",
		"proxy:\n  cooldown: null\n",
		"proxy:\n  cooldown:\n    base: null\n    max: null\n    factor: null\n    jitter: null\n",
	}
	for _, y := range cases {
		y := y
		t.Run(strings.TrimSpace(strings.ReplaceAll(y, "\n", "|")), func(t *testing.T) {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("配置 %q 触发 panic: %v", y, r)
				}
			}()
			cfg, err := decodeConfig([]byte(y))
			if err != nil {
				t.Fatalf("配置 %q 报错: %v", y, err)
			}
			if cfg.Server.MaxBodyBytes == nil || *cfg.Server.MaxBodyBytes != defaultMaxBodyBytes {
				t.Fatalf("配置 %q 后 max_body_bytes 应回退默认值", y)
			}
			if cfg.Server.ShutdownTimeout.V <= 0 {
				t.Fatalf("配置 %q 后 shutdown_timeout 应回退默认值", y)
			}
			if cfg.Upstream.SessionTTL.V <= 0 {
				t.Fatalf("配置 %q 后 session_ttl 应回退默认值", y)
			}
			if cfg.Upstream.RequestTimeout.V <= 0 {
				t.Fatalf("配置 %q 后 request_timeout 应回退默认值", y)
			}
			if cfg.Upstream.FirstByteTimeout.V <= 0 {
				t.Fatalf("配置 %q 后 first_byte_timeout 应回退默认值", y)
			}
			if cfg.Upstream.CatalogRefresh.V <= 0 {
				t.Fatalf("配置 %q 后 catalog_refresh 应回退默认值", y)
			}
			if cfg.Upstream.CatalogTimeout.V <= 0 {
				t.Fatalf("配置 %q 后 catalog_timeout 应回退默认值", y)
			}
			if cfg.Proxy.Cooldown.Base.V <= 0 {
				t.Fatalf("配置 %q 后 cooldown.base 应回退默认值", y)
			}
			if cfg.Proxy.Cooldown.Max.V < cfg.Proxy.Cooldown.Base.V {
				t.Fatalf("配置 %q 后 cooldown.max 应回退默认值", y)
			}
			if *cfg.Proxy.Cooldown.Factor < 1 || *cfg.Proxy.Cooldown.Jitter < 0 || *cfg.Proxy.Cooldown.Jitter > 1 {
				t.Fatalf("配置 %q 后 cooldown.factor/jitter 应回退默认值", y)
			}
		})
	}
}

// TestConfigSummaryMasksSensitive summary 分段 + api_keys/代理 URL 凭据脱敏（§3.4）。
func TestConfigSummaryMasksSensitive(t *testing.T) {
	cfg, err := decodeConfig([]byte(`
server:
  api_keys: ["sk-test-1234", "ab"]
proxy:
  proxies:
    - socks5://user:pass@10.0.0.2:1080
    - direct
`))
	if err != nil {
		t.Fatalf("配置: %v", err)
	}
	s := cfg.summary()
	for _, want := range []string{"[server]", "[upstream]", "[proxy]", "sk-t***", "***", "user:***@10.0.0.2:1080", "direct"} {
		if !strings.Contains(s, want) {
			t.Fatalf("summary 未包含 %q:\n%s", want, s)
		}
	}
	if strings.Contains(s, "sk-test-1234") {
		t.Fatal("summary 泄漏了完整 api_key")
	}
	if strings.Contains(s, ":pass@") {
		t.Fatal("summary 泄漏了代理密码")
	}
	// 值里有空格的应被 %q 包裹（base_url 无空格→不带引号；带空格的字符串→带引号）。
	if !strings.Contains(s, "base_url=") {
		t.Fatalf("summary 缺 base_url 行:\n%s", s)
	}
}
