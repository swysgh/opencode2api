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
// 上游会解出时间戳做新鲜度校验 —— 前缀解不出近期时间的会话一律 403 FreeTierError
// （实测 2026-10-14：随机 hex 全 403，12 小时前的时间戳仍 200）。后缀 14 位 base62 上游不校验。
//
// 因此这里把「TTL 时间窗起点」编码进前缀：同一 seed 在同一窗口内返回同一个 id，
// 且 id 里的时间戳始终落在上游接受的新鲜范围内（窗口长度须远小于上游新鲜度上限）。
func sessionIDFor(seed string, ttl time.Duration, now time.Time) string {
	if ttl <= 0 {
		ttl = 30 * time.Minute
	}
	bucket := now.UnixNano() / int64(ttl)
	createdMs := bucket * int64(ttl/time.Millisecond)
	sum := sha256.Sum256([]byte(seed + "|" + strconv.FormatInt(bucket, 10)))

	// 计数器取哈希里的一个字节（1..4095），与 OpenCode 的同毫秒序号同量级但不撞号。
	counter := uint64(sum[20])%4095 + 1
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
