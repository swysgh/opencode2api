package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"time"
)

const base62Chars = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

// sessionIDFor 生成 OpenCode 客户端形状的会话 id：ses_ + 12 位 hex + 14 位 base62。
//
// 12 位 hex 不是随机值：OpenCode 的 cq() 编码的是 hex48(~(毫秒时间戳*4096 + 同毫秒计数器))，
// 上游会解出时间戳做新鲜度校验 —— 前缀解不出近期时间的会话一律 403 FreeTierError。
// 后缀 14 位 base62 上游不校验。
//
// V2 门禁（2026-10-15 消融实测）把新鲜度窗口收紧到约 20 分钟：编码 1 分钟龄的时间戳 200，
// 23~28 分钟龄 403。因此这里把「时间窗起点」编码进前缀时，桶长硬封顶 10 分钟——
// 同一 seed 在同一窗口内返回同一个 id（稳定性），且 id 里的时间戳最大 10 分钟龄，
// 始终落在上游接受的新鲜范围内。窗口长度与上游新鲜度上限之间的裕量即来自此封顶。
// 调用方传入更大的 ttl（如配置的 session_ttl=30m）只影响种子分桶数量，不再放大时间戳年龄。
func sessionIDFor(seed string, ttl time.Duration, now time.Time) string {
	bucketLen := ttl
	if bucketLen <= 0 {
		bucketLen = 10 * time.Minute
	}
	if max := 10 * time.Minute; bucketLen > max {
		bucketLen = max
	}
	nowMs := now.UnixMilli()
	bucket := nowMs / int64(bucketLen/time.Millisecond)
	createdMs := bucket * int64(bucketLen/time.Millisecond)
	sum := sha256.Sum256([]byte(seed + "|" + strconv.FormatInt(bucket, 10)))

	// 计数器是 OpenCode 客户端的「同毫秒序号」，真实客户端一次只发几个请求，序号很小；
	// V2 门禁（2026-10-15 消融实测）校验 counter ≤ 63：47/1/48/56/60/63 均 200，64+ 全 403。
	// 这里从哈希取 1..63 的值保持 seed 可分辨性。
	counter := uint64(sum[20])%63 + 1
	enc := (^(uint64(createdMs)*4096 + counter)) & 0xFFFFFFFFFFFF

	var b strings.Builder
	b.WriteString("ses_")
	b.WriteString(fmt.Sprintf("%012x", enc))
	for i := 0; i < 14; i++ {
		b.WriteByte(base62Chars[int(sum[i])%len(base62Chars)])
	}
	return b.String()
}

// decodeSessionCreatedMs 是 sessionIDFor 的逆运算，仅用于测试与排障：
// 从 12 位 hex 前缀解出会话创建时间（毫秒，已按 2^36 取模，与上游看到的量一致）。
func decodeSessionCreatedMs(id string) (int64, error) {
	body := strings.TrimPrefix(id, "ses_")
	if len(body) < 12 {
		return 0, fmt.Errorf("会话 id %q 太短", id)
	}
	raw, err := hex.DecodeString(body[:12])
	if err != nil {
		return 0, fmt.Errorf("会话 id %q 前缀不是 hex: %w", id, err)
	}
	var enc uint64
	for _, c := range raw {
		enc = enc<<8 | uint64(c)
	}
	n := (^enc) & 0xFFFFFFFFFFFF
	return int64(n >> 12), nil
}
