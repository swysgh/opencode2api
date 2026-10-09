package main

import (
	"sort"
	"strings"
	"sync"
)

// router 负责客户端调用名 ↔ 上游 id 的双向解析与路由判定（§3/§4）。
type router struct {
	mu          sync.RWMutex
	modelPrefix string
	// exposed：当前暴露的上游 id 集合（命中正则/列表 或 来自目录）。
	exposed map[string]struct{}
	// aliasToUpstream：额外客户端名（不带前缀）→ 上游 id。
	aliasToUpstream map[string]string
}

// newRouter 构造路由器。aliases 是 上游 id → 额外名 的映射（§3.4）。
func newRouter(prefix string, aliases map[string][]string) *router {
	if prefix == "" {
		prefix = defaultModelPrefix
	}
	a2u := map[string]string{}
	for upstreamID, names := range aliases {
		for _, n := range names {
			n = strings.TrimSpace(n)
			if n == "" {
				continue
			}
			// 别名不应带前缀（§3.4）；若带了也剥掉，避免双重前缀。
			if strings.HasPrefix(n, prefix) {
				n = strings.TrimPrefix(n, prefix)
			}
			a2u[n] = upstreamID
		}
	}
	return &router{
		modelPrefix:     prefix,
		exposed:         map[string]struct{}{},
		aliasToUpstream: a2u,
	}
}

// setExposed 更新当前暴露的上游 id 集合。
func (r *router) setExposed(ids []string) {
	m := map[string]struct{}{}
	for _, id := range ids {
		m[id] = struct{}{}
	}
	r.mu.Lock()
	r.exposed = m
	r.mu.Unlock()
}

// exposedList 返回排序后的暴露集合（供注册用）。
func (r *router) exposedList() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.exposed))
	for id := range r.exposed {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// registeredName 返回某个上游 id 的注册名（前缀 + id）。
func (r *router) registeredName(upstreamID string) string {
	r.mu.RLock()
	prefix := r.modelPrefix
	r.mu.RUnlock()
	return prefix + upstreamID
}

// aliasNames 返回所有别名（额外客户端名，不带前缀）。
func (r *router) aliasNames() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.aliasToUpstream))
	for n := range r.aliasToUpstream {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// matchRoute 判断请求模型名是否由本服务接管（§5.2）：
//   - 以 prefix 开头 → 剥前缀，命中暴露集合则接管；
//   - 命中别名表 → 接管；
//   - 裸上游 id（不带前缀且不在别名表）→ 再看一眼暴露集合，命中则接管
//     （独立服务没有名字冲突问题，比插件版宽松：请求 big-pickle 等价于 oc/big-pickle）；
//   - 陌生名 → 不接管。
func (r *router) matchRoute(requestedModel string) bool {
	if requestedModel == "" {
		return false
	}
	r.mu.RLock()
	prefix := r.modelPrefix
	exposed := r.exposed
	a2u := r.aliasToUpstream
	r.mu.RUnlock()

	if strings.HasPrefix(requestedModel, prefix) {
		id := strings.TrimPrefix(requestedModel, prefix)
		if _, ok := exposed[id]; ok {
			return true
		}
		return false
	}
	if _, ok := a2u[requestedModel]; ok {
		return true
	}
	// 额外接受裸上游 id（§5.2）。
	if _, ok := exposed[requestedModel]; ok {
		return true
	}
	return false
}

// resolveUpstreamModel 把请求模型名解析为上游 id（供执行器改写 body，§5.2）：
//   - 剥 prefix 后命中暴露集合 → 用该 id；
//   - 命中别名表 → 用对应上游 id；
//   - 裸上游 id 命中暴露集合 → 直接用该 id（§5.2，独立服务无名字冲突）；
//   - 都不中 → 返回 ""（model_not_found）。
func (r *router) resolveUpstreamModel(requestedModel string) string {
	if requestedModel == "" {
		return ""
	}
	r.mu.RLock()
	prefix := r.modelPrefix
	exposed := r.exposed
	a2u := r.aliasToUpstream
	r.mu.RUnlock()

	if strings.HasPrefix(requestedModel, prefix) {
		id := strings.TrimPrefix(requestedModel, prefix)
		if _, ok := exposed[id]; ok {
			return id
		}
		return ""
	}
	if up, ok := a2u[requestedModel]; ok {
		return up
	}
	// 额外接受裸上游 id（§5.2）。
	if _, ok := exposed[requestedModel]; ok {
		return requestedModel
	}
	return ""
}
