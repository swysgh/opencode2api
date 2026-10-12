package main

import (
	"fmt"
	"io"
	"net"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// levelRank 把日志级别名映射到数值，便于按级别过滤（log.go 用，§4）。
var levelRank = map[string]int{"debug": 0, "info": 1, "warn": 2, "error": 3}

// 非流式请求的处理策略（§7）。
const (
	// nonStreamForce 强制把上游改成 stream:true，并把上游 SSE 聚合成一个 chat.completion。
	nonStreamForce = "force"
	// nonStreamReject 不发上游请求，直接 400 报错。
	nonStreamReject = "reject"
	// nonStreamPass 原样放过（上游状态码与 body 原样透出）。
	nonStreamPass = "pass"
)

// 默认的上游目录基址（公开接口，无需鉴权）。
const defaultBaseURL = "https://opencode.ai/zen/v1"

// defaultMatchRegex 兜住免费档上游 id：*-free 结尾、以及两个不带 free 的免费模型。
const defaultMatchRegex = `(-free$)|(^big-pickle$)|(^space-bunny)`

// defaultModelPrefix 是客户端调用名前缀（统一加 oc/）。
const defaultModelPrefix = "oc/"

// server 分组默认值（§3.2）。
const (
	defaultListen          = "127.0.0.1:8318"
	defaultMaxBodyBytes    = int64(67108864) // 64MiB
	defaultShutdownTimeout = 10 * time.Second
)

// upstream 分组默认值（§3.2）。
const (
	defaultUserAgent        = "opencode/1.18.31"
	defaultClientName       = "desktop"
	defaultCatalogFile      = "./data/catalog.json"
	defaultSessionTTL       = 30 * time.Minute
	defaultRequestTimeout   = 300 * time.Second
	defaultFirstByteTimeout = 60 * time.Second
	defaultCatalogRefresh   = 6 * time.Hour
	defaultCatalogTimeout   = 10 * time.Second
)

// 代理冷却默认值（§10）。
const (
	defaultCooldownBase     = 30 * time.Second
	defaultCooldownFactor   = 2.0
	defaultCooldownMax      = 10 * time.Minute
	defaultCooldownJitter   = 0.2
	defaultProxyMaxAttempts = 3
)

// defaultCooldownStatuses 命中即冷却该代理并换下一个（§10）。
var defaultCooldownStatuses = []int{403, 407, 429, 500, 502, 503, 504}

// proxyCooldownConfig 是代理冷却的退避参数（§3.2 / §10）。
type proxyCooldownConfig struct {
	Base     *durationValue `yaml:"base"`     // 默认 30s
	Factor   *float64       `yaml:"factor"`   // 默认 2.0
	Max      *durationValue `yaml:"max"`      // 默认 10m
	Jitter   *float64       `yaml:"jitter"`   // 默认 0.2
	Statuses []int          `yaml:"statuses"` // 默认 [403,407,429,500,502,503,504]
}

// durationValue 既能从 YAML 解析成 time.Duration，又能在默认值判定时区分
// "没写" 与 "显式写了 0"（§3.1 的零值坑）。
type durationValue struct {
	V   time.Duration
	Set bool
}

// UnmarshalYAML 支持字符串时长（如 "6h"）与数字（秒）。
func (d *durationValue) UnmarshalYAML(value *yaml.Node) error {
	d.Set = true
	var s string
	if err := value.Decode(&s); err == nil {
		if s == "" {
			d.V = 0
			return nil
		}
		v, err := time.ParseDuration(s)
		if err != nil {
			return fmt.Errorf("非法时长: %q", s)
		}
		d.V = v
		return nil
	}
	var n float64
	if err := value.Decode(&n); err != nil {
		return err
	}
	d.V = time.Duration(n * float64(time.Second))
	return nil
}

// value 返回解析后的值；没写时回退到 def。
func (d *durationValue) value(def time.Duration) time.Duration {
	if d == nil || !d.Set {
		return def
	}
	return d.V
}

func floatPtr(v float64) *float64 { return &v }
func boolPtr(v bool) *bool        { return &v }
func stringPtr(v string) *string  { return &v }

// config 是服务全部配置项，分 server / upstream / proxy 三组（§3.2）。
// 注意：「默认非零」的开关/时长一律用指针形态，以区分「没写」与「显式写了 false/0」。
type config struct {
	Server   serverConfig   `yaml:"server"`
	Upstream upstreamConfig `yaml:"upstream"`
	Proxy    proxyConfig    `yaml:"proxy"`

	// 归一化后的派生值（不是配置项，由 normalize 填充）。
	matchRe      *regexp.Regexp
	ttl          time.Duration
	modelSet     map[string]struct{}
	responsesSet map[string]struct{}
	excludeSet   map[string]struct{}
}

// serverConfig 是对外 HTTP 服务分组（§3.2）。
type serverConfig struct {
	Listen          string         `yaml:"listen"`           // 默认 "127.0.0.1:8318"
	APIKeys         []string       `yaml:"api_keys"`         // 默认 []（空 = 不鉴权）
	MaxBodyBytes    *int64         `yaml:"max_body_bytes"`   // 默认 67108864（64MiB）
	ShutdownTimeout *durationValue `yaml:"shutdown_timeout"` // 默认 10s
	LogLevel        string         `yaml:"log_level"`        // 默认 "info"
	LogFile         string         `yaml:"log_file"`         // 默认 ""（stdout）
}

// upstreamConfig 是上游相关分组（§3.2）。
type upstreamConfig struct {
	BaseURL          *string             `yaml:"base_url"`           // 默认 "https://opencode.ai/zen/v1"；显式空/非法 → 报错
	ModelPrefix      string              `yaml:"model_prefix"`       // 默认 "oc/"
	Models           []string            `yaml:"models"`             // 默认 []
	ResponsesModels  []string            `yaml:"responses_models"`   // 默认 []，只支持 /responses 的上游 id
	ExcludeModels    []string            `yaml:"exclude_models"`     // 默认 []，从暴露集合中剔除的上游 id
	MatchRegex       string              `yaml:"match_regex"`        // 默认 `(-free$)|(^big-pickle$)|(^space-bunny)`
	ModelAliases     map[string][]string `yaml:"model_aliases"`      // 默认 {}
	CatalogFile      string              `yaml:"catalog_file"`       // 默认 "./data/catalog.json"
	CatalogRefresh   *durationValue      `yaml:"catalog_refresh"`    // 默认 6h
	CatalogTimeout   *durationValue      `yaml:"catalog_timeout"`    // 默认 10s
	CatalogViaProxy  *bool               `yaml:"catalog_via_proxy"`  // 默认 false
	InjectTools      *bool               `yaml:"inject_tools"`       // 默认 true
	ForceStream      *bool               `yaml:"force_stream"`       // 默认 true
	NonStreamPolicy  string              `yaml:"nonstream_policy"`   // 默认 "force"
	RestoreToolNames *bool               `yaml:"restore_tool_names"` // 默认 true
	UserAgent        string              `yaml:"user_agent"`         // 默认 "opencode/1.18.31"
	ClientName       string              `yaml:"client_name"`        // 默认 "desktop"
	SessionTTL       *durationValue      `yaml:"session_ttl"`        // 默认 30m
	RequestTimeout   *durationValue      `yaml:"request_timeout"`    // 默认 300s
	FirstByteTimeout *durationValue      `yaml:"first_byte_timeout"` // 默认 60s
}

// proxyConfig 是出口代理池分组（§3.2）。
type proxyConfig struct {
	Proxies     []string            `yaml:"proxies"`      // 默认 []
	ProxyFile   string              `yaml:"proxy_file"`   // 默认 ""
	Strategy    string              `yaml:"strategy"`     // 默认 "round_robin"
	MaxAttempts *int                `yaml:"max_attempts"` // 默认 3
	Cooldown    proxyCooldownConfig `yaml:"cooldown"`     // 见 proxyCooldownConfig
}

// defaultConfig 返回所有字段填好默认值的配置（§3.2）。
func defaultConfig() config {
	mb := defaultMaxBodyBytes
	attempts := defaultProxyMaxAttempts
	return config{
		Server: serverConfig{
			Listen:          defaultListen,
			APIKeys:         nil,
			MaxBodyBytes:    &mb,
			ShutdownTimeout: &durationValue{V: defaultShutdownTimeout, Set: false},
			LogLevel:        "info",
			LogFile:         "",
		},
		Upstream: upstreamConfig{
			BaseURL:          stringPtr(defaultBaseURL),
			ModelPrefix:      defaultModelPrefix,
			Models:           nil,
			MatchRegex:       defaultMatchRegex,
			ModelAliases:     map[string][]string{},
			CatalogFile:      defaultCatalogFile,
			CatalogRefresh:   &durationValue{V: defaultCatalogRefresh, Set: false},
			CatalogTimeout:   &durationValue{V: defaultCatalogTimeout, Set: false},
			CatalogViaProxy:  boolPtr(false),
			InjectTools:      boolPtr(true),
			ForceStream:      boolPtr(true),
			NonStreamPolicy:  nonStreamForce,
			RestoreToolNames: boolPtr(true),
			UserAgent:        defaultUserAgent,
			ClientName:       defaultClientName,
			SessionTTL:       &durationValue{V: defaultSessionTTL, Set: false},
			RequestTimeout:   &durationValue{V: defaultRequestTimeout, Set: false},
			FirstByteTimeout: &durationValue{V: defaultFirstByteTimeout, Set: false},
		},
		Proxy: proxyConfig{
			Proxies:     nil,
			ProxyFile:   "",
			Strategy:    "round_robin",
			MaxAttempts: &attempts,
			Cooldown: proxyCooldownConfig{
				Base:     &durationValue{V: defaultCooldownBase, Set: false},
				Factor:   floatPtr(defaultCooldownFactor),
				Max:      &durationValue{V: defaultCooldownMax, Set: false},
				Jitter:   floatPtr(defaultCooldownJitter),
				Statuses: append([]int(nil), defaultCooldownStatuses...),
			},
		},
	}
}

// decodeConfig 解析 YAML 配置（§3.1）。
// 未知键（拼错）→ 中文报错（KnownFields(true)）；缺失字段保持默认值；
// 显式非法值 → 返回带中文原因的 error（normalize）。
func decodeConfig(raw []byte) (config, error) {
	cfg := defaultConfig()
	if len(raw) > 0 {
		dec := yaml.NewDecoder(strings.NewReader(string(raw)))
		dec.KnownFields(true)
		if err := dec.Decode(&cfg); err != nil {
			if err == io.EOF { // 空文件 / 只有注释：与没写一样，全默认。
				return cfg, nil
			}
			return config{}, fmt.Errorf("配置错误: 解析失败: %w", err)
		}
	}
	if err := cfg.normalize(); err != nil {
		return config{}, err
	}
	return cfg, nil
}

// injectTools / forceStream / restoreToolNames / catalogViaProxy 区分「没写」与「显式 false」。
func (c *config) injectTools() bool { return c.Upstream.InjectTools == nil || *c.Upstream.InjectTools }
func (c *config) forceStream() bool { return c.Upstream.ForceStream == nil || *c.Upstream.ForceStream }
func (c *config) restoreToolNames() bool {
	return c.Upstream.RestoreToolNames == nil || *c.Upstream.RestoreToolNames
}
func (c *config) catalogViaProxy() bool {
	return c.Upstream.CatalogViaProxy != nil && *c.Upstream.CatalogViaProxy
}

// baseURL 返回归一化后的生效上游基址（normalize 保证非 nil）。
func (c *config) baseURL() string { return *c.Upstream.BaseURL }

// proxyMaxAttempts 返回单请求最多尝试几个代理（<1 视为 1，§3.3）。
func (c *config) proxyMaxAttempts() int {
	if c.Proxy.MaxAttempts == nil {
		return defaultProxyMaxAttempts
	}
	if *c.Proxy.MaxAttempts < 1 {
		return 1
	}
	return *c.Proxy.MaxAttempts
}

// normalize 校验并归一化配置，非法时返回带中文原因的 error（§3.3，fail fast）。
func (c *config) normalize() error {
	// ---- server ----
	s := &c.Server
	s.Listen = strings.TrimSpace(s.Listen)
	if s.Listen == "" {
		s.Listen = defaultListen
	}
	if _, portStr, err := net.SplitHostPort(s.Listen); err != nil {
		return fmt.Errorf("配置错误: server.listen: 非法监听地址 %q（应为 host:port）", s.Listen)
	} else if port, perr := strconv.Atoi(portStr); perr != nil || port < 1 || port > 65535 {
		return fmt.Errorf("配置错误: server.listen: 非法端口 %q（应在 1~65535）", portStr)
	}

	// api_keys：去空白、去空串、去重（空 → 启动时由 main warn：/v1 不鉴权）。
	keys := make([]string, 0, len(s.APIKeys))
	seenKey := map[string]struct{}{}
	for _, k := range s.APIKeys {
		k = strings.TrimSpace(k)
		if k == "" {
			continue
		}
		if _, dup := seenKey[k]; dup {
			continue
		}
		seenKey[k] = struct{}{}
		keys = append(keys, k)
	}
	s.APIKeys = keys

	if s.MaxBodyBytes == nil { // 显式 null → 回退默认
		mb := defaultMaxBodyBytes
		s.MaxBodyBytes = &mb
	}
	if *s.MaxBodyBytes <= 0 {
		return fmt.Errorf("配置错误: server.max_body_bytes: 必须为正数，收到 %d", *s.MaxBodyBytes)
	}

	// 显式写 `xxx: null` 会让指针变 nil，下面统一先补齐成零值 durationValue
	// 再判错（不 panic，配置错误应报错而不是崩，照抄 cpa-zen 的补齐写法）。
	if s.ShutdownTimeout == nil {
		s.ShutdownTimeout = &durationValue{}
	}
	st := s.ShutdownTimeout.value(defaultShutdownTimeout)
	if st <= 0 {
		return fmt.Errorf("配置错误: server.shutdown_timeout: 必须是正数时长，收到 %s", s.ShutdownTimeout.V)
	}
	s.ShutdownTimeout.V = st

	s.LogLevel = strings.ToLower(strings.TrimSpace(s.LogLevel))
	if s.LogLevel == "" {
		s.LogLevel = "info"
	}
	if _, ok := levelRank[s.LogLevel]; !ok {
		return fmt.Errorf("配置错误: server.log_level: 只支持 debug|info|warn|error，收到 %q", s.LogLevel)
	}
	s.LogFile = strings.TrimSpace(s.LogFile)

	// ---- upstream ----
	u := &c.Upstream
	// base_url 用指针区分「没写」（→ 默认）与「显式写了空/非法」（→ 报错，fail fast）。
	if u.BaseURL == nil {
		u.BaseURL = stringPtr(defaultBaseURL)
	}
	bu := strings.TrimSpace(*u.BaseURL)
	if !strings.HasPrefix(bu, "http://") && !strings.HasPrefix(bu, "https://") {
		return fmt.Errorf("配置错误: upstream.base_url: 必须以 http:// 或 https:// 开头，收到 %q", bu)
	}
	*u.BaseURL = bu

	u.ModelPrefix = strings.TrimSpace(u.ModelPrefix)
	if u.ModelPrefix == "" {
		u.ModelPrefix = defaultModelPrefix
	}

	u.MatchRegex = strings.TrimSpace(u.MatchRegex)
	if u.MatchRegex != "" {
		re, errRe := regexp.Compile(u.MatchRegex)
		if errRe != nil {
			return fmt.Errorf("配置错误: upstream.match_regex: 不是合法的正则: %v", errRe)
		}
		c.matchRe = re
	} else {
		c.matchRe = nil // 显式置空 = 关掉正则兜底，只靠 models 显式名单
	}

	// models：去空白、去空项、去重、保持稳定顺序。
	seen := map[string]struct{}{}
	out := make([]string, 0, len(u.Models))
	for _, m := range u.Models {
		m = strings.TrimSpace(m)
		if m == "" {
			continue
		}
		if _, dup := seen[m]; dup {
			continue
		}
		seen[m] = struct{}{}
		out = append(out, m)
	}
	u.Models = out
	c.modelSet = seen

	// responses_models / exclude_models：去空白、空项报错、去重保序。
	rm, rset, errRM := normalizeUpstreamIDList(u.ResponsesModels, "upstream.responses_models")
	if errRM != nil {
		return errRM
	}
	u.ResponsesModels = rm
	c.responsesSet = rset
	em, eset, errEM := normalizeUpstreamIDList(u.ExcludeModels, "upstream.exclude_models")
	if errEM != nil {
		return errEM
	}
	u.ExcludeModels = em
	c.excludeSet = eset

	// 别名表归一化：去空白、去空项、去重。
	if u.ModelAliases == nil {
		u.ModelAliases = map[string][]string{}
	}
	for id, names := range u.ModelAliases {
		clean := make([]string, 0, len(names))
		ns := map[string]struct{}{}
		for _, n := range names {
			n = strings.TrimSpace(n)
			if n == "" {
				continue
			}
			if _, dup := ns[n]; dup {
				continue
			}
			ns[n] = struct{}{}
			clean = append(clean, n)
		}
		if len(clean) == 0 {
			delete(u.ModelAliases, id)
		} else {
			u.ModelAliases[id] = clean
		}
	}

	if u.CatalogFile == "" {
		u.CatalogFile = defaultCatalogFile
	}

	if u.CatalogRefresh == nil {
		u.CatalogRefresh = &durationValue{}
	}
	cr := u.CatalogRefresh.value(defaultCatalogRefresh)
	if cr <= 0 {
		return fmt.Errorf("配置错误: upstream.catalog_refresh: 必须是正数时长，收到 %s", u.CatalogRefresh.V)
	}
	u.CatalogRefresh.V = cr

	if u.CatalogTimeout == nil {
		u.CatalogTimeout = &durationValue{}
	}
	cto := u.CatalogTimeout.value(defaultCatalogTimeout)
	if cto <= 0 {
		return fmt.Errorf("配置错误: upstream.catalog_timeout: 必须是正数时长，收到 %s", u.CatalogTimeout.V)
	}
	u.CatalogTimeout.V = cto

	if u.CatalogViaProxy == nil {
		u.CatalogViaProxy = boolPtr(false)
	}
	if u.InjectTools == nil {
		u.InjectTools = boolPtr(true)
	}
	if u.ForceStream == nil {
		u.ForceStream = boolPtr(true)
	}
	if u.RestoreToolNames == nil {
		u.RestoreToolNames = boolPtr(true)
	}

	u.NonStreamPolicy = strings.ToLower(strings.TrimSpace(u.NonStreamPolicy))
	if u.NonStreamPolicy == "" {
		u.NonStreamPolicy = nonStreamForce
	}
	switch u.NonStreamPolicy {
	case nonStreamForce, nonStreamReject, nonStreamPass:
	default:
		return fmt.Errorf("配置错误: upstream.nonstream_policy: 只支持 force|reject|pass，收到 %q", u.NonStreamPolicy)
	}

	u.UserAgent = strings.TrimSpace(u.UserAgent)
	if u.UserAgent == "" {
		u.UserAgent = defaultUserAgent
	}
	if !strings.Contains(strings.ToLower(u.UserAgent), "opencode/") {
		return fmt.Errorf("配置错误: upstream.user_agent: 必须包含 opencode/<版本>，收到 %q（上游 Cloudflare 会拦掉其它 UA）", u.UserAgent)
	}

	u.ClientName = strings.TrimSpace(u.ClientName)
	if u.ClientName == "" {
		u.ClientName = defaultClientName
	}

	if u.SessionTTL == nil {
		u.SessionTTL = &durationValue{}
	}
	ttl := u.SessionTTL.value(defaultSessionTTL)
	if ttl <= 0 {
		return fmt.Errorf("配置错误: upstream.session_ttl: 必须是正数时长，收到 %s", u.SessionTTL.V)
	}
	u.SessionTTL.V = ttl
	c.ttl = ttl

	if u.RequestTimeout == nil {
		u.RequestTimeout = &durationValue{}
	}
	rt := u.RequestTimeout.value(defaultRequestTimeout)
	if rt <= 0 {
		return fmt.Errorf("配置错误: upstream.request_timeout: 必须是正数时长，收到 %s", u.RequestTimeout.V)
	}
	u.RequestTimeout.V = rt

	if u.FirstByteTimeout == nil {
		u.FirstByteTimeout = &durationValue{}
	}
	fbt := u.FirstByteTimeout.value(defaultFirstByteTimeout)
	if fbt <= 0 {
		return fmt.Errorf("配置错误: upstream.first_byte_timeout: 必须是正数时长，收到 %s", u.FirstByteTimeout.V)
	}
	u.FirstByteTimeout.V = fbt

	// ---- proxy ----
	p := &c.Proxy
	p.Strategy = strings.ToLower(strings.TrimSpace(p.Strategy))
	if p.Strategy == "" {
		p.Strategy = "round_robin"
	}
	switch p.Strategy {
	case "round_robin", "random", "least_inflight":
	default:
		return fmt.Errorf("配置错误: proxy.strategy: 只支持 round_robin|random|least_inflight，收到 %q", p.Strategy)
	}
	if p.MaxAttempts == nil {
		attempts := defaultProxyMaxAttempts
		p.MaxAttempts = &attempts
	}
	// max_attempts < 1 → 视为 1（proxyMaxAttempts，不报错）。
	p.ProxyFile = strings.TrimSpace(p.ProxyFile)

	// 代理冷却参数校验（§3.3）。
	cd := &p.Cooldown
	if cd.Base == nil {
		cd.Base = &durationValue{}
	}
	cd.Base.V = cd.Base.value(defaultCooldownBase)
	if cd.Base.V <= 0 {
		return fmt.Errorf("配置错误: proxy.cooldown.base: 必须为正，收到 %s", cd.Base.V)
	}
	if cd.Factor == nil {
		cd.Factor = floatPtr(defaultCooldownFactor)
	}
	if *cd.Factor < 1 {
		return fmt.Errorf("配置错误: proxy.cooldown.factor: 必须 ≥ 1，收到 %v", *cd.Factor)
	}
	if cd.Max == nil {
		cd.Max = &durationValue{}
	}
	cd.Max.V = cd.Max.value(defaultCooldownMax)
	if cd.Max.V <= 0 || cd.Max.V < cd.Base.V {
		return fmt.Errorf("配置错误: proxy.cooldown.max: 必须为正且 ≥ base（%s），收到 %s", cd.Base.V, cd.Max.V)
	}
	if cd.Jitter == nil {
		cd.Jitter = floatPtr(defaultCooldownJitter)
	}
	if *cd.Jitter < 0 || *cd.Jitter > 1 {
		return fmt.Errorf("配置错误: proxy.cooldown.jitter: 必须在 0~1，收到 %v", *cd.Jitter)
	}
	if len(cd.Statuses) == 0 {
		cd.Statuses = append([]int(nil), defaultCooldownStatuses...)
	}
	for _, st := range cd.Statuses {
		if st < 100 || st > 599 {
			return fmt.Errorf("配置错误: proxy.cooldown.statuses: 每项必须在 100~599，收到 %d", st)
		}
	}
	return nil
}

// normalizeUpstreamIDList 归一化一个上游 id 列表：去空白、空项报错（带字段名与下标）、
// 去重保序（保留首次出现的顺序），并返回查找集合。
func normalizeUpstreamIDList(items []string, field string) ([]string, map[string]struct{}, error) {
	out := make([]string, 0, len(items))
	set := map[string]struct{}{}
	for i, raw := range items {
		v := strings.TrimSpace(raw)
		if v == "" {
			return nil, nil, fmt.Errorf("配置错误: %s[%d]: 不能为空", field, i)
		}
		if _, dup := set[v]; dup {
			continue
		}
		set[v] = struct{}{}
		out = append(out, v)
	}
	return out, set, nil
}

// isResponsesModel 判断上游 id 是否必须走 /responses 协议。
func (c *config) isResponsesModel(upstreamID string) bool {
	if upstreamID == "" {
		return false
	}
	_, ok := c.responsesSet[upstreamID]
	return ok
}

// isExcludedModel 判断上游 id 是否被显式排除。
func (c *config) isExcludedModel(upstreamID string) bool {
	if upstreamID == "" {
		return false
	}
	_, ok := c.excludeSet[upstreamID]
	return ok
}

// matchesUpstream 判断一个上游 id 是否需要暴露（§5.1）：
// 命中 models 精确名单，或命中 match_regex；命中 excludeSet 一律不暴露。
func (c *config) matchesUpstream(id string) bool {
	if id == "" {
		return false
	}
	if _, ok := c.modelSet[id]; ok {
		if _, excluded := c.excludeSet[id]; excluded {
			return false
		}
		return true
	}
	if c.matchRe != nil && c.matchRe.MatchString(id) {
		if _, excluded := c.excludeSet[id]; excluded {
			return false
		}
		return true
	}
	return false
}

// maskAPIKey 脱敏 api_key：只留前 4 位 + ***（不足 4 位全打 ***，§3.4）。
func maskAPIKey(k string) string {
	if len(k) < 4 {
		return "***"
	}
	return k[:4] + "***"
}

// maskProxyURL 把代理 URL 里的凭据脱敏成 user:***@（§3.4；复用 proxy.go 的 maskUser）。
func maskProxyURL(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return s
	}
	switch strings.ToLower(s) {
	case "direct", "none", "off":
		return "direct"
	}
	u, err := url.Parse(s)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return s
	}
	return u.Scheme + "://" + maskUser(u) + u.Host
}

// summary 返回生效配置的多行摘要（main 在 logger 就绪后以 info 打印，§3.4）：
// 一行一个 key=value，分段 [server] [upstream] [proxy]，值里有空格用 %q，
// api_keys 与代理 URL 凭据脱敏。
func (c *config) summary() string {
	maskedKeys := make([]string, 0, len(c.Server.APIKeys))
	for _, k := range c.Server.APIKeys {
		maskedKeys = append(maskedKeys, maskAPIKey(k))
	}
	maskedProxies := make([]string, 0, len(c.Proxy.Proxies))
	for _, p := range c.Proxy.Proxies {
		maskedProxies = append(maskedProxies, maskProxyURL(p))
	}
	models := append([]string(nil), c.Upstream.Models...)
	sort.Strings(models)
	responsesModels := append([]string(nil), c.Upstream.ResponsesModels...)
	sort.Strings(responsesModels)
	excludeModels := append([]string(nil), c.Upstream.ExcludeModels...)
	sort.Strings(excludeModels)
	aliases := make([]string, 0, len(c.Upstream.ModelAliases))
	for id, names := range c.Upstream.ModelAliases {
		aliases = append(aliases, fmt.Sprintf("%s→[%s]", id, strings.Join(names, ",")))
	}
	sort.Strings(aliases)
	statuses := make([]string, 0, len(c.Proxy.Cooldown.Statuses))
	for _, st := range c.Proxy.Cooldown.Statuses {
		statuses = append(statuses, strconv.Itoa(st))
	}

	var b strings.Builder
	b.WriteString("[server]\n")
	fmt.Fprintf(&b, "listen=%q\n", c.Server.Listen)
	fmt.Fprintf(&b, "api_keys=%d[%s]\n", len(maskedKeys), strings.Join(maskedKeys, ","))
	fmt.Fprintf(&b, "max_body_bytes=%d\n", *c.Server.MaxBodyBytes)
	fmt.Fprintf(&b, "shutdown_timeout=%s\n", c.Server.ShutdownTimeout.V)
	fmt.Fprintf(&b, "log_level=%s\n", c.Server.LogLevel)
	fmt.Fprintf(&b, "log_file=%q\n", c.Server.LogFile)

	b.WriteString("[upstream]\n")
	fmt.Fprintf(&b, "base_url=%q\n", *c.Upstream.BaseURL)
	fmt.Fprintf(&b, "model_prefix=%q\n", c.Upstream.ModelPrefix)
	fmt.Fprintf(&b, "models=%d[%s]\n", len(models), strings.Join(models, ","))
	fmt.Fprintf(&b, "responses_models=%d[%s]\n", len(responsesModels), strings.Join(responsesModels, ","))
	fmt.Fprintf(&b, "exclude_models=%d[%s]\n", len(excludeModels), strings.Join(excludeModels, ","))
	fmt.Fprintf(&b, "match_regex=%q\n", c.Upstream.MatchRegex)
	fmt.Fprintf(&b, "model_aliases=%d[%s]\n", len(aliases), strings.Join(aliases, ";"))
	fmt.Fprintf(&b, "catalog_file=%q\n", c.Upstream.CatalogFile)
	fmt.Fprintf(&b, "catalog_refresh=%s\n", c.Upstream.CatalogRefresh.V)
	fmt.Fprintf(&b, "catalog_timeout=%s\n", c.Upstream.CatalogTimeout.V)
	fmt.Fprintf(&b, "catalog_via_proxy=%v\n", c.catalogViaProxy())
	fmt.Fprintf(&b, "inject_tools=%v\n", c.injectTools())
	fmt.Fprintf(&b, "force_stream=%v\n", c.forceStream())
	fmt.Fprintf(&b, "nonstream_policy=%s\n", c.Upstream.NonStreamPolicy)
	fmt.Fprintf(&b, "restore_tool_names=%v\n", c.restoreToolNames())
	fmt.Fprintf(&b, "user_agent=%q\n", c.Upstream.UserAgent)
	fmt.Fprintf(&b, "client_name=%q\n", c.Upstream.ClientName)
	fmt.Fprintf(&b, "session_ttl=%s\n", c.Upstream.SessionTTL.V)
	fmt.Fprintf(&b, "request_timeout=%s\n", c.Upstream.RequestTimeout.V)
	fmt.Fprintf(&b, "first_byte_timeout=%s\n", c.Upstream.FirstByteTimeout.V)

	b.WriteString("[proxy]\n")
	fmt.Fprintf(&b, "proxies=%d[%s]\n", len(maskedProxies), strings.Join(maskedProxies, ","))
	fmt.Fprintf(&b, "proxy_file=%q\n", c.Proxy.ProxyFile)
	fmt.Fprintf(&b, "strategy=%s\n", c.Proxy.Strategy)
	fmt.Fprintf(&b, "max_attempts=%d\n", c.proxyMaxAttempts())
	fmt.Fprintf(&b, "cooldown.base=%s\n", c.Proxy.Cooldown.Base.V)
	fmt.Fprintf(&b, "cooldown.factor=%v\n", *c.Proxy.Cooldown.Factor)
	fmt.Fprintf(&b, "cooldown.max=%s\n", c.Proxy.Cooldown.Max.V)
	fmt.Fprintf(&b, "cooldown.jitter=%v\n", *c.Proxy.Cooldown.Jitter)
	fmt.Fprintf(&b, "cooldown.statuses=%s\n", strings.Join(statuses, ","))

	return strings.TrimRight(b.String(), "\n")
}
