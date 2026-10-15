package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// miniAPIJSON 构造一个缩小的 models.opencode.ai/api.json 形状（opencode 段）。
func miniAPIJSON() string {
	return `{
  "other-provider": {"id": "other", "npm": "@ai-sdk/openai-compatible", "models": {"x": {"id": "x"}}},
  "opencode": {
    "id": "opencode",
    "npm": "@ai-sdk/openai-compatible",
    "api": "https://opencode.ai/inference/v1",
    "models": {
      "big-pickle":  {"id": "big-pickle", "cost": {"input": 0, "output": 0}},
      "muse-free":   {"id": "muse-free", "provider": {"npm": "@ai-sdk/openai"}, "cost": {"input": 0, "output": 0}},
      "claude-free": {"id": "claude-free", "provider": {"npm": "@ai-sdk/anthropic"}, "cost": {"input": 0, "output": 0}},
      "old-free":    {"id": "old-free", "status": "deprecated", "cost": {"input": 0, "output": 0}},
      "paid":        {"id": "paid", "cost": {"input": 3, "output": 15}}
    }
  }
}`
}

func TestProtocolFromSDK(t *testing.T) {
	cases := map[string]string{
		"@ai-sdk/openai-compatible": "chat",
		"@ai-sdk/openai":            "responses",
		"@ai-sdk/anthropic":         "anthropic",
		"":                          "",
		"@ai-sdk/amazon-bedrock":    "",
	}
	for in, want := range cases {
		if got := protocolFromSDK(in); got != want {
			t.Errorf("protocolFromSDK(%q) = %q，期望 %q", in, got, want)
		}
	}
}

func TestMetadataModelIsFree(t *testing.T) {
	zero, three := 0.0, 3.0
	if !(metadataModel{CostInput: &zero, CostOutput: &zero}).isFree() {
		t.Error("cost 0/0 应判免费")
	}
	if (metadataModel{Status: "deprecated", CostInput: &zero, CostOutput: &zero}).isFree() {
		t.Error("deprecated 不应判免费")
	}
	if (metadataModel{CostInput: &zero, CostOutput: &three}).isFree() {
		t.Error("output 收费不应判免费")
	}
	if (metadataModel{}).isFree() {
		t.Error("缺 cost 视为未知，不应判免费")
	}
}

func TestMetadataIngestAndDerivedTables(t *testing.T) {
	m := loadMetadata("")
	var providers map[string]metadataProvider
	if err := json.Unmarshal([]byte(miniAPIJSON()), &providers); err != nil {
		t.Fatalf("解析 miniAPIJSON 失败: %v", err)
	}
	m.ingest(providers["opencode"])

	if len(m.models) != 5 {
		t.Fatalf("models 数 = %d，期望 5", len(m.models))
	}
	// 协议：模型级覆盖 provider 级；未知 SDK 不进协议表。
	if got := m.protocolOf("big-pickle"); got != "chat" {
		t.Errorf("big-pickle 协议 = %q，期望 chat", got)
	}
	if got := m.protocolOf("muse-free"); got != "responses" {
		t.Errorf("muse-free 协议 = %q，期望 responses", got)
	}
	if got := m.protocolOf("claude-free"); got != "anthropic" {
		t.Errorf("claude-free 协议 = %q，期望 anthropic", got)
	}
	// provider 级默认 npm 会覆盖所有未显式声明 SDK 的模型（paid → chat）。
	if got := m.protocolOf("paid"); got != "chat" {
		t.Errorf("paid 协议 = %q，期望 chat（继承 provider 级默认）", got)
	}
	// 免费档判定。
	if !m.metadataFreeActive("big-pickle") {
		t.Error("big-pickle 应判免费可服务")
	}
	if m.metadataFreeActive("claude-free") {
		t.Error("anthropic 协议不应判可服务（无 /messages 通道）")
	}
	if m.metadataFreeActive("old-free") {
		t.Error("deprecated 不应判免费")
	}
	if m.metadataFreeActive("paid") {
		t.Error("付费模型不应判免费")
	}
	if m.metadataFreeActive("nonexistent") {
		t.Error("未知模型不应判免费")
	}
	if !m.hasData() {
		t.Error("ingest 后 hasData 应为 true")
	}
}

func TestMetadataCacheRoundtrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "metadata.json")
	m := loadMetadata("")
	var providers map[string]metadataProvider
	if err := json.Unmarshal([]byte(miniAPIJSON()), &providers); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	m.path = path
	m.fetchedAt = time.Unix(1700000000, 0)
	m.ingest(providers["opencode"])
	// 手工落盘（复用 refresh 的写盘形状）。
	shape := metadataCacheShape{FetchedAt: m.fetchedAt.Unix()}
	// ingest 只填充派生表；写盘用原始 provider 段。
	shape.Provider = providers["opencode"]
	b, err := json.Marshal(shape)
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatalf("写缓存失败: %v", err)
	}

	loaded := loadMetadata(path)
	if len(loaded.models) != 5 {
		t.Fatalf("重载 models 数 = %d，期望 5", len(loaded.models))
	}
	if loaded.protocolOf("muse-free") != "responses" {
		t.Fatalf("重载后 muse-free 协议 = %q，期望 responses", loaded.protocolOf("muse-free"))
	}
	if loaded.age() < 24*time.Hour {
		t.Fatalf("重载后 age 异常（fetchedAt=2023，应远大于 24h）: %v", loaded.age())
	}
}

func TestMetadataRefreshViaHTTP(t *testing.T) {
	var mu sync.Mutex
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits++
		mu.Unlock()
		if r.URL.Path != "/api.json" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(miniAPIJSON()))
	}))
	defer srv.Close()

	dir := t.TempDir()
	m := loadMetadata(filepath.Join(dir, "metadata.json"))
	if err := m.refresh(context.Background(), srv.URL+"/api.json", 5*time.Second, nil); err != nil {
		t.Fatalf("refresh 失败: %v", err)
	}
	if len(m.models) != 5 {
		t.Fatalf("refresh 后 models = %d，期望 5", len(m.models))
	}
	// 落盘验证。
	loaded := loadMetadata(m.path)
	if len(loaded.models) != 5 {
		t.Fatalf("落盘重载 models = %d，期望 5", len(loaded.models))
	}
	// 404 → 报错且原数据保留。
	m2 := loadMetadata(filepath.Join(dir, "m2.json"))
	if err := m2.refresh(context.Background(), srv.URL+"/nope", 5*time.Second, nil); err == nil {
		t.Fatal("对 404 路径 refresh 应报错")
	}
	if len(m2.models) != 0 {
		t.Fatalf("失败刷新不应污染缓存，models = %d", len(m2.models))
	}
	// 响应里没有 opencode 段 → 报错。
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"foo":{"npm":"@ai-sdk/openai","models":{}}}`))
	}))
	defer srv2.Close()
	if err := m2.refresh(context.Background(), srv2.URL, 5*time.Second, nil); err == nil {
		t.Fatal("缺 opencode 段应报错")
	}
}

// writeCatalogFile 写一个最小目录缓存文件，供 loadCatalog 读。
func writeCatalogFile(t *testing.T, ids ...string) string {
	t.Helper()
	type entry struct {
		ID string `json:"id"`
	}
	data := struct {
		Object string  `json:"object"`
		Data   []entry `json:"data"`
	}{Object: "list"}
	for _, id := range ids {
		data.Data = append(data.Data, entry{ID: id})
	}
	b, err := json.Marshal(data)
	if err != nil {
		t.Fatalf("序列化目录失败: %v", err)
	}
	path := filepath.Join(t.TempDir(), "catalog.json")
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatalf("写目录失败: %v", err)
	}
	return path
}

func TestExposedFromCatalogWithMetadata(t *testing.T) {
	cat := loadCatalog(writeCatalogFile(t,
		"big-pickle", "muse-free", "claude-free", "old-free", "paid", "brand-new-free"))
	m := loadMetadata("")
	var providers map[string]metadataProvider
	if err := json.Unmarshal([]byte(miniAPIJSON()), &providers); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	m.ingest(providers["opencode"])

	cfg, err := decodeConfig([]byte("upstream:\n  models: []\n"))
	if err != nil {
		t.Fatalf("decodeConfig 失败: %v", err)
	}
	got := exposedFromCatalog(&cfg, cat, m)
	want := []string{"big-pickle", "brand-new-free", "muse-free"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("暴露集合 = %v，期望 %v（deprecated/anthropic/付费 剔除，未知 id 走正则兜底）", got, want)
	}

	// 无元数据 → 纯正则兜底（老行为， deprecated 也会暴露）。
	got = exposedFromCatalog(&cfg, cat, nil)
	want = []string{"big-pickle", "brand-new-free", "claude-free", "muse-free", "old-free"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("无元数据暴露集合 = %v，期望 %v", got, want)
	}

	// 显式 models 不受元数据约束（人工兜底通道），但仍受 exclude 约束。
	cfg2, err := decodeConfig([]byte("upstream:\n  models: [\"paid\", \"old-free\"]\n  exclude_models: [\"old-free\"]\n"))
	if err != nil {
		t.Fatalf("decodeConfig 失败: %v", err)
	}
	got = exposedFromCatalog(&cfg2, cat, m)
	if !strListContains(got, "paid") {
		t.Fatalf("显式 models 应无条件暴露（除非 exclude）：got=%v", got)
	}
	if strListContains(got, "old-free") {
		t.Fatalf("exclude_models 应挡住显式 models：got=%v", got)
	}
}

func strListContains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// TestServerMetadataProtocolRouting 端到端：没有 responses_models 配置时，
// 元数据声明 responses 协议的模型自动走 /responses 上游，chat 模型走 /chat/completions。
func TestServerMetadataProtocolRouting(t *testing.T) {
	fixture := loadResponsesFixture(t, "responses_text.sse")
	var (
		mu      sync.Mutex
		gotPath string
	)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		gotPath = r.URL.Path
		mu.Unlock()
		// 两种协议都回同一段 SSE（翻译层各取所需）。
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write(fixture)
	}))
	defer upstream.Close()

	// 元数据缓存文件：muse-free → responses，pickle-free → chat。
	dir := t.TempDir()
	shape := metadataCacheShape{FetchedAt: time.Now().Unix()}
	if err := json.Unmarshal([]byte(miniAPIJSON()), &providersTmp); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	shape.Provider = providersTmp["opencode"]
	mb, _ := json.Marshal(shape)
	metaPath := filepath.Join(dir, "metadata.json")
	if err := os.WriteFile(metaPath, mb, 0o644); err != nil {
		t.Fatalf("写元数据缓存失败: %v", err)
	}

	raw := fmt.Sprintf(`
server:
  api_keys: []
upstream:
  base_url: %q
  metadata_url: ""
  metadata_file: %q
  models: ["muse-free", "pickle-free"]
  user_agent: "opencode/9.9.9"
  request_timeout: "5s"
  first_byte_timeout: "5s"
`, upstream.URL, metaPath)
	cfg, err := decodeConfig([]byte(raw))
	if err != nil {
		t.Fatalf("decodeConfig 失败: %v", err)
	}
	pool, err := buildProxyPool(&cfg, testLogger())
	if err != nil {
		t.Fatalf("buildProxyPool 失败: %v", err)
	}
	meta := loadMetadata(metaPath)
	if !meta.hasData() {
		t.Fatal("元数据缓存应加载成功")
	}
	rt := newRouter(cfg.Upstream.ModelPrefix, cfg.Upstream.ModelAliases)
	rt.setExposed([]string{"muse-free", "pickle-free"})
	h := routes(&cfg, testLogger(), pool, rt, nil, meta, time.Now())

	// responses 协议模型（元数据驱动，无 responses_models 配置）。
	rec := srvDo(h, http.MethodPost, "/v1/chat/completions", "",
		`{"model":"muse-free","stream":false,"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("状态 = %d，期望 200；body=%s", rec.Code, rec.Body.String())
	}
	mu.Lock()
	if gotPath != "/responses" {
		t.Fatalf("muse-free 上游路径 = %q，期望 /responses（元数据驱动）", gotPath)
	}
	mu.Unlock()

	// chat 协议模型。
	rec = srvDo(h, http.MethodPost, "/v1/chat/completions", "",
		`{"model":"pickle-free","stream":false,"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("状态 = %d，期望 200；body=%s", rec.Code, rec.Body.String())
	}
	mu.Lock()
	if gotPath != "/chat/completions" {
		t.Fatalf("pickle-free 上游路径 = %q，期望 /chat/completions", gotPath)
	}
	mu.Unlock()

	// /healthz 带元数据计数。
	rec = srvDo(h, http.MethodGet, "/healthz", "", "")
	if !strings.Contains(rec.Body.String(), `"metadata":5`) {
		t.Fatalf("healthz 应含 metadata 计数: %s", rec.Body.String())
	}
}

// providersTmp 是 miniAPIJSON 的解码目标（包级变量仅为测试便利）。
var providersTmp map[string]metadataProvider

// TestModelsEndpointTriggersMetadataRefresh 覆盖「/v1/models 请求时刷新过期元数据」：
// 旧缓存（muse 已弃用）+ 新元数据（muse 复活）→ GET /v1/models 后 muse 出现。
func TestModelsEndpointTriggersMetadataRefresh(t *testing.T) {
	// 元数据源：miniAPIJSON（muse-free 免费 active）。
	metaSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(miniAPIJSON()))
	}))
	defer metaSrv.Close()

	dir := t.TempDir()
	// 旧缓存：fetchedAt 很老，且 muse-free 标 deprecated（旧世界）。
	stale := `{"fetched_at": 1000000000, "provider": {"npm": "@ai-sdk/openai-compatible", "models": {
		"muse-free": {"id": "muse-free", "status": "deprecated", "cost": {"input": 0, "output": 0}}}}}`
	metaPath := filepath.Join(dir, "metadata.json")
	if err := os.WriteFile(metaPath, []byte(stale), 0o644); err != nil {
		t.Fatalf("写旧缓存失败: %v", err)
	}
	catPath := writeCatalogFile(t, "muse-free", "brand-new-free")

	raw := fmt.Sprintf(`
server:
  api_keys: []
upstream:
  base_url: "http://127.0.0.1:1"
  catalog_file: %q
  metadata_url: %q
  metadata_file: %q
`, catPath, metaSrv.URL, metaPath)
	cfg, err := decodeConfig([]byte(raw))
	if err != nil {
		t.Fatalf("decodeConfig 失败: %v", err)
	}
	pool, err := buildProxyPool(&cfg, testLogger())
	if err != nil {
		t.Fatalf("buildProxyPool 失败: %v", err)
	}
	cat := loadCatalog(catPath)
	meta := loadMetadata(metaPath)
	rt := newRouter(cfg.Upstream.ModelPrefix, cfg.Upstream.ModelAliases)
	rt.setExposed(exposedFromCatalog(&cfg, cat, meta))
	h := routes(&cfg, testLogger(), pool, rt, cat, meta, time.Now())

	// 刷新前：旧缓存里 muse 是 deprecated → 只有 brand-new-free（正则兜底）。
	if got := rt.exposedList(); strings.Join(got, ",") != "brand-new-free" {
		t.Fatalf("刷新前暴露 = %v，期望 [brand-new-free]", got)
	}

	// GET /v1/models 触发刷新。
	rec := srvDo(h, http.MethodGet, "/v1/models", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("/v1/models 状态 = %d", rec.Code)
	}
	// 刷新后：muse-free 复活（免费+active+responses）。
	names := rt.exposedList()
	if !strListContains(names, "muse-free") {
		t.Fatalf("刷新后应暴露 muse-free：got=%v", names)
	}
	if !strListContains(names, "brand-new-free") {
		t.Fatalf("未知新模型仍应正则兜底：got=%v", names)
	}
	// 响应里能列出 oc/ 前缀名。
	if !strings.Contains(rec.Body.String(), "oc/muse-free") {
		t.Fatalf("模型列表应含 oc/muse-free: %s", rec.Body.String())
	}
}
