package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// 元数据驱动的模型发现：上游 /zen/v1/models 目录只有 id（无协议/价格/弃用状态），
// 这里叠加官方模型目录 models.opencode.ai/api.json 的 opencode 段做三类判定：
//   - 免费档：cost input/output 均为 0 且 status != deprecated；
//   - 协议路由：provider.npm 的 SDK（openai-compatible→chat、openai→responses、anthropic→anthropic）；
//   - 模态：modalities.input/output（对接 CPA 渠道的 input/output-modalities）。
// 可用性永远以目录为准：元数据只给「目录里存在的 id」做分类，deprecated 不覆盖正则兜底。
// 目录与元数据都落盘缓存：重启零请求，拉取失败沿用缓存（不致命）。

const defaultMetadataURL = "https://models.opencode.ai/api.json"
const defaultMetadataFile = "./data/metadata.json"

// 上游免费/付费/协议形态天天变，元数据 24h 刷新足够（目录刷新仍是 6h）。
const (
	defaultMetadataRefresh = 24 * time.Hour
	defaultMetadataTimeout = 15 * time.Second
)

// metadataProvider / metadataModel 是 api.json 里我们关心的字段子集（宽容解析：
// 没有的字段零值，不报错；cost 指针区分「没写」与「写了 0」）。
type metadataProvider struct {
	API    string                      `json:"api"`
	NPM    string                      `json:"npm"`
	Models map[string]metadataModelRaw `json:"models"`
}

type metadataModel struct {
	ID          string `json:"id"`
	Status      string `json:"status"`
	Reasoning   bool   `json:"reasoning"`
	ToolCall    bool   `json:"tool_call"`
	ProviderNPM string `json:"-"`
	CostInput   *float64 `json:"-"`
	CostOutput  *float64 `json:"-"`
	InputMods   []string `json:"-"`
	OutputMods  []string `json:"-"`
}

// 中间形状：先按 JSON 解出来再归并到 metadataModel（字段名带点，不好直接打 tag）。
type metadataModelRaw struct {
	ID          string `json:"id"`
	Status      string `json:"status"`
	Reasoning   bool   `json:"reasoning"`
	ToolCall    bool   `json:"tool_call"`
	Provider    *struct {
		NPM string `json:"npm"`
	} `json:"provider"`
	Cost *struct {
		Input  *float64 `json:"input"`
		Output *float64 `json:"output"`
	} `json:"cost"`
	Modalities *struct {
		Input  []string `json:"input"`
		Output []string `json:"output"`
	} `json:"modalities"`
}

func (r metadataModelRaw) normalize() metadataModel {
	m := metadataModel{
		ID:        r.ID,
		Status:    r.Status,
		Reasoning: r.Reasoning,
		ToolCall:  r.ToolCall,
	}
	if r.Provider != nil {
		m.ProviderNPM = r.Provider.NPM
	}
	if r.Cost != nil {
		m.CostInput = r.Cost.Input
		m.CostOutput = r.Cost.Output
	}
	if r.Modalities != nil {
		m.InputMods = r.Modalities.Input
		m.OutputMods = r.Modalities.Output
	}
	return m
}

// isFree 判定免费档：cost 的 input/output 都显式为 0（缺 cost 视为未知，不算免费），
// 且未被上游标 deprecated。
func (m metadataModel) isFree() bool {
	if m.Status == "deprecated" {
		return false
	}
	return m.CostInput != nil && *m.CostInput == 0 &&
		m.CostOutput != nil && *m.CostOutput == 0
}

// protocolFromSDK 把 provider.npm 的 SDK 名映射成上游协议：
// openai-compatible → chat（先判，避免被 Contains "@ai-sdk/openai" 吃掉）、
// @ai-sdk/openai → responses、anthropic → anthropic；其余/未知 → ""（按 chat 兜底）。
func protocolFromSDK(npm string) string {
	v := strings.ToLower(strings.TrimSpace(npm))
	switch {
	case v == "":
		return ""
	case strings.Contains(v, "openai-compatible"):
		return "chat"
	case strings.Contains(v, "@ai-sdk/openai"):
		return "responses"
	case strings.Contains(v, "anthropic"):
		return "anthropic"
	default:
		return ""
	}
}

// protocolOf 返回模型的上游协议（"" = 未知）：模型级 provider.npm 覆盖 provider 级。
func (m *metadataStore) protocolOf(id string) string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.protocols[id]
}

// modelInfo 返回模型的元数据（不存在时 ok=false）。
func (m *metadataStore) modelInfo(id string) (metadataModel, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	mo, ok := m.models[id]
	return mo, ok
}

// hasData 报告是否加载到了任何元数据（决定 exposedFromCatalog 是否启用元数据路径）。
func (m *metadataStore) hasData() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.models) > 0
}

// protocolCounts 返回各协议的模型数（启动日志用）。
func (m *metadataStore) protocolCounts() map[string]int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := map[string]int{}
	for _, p := range m.protocols {
		out[p]++
	}
	out["unknown"] = len(m.models) - len(m.protocols)
	return out
}

// metadataStore 缓存 opencode 段元数据并派生协议表（落盘 metadata_file）。
type metadataStore struct {
	mu        sync.RWMutex
	path      string
	fetchedAt time.Time
	models    map[string]metadataModel
	protocols map[string]string
}

// metadataCacheShape 是落盘文件的形状：只留 opencode 段 + 抓取时间。
type metadataCacheShape struct {
	FetchedAt int64            `json:"fetched_at"`
	Provider  metadataProvider `json:"provider"`
}

// loadMetadata 读取落盘缓存（文件不存在/损坏返回空 store，不报错——启动不强依赖元数据）。
func loadMetadata(path string) *metadataStore {
	m := &metadataStore{path: path, models: map[string]metadataModel{}, protocols: map[string]string{}}
	if path == "" {
		return m
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return m
	}
	var shape metadataCacheShape
	if err := json.Unmarshal(data, &shape); err != nil {
		return m
	}
	m.fetchedAt = time.Unix(shape.FetchedAt, 0)
	m.ingest(shape.Provider)
	return m
}

// ingest 用 provider 段重建派生表（models + protocols）。调用方持有写锁。
func (m *metadataStore) ingest(p metadataProvider) {
	models := map[string]metadataModel{}
	protocols := map[string]string{}
	for id, raw := range p.Models {
		mo := raw.normalize()
		if mo.ID == "" {
			mo.ID = id
		}
		if mo.ID == "" {
			continue
		}
		models[mo.ID] = mo
		npm := p.NPM
		if mo.ProviderNPM != "" {
			npm = mo.ProviderNPM
		}
		if proto := protocolFromSDK(npm); proto != "" {
			protocols[mo.ID] = proto
		}
	}
	m.models = models
	m.protocols = protocols
}

// age 返回自上次成功抓取过去的时长（从未抓取 → 很大）。
func (m *metadataStore) age() time.Duration {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.fetchedAt.IsZero() {
		return 1<<63 - 1
	}
	return time.Since(m.fetchedAt)
}

// refresh 同步重拉元数据并落盘。失败返回 error（调用方沿用缓存，不致命）。
// api.json 是全 provider 的大对象，只保留 opencode 段（opencode-go 是付费 Go 档，不收）。
func (m *metadataStore) refresh(ctx context.Context, url string, timeout time.Duration, doer httpDoer) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("构造元数据请求失败: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "opencode2api-metadata/"+version)
	if doer == nil {
		doer = &http.Client{Timeout: timeout}
	}
	resp, err := doer.Do(req)
	if err != nil {
		return fmt.Errorf("拉取元数据失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("拉取元数据返回 %d", resp.StatusCode)
	}
	var providers map[string]metadataProvider
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<20)).Decode(&providers); err != nil {
		return fmt.Errorf("解析元数据失败: %w", err)
	}
	p, ok := providers["opencode"]
	if !ok || len(p.Models) == 0 {
		return fmt.Errorf("元数据里没有 opencode 段（或为空）")
	}

	m.mu.Lock()
	m.fetchedAt = time.Now()
	m.ingest(p)
	m.mu.Unlock()

	if m.path != "" {
		shape := metadataCacheShape{FetchedAt: m.fetchedAt.Unix(), Provider: p}
		if data, mErr := json.Marshal(shape); mErr == nil {
			_ = os.WriteFile(m.path, data, 0o644)
		}
	}
	return nil
}

// metadataFreeActive 判定「目录 id 命中元数据免费档且协议可服务」：
// 免费（cost 0/0 且非 deprecated）且协议不是 anthropic（本服务没有 /messages 通道）。
// 协议未知按可服务兜底（与正则兜底同精神）。
func (m *metadataStore) metadataFreeActive(id string) bool {
	mo, ok := m.modelInfo(id)
	if !ok || !mo.isFree() {
		return false
	}
	return m.protocolOf(id) != "anthropic"
}
