package service

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
)

type openAICompatAnthropicDigestBinding struct {
	PromptCacheKey string
	ExpiresAt      time.Time
}

func buildOpenAICompatAnthropicDigestChain(req *apicompat.AnthropicRequest) string {
	if req == nil {
		return ""
	}

	parts := make([]string, 0, len(req.Messages)+1)
	if len(req.System) > 0 && strings.TrimSpace(string(req.System)) != "" && strings.TrimSpace(string(req.System)) != "null" {
		parts = append(parts, "s:"+shortHash(req.System))
	}
	for _, msg := range req.Messages {
		content := msg.Content
		if len(content) == 0 || strings.TrimSpace(string(content)) == "" {
			continue
		}
		prefix := "u"
		if strings.TrimSpace(msg.Role) == "assistant" {
			prefix = "a"
		}
		parts = append(parts, prefix+":"+shortHash(content))
	}
	return strings.Join(parts, "-")
}

func openAICompatAnthropicDigestNamespace(account *Account, cAPIKeyID int64) string {
	if account == nil || account.ID <= 0 {
		return ""
	}
	return fmt.Sprintf("%d|%d|", account.ID, cAPIKeyID)
}

// openAICompatAnthropicDigestKey is the session-map key of one digest
// chain prefix: a SHA-256 chain over the namespace and the chain's parts
// (key_0 = H(ns), key_i = H(key_i-1 || part_i)). Every prefix's key comes
// from the one before it, so finding the longest bound prefix is linear in
// the chain (review rr10: ns + chain per prefix was quadratic, 1.7 TB
// allocated for a 16 MiB /v1/messages body of tiny messages), and the map
// holds 32 bytes per binding instead of the whole chain.
type openAICompatAnthropicDigestKey [sha256.Size]byte

// forEachOpenAICompatAnthropicDigestPrefix calls fn with the key of each
// prefix of chain that ends at a part boundary, shortest first, and the
// prefix's length; it stops when fn returns false.
func forEachOpenAICompatAnthropicDigestPrefix(ns, chain string, fn func(key openAICompatAnthropicDigestKey, end int) bool) {
	h := sha256.New()
	var key openAICompatAnthropicDigestKey
	_, _ = io.WriteString(h, ns)
	h.Sum(key[:0])
	for start := 0; start <= len(chain); {
		end := strings.IndexByte(chain[start:], '-')
		if end < 0 {
			end = len(chain)
		} else {
			end += start
		}
		h.Reset()
		_, _ = h.Write(key[:])
		_, _ = io.WriteString(h, chain[start:end])
		h.Sum(key[:0])
		if !fn(key, end) {
			return
		}
		start = end + 1
	}
}

func openAICompatAnthropicDigestChainKey(ns, chain string) (key openAICompatAnthropicDigestKey) {
	forEachOpenAICompatAnthropicDigestPrefix(ns, chain, func(k openAICompatAnthropicDigestKey, _ int) bool {
		key = k
		return true
	})
	return key
}

func (s *OpenAIGatewayService) findOpenAICompatAnthropicDigestPromptCacheKey(account *Account, cAPIKeyID int64, digestChain string) (promptCacheKey string, matchedChain string) {
	if s == nil || digestChain == "" {
		return "", ""
	}
	ns := openAICompatAnthropicDigestNamespace(account, cAPIKeyID)
	if ns == "" {
		return "", ""
	}
	// The longest prefix with a live binding wins; expired bindings met on
	// the way are deleted.
	now := time.Now()
	forEachOpenAICompatAnthropicDigestPrefix(ns, digestChain, func(key openAICompatAnthropicDigestKey, end int) bool {
		raw, ok := s.openaiCompatAnthropicDigestSessions.Load(key)
		if !ok {
			return true
		}
		if binding, ok := raw.(openAICompatAnthropicDigestBinding); ok {
			if binding.ExpiresAt.IsZero() || now.Before(binding.ExpiresAt) {
				if k := strings.TrimSpace(binding.PromptCacheKey); k != "" {
					promptCacheKey, matchedChain = k, digestChain[:end]
					return true
				}
			}
		}
		s.openaiCompatAnthropicDigestSessions.Delete(key)
		return true
	})
	return promptCacheKey, matchedChain
}

func (s *OpenAIGatewayService) bindOpenAICompatAnthropicDigestPromptCacheKey(account *Account, cAPIKeyID int64, digestChain, promptCacheKey, oldDigestChain string) {
	if s == nil || digestChain == "" || strings.TrimSpace(promptCacheKey) == "" {
		return
	}
	ns := openAICompatAnthropicDigestNamespace(account, cAPIKeyID)
	if ns == "" {
		return
	}
	binding := openAICompatAnthropicDigestBinding{
		PromptCacheKey: strings.TrimSpace(promptCacheKey),
		ExpiresAt:      time.Now().Add(s.openAIWSResponseStickyTTL()),
	}
	s.openaiCompatAnthropicDigestSessions.Store(openAICompatAnthropicDigestChainKey(ns, digestChain), binding)
	if oldDigestChain != "" && oldDigestChain != digestChain {
		s.openaiCompatAnthropicDigestSessions.Delete(openAICompatAnthropicDigestChainKey(ns, oldDigestChain))
	}
}

func promptCacheKeyFromAnthropicDigest(digestChain string) string {
	if strings.TrimSpace(digestChain) == "" {
		return ""
	}
	return "anthropic-digest-" + hashSensitiveValueForLog(digestChain)
}

func promptCacheKeyFromAnthropicMetadataSession(req *apicompat.AnthropicRequest) string {
	if req == nil || len(req.Metadata) == 0 {
		return ""
	}
	var metadata struct {
		UserID string `json:"user_id"`
	}
	if err := json.Unmarshal(req.Metadata, &metadata); err != nil {
		return ""
	}
	parsed := ParseMetadataUserID(metadata.UserID)
	if parsed == nil || strings.TrimSpace(parsed.SessionID) == "" {
		return ""
	}
	seed := strings.Join([]string{
		"anthropic-metadata",
		strings.TrimSpace(parsed.DeviceID),
		strings.TrimSpace(parsed.AccountUUID),
		strings.TrimSpace(parsed.SessionID),
	}, "|")
	return "anthropic-metadata-" + hashSensitiveValueForLog(seed)
}

func cloneAnthropicRequestForDigest(req *apicompat.AnthropicRequest) *apicompat.AnthropicRequest {
	if req == nil {
		return nil
	}
	cp := *req
	if len(req.System) > 0 {
		cp.System = append(json.RawMessage(nil), req.System...)
	}
	if len(req.Messages) > 0 {
		cp.Messages = append([]apicompat.AnthropicMessage(nil), req.Messages...)
	}
	return &cp
}
