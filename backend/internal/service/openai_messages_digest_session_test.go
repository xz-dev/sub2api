//go:build unit

package service

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestOpenAICompatAnthropicDigestLongestPrefix: the longest prefix with a
// live binding wins, an expired binding is deleted, and another namespace
// never matches.
func TestOpenAICompatAnthropicDigestLongestPrefix(t *testing.T) {
	s := &OpenAIGatewayService{}
	account := &Account{ID: 7}
	s.bindOpenAICompatAnthropicDigestPromptCacheKey(account, 1, "s:a-u:b", "k-short", "")
	s.bindOpenAICompatAnthropicDigestPromptCacheKey(account, 1, "s:a-u:b-a:c-u:d", "k-long", "")

	key, matched := s.findOpenAICompatAnthropicDigestPromptCacheKey(account, 1, "s:a-u:b-a:c-u:d-a:e-u:f")
	require.Equal(t, "k-long", key)
	require.Equal(t, "s:a-u:b-a:c-u:d", matched)
	key, matched = s.findOpenAICompatAnthropicDigestPromptCacheKey(account, 1, "s:a-u:b-a:x")
	require.Equal(t, "k-short", key)
	require.Equal(t, "s:a-u:b", matched)
	key, _ = s.findOpenAICompatAnthropicDigestPromptCacheKey(account, 2, "s:a-u:b-a:c-u:d")
	require.Empty(t, key, "another API key")
	key, _ = s.findOpenAICompatAnthropicDigestPromptCacheKey(account, 1, "s:a-u:bb")
	require.Empty(t, key, "a prefix ends at a part boundary only")

	// Rebinding moves the binding: the old chain is deleted.
	s.bindOpenAICompatAnthropicDigestPromptCacheKey(account, 1, "s:a-u:b-a:c-u:d-a:e", "k-long", "s:a-u:b-a:c-u:d")
	_, matched = s.findOpenAICompatAnthropicDigestPromptCacheKey(account, 1, "s:a-u:b-a:c-u:d-a:z")
	require.Equal(t, "s:a-u:b", matched)

	// An expired binding is deleted and the next shorter one is used.
	s.openaiCompatAnthropicDigestSessions.Store(openAICompatAnthropicDigestChainKey(openAICompatAnthropicDigestNamespace(account, 1), "s:a-u:b-a:q"),
		openAICompatAnthropicDigestBinding{PromptCacheKey: "old", ExpiresAt: time.Now().Add(-time.Second)})
	key, _ = s.findOpenAICompatAnthropicDigestPromptCacheKey(account, 1, "s:a-u:b-a:q")
	require.Equal(t, "k-short", key)
	_, ok := s.openaiCompatAnthropicDigestSessions.Load(openAICompatAnthropicDigestChainKey(openAICompatAnthropicDigestNamespace(account, 1), "s:a-u:b-a:q"))
	require.False(t, ok)
}

// TestOpenAICompatAnthropicDigestLookupIsLinear: a chain of 100k parts
// (a ~2 MiB body of tiny messages) is looked up with allocations linear in
// the parts, not in parts x chain length (rr10: 27 GB at 2 MiB before).
func TestOpenAICompatAnthropicDigestLookupIsLinear(t *testing.T) {
	s := &OpenAIGatewayService{}
	account := &Account{ID: 7}
	parts := make([]string, 100_000)
	for i := range parts {
		parts[i] = "u:" + strconv.FormatInt(int64(i)*2654435761%1_000_000_007, 16)
	}
	chain := strings.Join(parts, "-")
	s.bindOpenAICompatAnthropicDigestPromptCacheKey(account, 1, strings.Join(parts[:10], "-"), "k", "")
	var key string
	allocs := testing.AllocsPerRun(1, func() {
		key, _ = s.findOpenAICompatAnthropicDigestPromptCacheKey(account, 1, chain)
	})
	require.Equal(t, "k", key)
	require.Less(t, allocs, float64(len(parts)*2+64), "allocations per lookup")
}
