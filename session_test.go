package main

import (
	"regexp"
	"testing"
	"time"
)

// 上游校验的会话 id 形状（实测：不符合即 403 FreeTierError）。
var sessionRe = regexp.MustCompile(`^ses_[0-9a-f]{12}[0-9A-Za-z]{14}$`)

// 2^36 毫秒 ≈ 2.2 年：上游只能看到时间戳的低 36 位，解码时同样取模比较。
const msMod36 = int64(1) << 36

func TestSessionIDMatchesUpstreamShape(t *testing.T) {
	id := sessionIDFor("req-1|zen-mimo-v2.5", 30*time.Minute, time.Now())
	if !sessionRe.MatchString(id) {
		t.Fatalf("会话 id %q 不匹配 ses_<12hex><14base62>", id)
	}
	if len(id) != 4+12+14 {
		t.Fatalf("长度 = %d, 期望 30", len(id))
	}
}

// 上游会从 12 位 hex 前缀解时间戳做新鲜度校验，随机 hex 会 403。
func TestSessionIDPrefixEncodesFreshTimestamp(t *testing.T) {
	now := time.Now()
	ttl := 30 * time.Minute
	for _, seed := range []string{"a", "b", "req-1|zen-mimo-v2.5"} {
		id := sessionIDFor(seed, ttl, now)
		got, err := decodeSessionCreatedMs(id)
		if err != nil {
			t.Fatalf("解码 %q 失败: %v", id, err)
		}
		delta := (now.UnixMilli()%msMod36 - got%msMod36 + msMod36) % msMod36
		if delta < 0 || delta > int64(ttl/time.Millisecond) {
			t.Fatalf("会话 %q 解出的时间戳比当前时间早 %d ms，超出 TTL %v", id, delta, ttl)
		}
	}
}

func TestSessionIDStableWithinTTLAndRotatesAfter(t *testing.T) {
	base := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	ttl := 30 * time.Minute

	first := sessionIDFor("seed", ttl, base)
	same := sessionIDFor("seed", ttl, base.Add(5*time.Minute))
	next := sessionIDFor("seed", ttl, base.Add(31*time.Minute))

	if first != same {
		t.Fatalf("同一时间窗内应稳定: %q vs %q", first, same)
	}
	if first == next {
		t.Fatalf("跨窗口应轮换: %q", first)
	}
}

func TestSessionIDDiffersBySeed(t *testing.T) {
	now := time.Now()
	if sessionIDFor("a", time.Minute, now) == sessionIDFor("b", time.Minute, now) {
		t.Fatal("不同 seed 不应产生同一 id")
	}
}
