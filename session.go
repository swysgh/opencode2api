package main

import (
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"
	"time"
)

const base62Chars = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

// sessionIDFor 生成 OpenCode 客户端形状的会话 id：ses_ + 12 位 hex + 14 位 base62。
//
// 两个上游约束（均实测）：
//   - 格式必须匹配 ses_<12 hex><14 base62>，否则 403 FreeTierError；
//   - 免费额度按会话计，稳定会话比每请求新建更不容易触发 429。
//
// 因此同一 seed 在同一 TTL 时间窗内返回同一个 id。
func sessionIDFor(seed string, ttl time.Duration, now time.Time) string {
	if ttl <= 0 {
		ttl = 30 * time.Minute
	}
	bucket := now.UnixNano() / int64(ttl)
	sum := sha256.Sum256([]byte(seed + "|" + strconv.FormatInt(bucket, 10)))

	var b strings.Builder
	b.WriteString("ses_")
	b.WriteString(hex.EncodeToString(sum[0:6]))
	for i := 6; i < 20; i++ {
		b.WriteByte(base62Chars[int(sum[i])%len(base62Chars)])
	}
	return b.String()
}
