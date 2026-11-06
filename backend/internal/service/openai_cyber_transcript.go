package service

import (
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"
	"unsafe"

	"github.com/tidwall/gjson"
)

// CyberSessionTranscriptBlock is a cumulative history-prefix key together with
// the number of history items it covers. Recording the length lets lookups
// recompute exactly that prefix, so long or padded continuations stay exact.
type CyberSessionTranscriptBlock struct {
	Key    string
	Length int
}

// CyberSessionTranscriptBlocks returns the exact full-history block followed
// by an optional rewrite-tolerant block ending before the latest user turn.
// The latter requires model-generated history so shared first-turn templates
// cannot block unrelated conversations.
func CyberSessionTranscriptBlocks(apiKeyID int64, body []byte) []CyberSessionTranscriptBlock {
	var full, preLatestUser CyberSessionTranscriptBlock
	// This is an entropy heuristic, not provenance proof: authenticated
	// server-side history would be required to distinguish fixed few-shot
	// assistant items perfectly.
	hasModelGeneratedItem := false
	walkOpenAICyberTranscript(apiKeyID, body, func(item gjson.Result, length int, prefixKey func() string) bool {
		if openAICyberTranscriptItemStartsUserTurn(item) && hasModelGeneratedItem && full.Length > 0 {
			preLatestUser = full
		}
		full = CyberSessionTranscriptBlock{Key: prefixKey(), Length: length}
		if openAICyberTranscriptItemIsModelGenerated(item) {
			hasModelGeneratedItem = true
		}
		return true
	})
	if full.Length == 0 {
		return nil
	}
	blocks := []CyberSessionTranscriptBlock{full}
	if preLatestUser.Length > 0 {
		blocks = append(blocks, preLatestUser)
	}
	return blocks
}

// cyberSessionTranscriptKeysAt recomputes the transcript prefix keys at the
// recorded block lengths, in ascending length order. Lengths beyond the
// request history cannot match and are skipped.
func cyberSessionTranscriptKeysAt(apiKeyID int64, body []byte, lengths []int) []string {
	wanted := make(map[int]struct{}, len(lengths))
	maxLength := 0
	for _, length := range lengths {
		if length > 0 {
			wanted[length] = struct{}{}
			maxLength = max(maxLength, length)
		}
	}
	if len(wanted) == 0 {
		return nil
	}
	keys := make([]string, 0, len(wanted))
	walkOpenAICyberTranscript(apiKeyID, body, func(_ gjson.Result, length int, prefixKey func() string) bool {
		if _, ok := wanted[length]; ok {
			keys = append(keys, prefixKey())
		}
		return length < maxLength
	})
	return keys
}

// walkOpenAICyberTranscript hashes model-visible history cumulatively and calls
// visit after each item with the 1-based prefix length. prefixKey returns the
// key of the prefix ending at that item and is only valid during the call.
// Returning false from visit stops the walk.
func walkOpenAICyberTranscript(apiKeyID int64, body []byte, visit func(item gjson.Result, length int, prefixKey func() string) bool) {
	if len(body) == 0 {
		return
	}
	root := openAIRequestPayloadView(body)
	if !root.Exists() || !root.IsObject() {
		return
	}

	h := sha256.New()
	_, _ = h.Write([]byte("cyber-transcript:v3|api_key="))
	_, _ = h.Write([]byte(strconv.FormatInt(apiKeyID, 10)))
	// Every canonical form, unescape and re-escape of request bytes below is
	// paid from one budget (derivedKeyBudgetBytes, rr10); past it a value is
	// hashed as sent. Members are read raw: nothing is unescaped outside it.
	budget := newDerivedKeyBudget()
	// Model and tool definitions are request configuration rather than history.
	// Root instructions remain model-visible context and participate in identity.
	for _, field := range []string{"instructions"} {
		v := firstRawJSONMember(root, field)
		if !v.Exists() || (v.Type == gjson.String && rawJSONStringBlank(v.Raw)) {
			continue
		}
		canonical := v.Raw
		if v.Type == gjson.String {
			if s, ok := budget.stringValue(v); ok {
				canonical = s
			}
		} else {
			canonical = cyberTranscriptCanonical(budget, v.Raw)
		}
		_, _ = h.Write([]byte("|"))
		_, _ = h.Write([]byte(field))
		_, _ = h.Write([]byte("="))
		_, _ = h.Write(cyberTranscriptBytesView(canonical))
	}

	prefixKey := func() string { return hex.EncodeToString(h.Sum(nil)) }
	length := 0
	feed := func(_, item gjson.Result) bool {
		canonical := item.Raw
		switch item.Type {
		case gjson.String:
			if encoded, ok := budget.marshalString(item); ok {
				canonical = encoded
			}
		case gjson.JSON:
			canonical = cyberTranscriptCanonical(budget, item.Raw)
		}
		if strings.TrimSpace(canonical) == "" {
			return true
		}
		_, _ = h.Write([]byte("|item="))
		_, _ = h.Write(cyberTranscriptBytesView(canonical))
		length++
		return visit(item, length, prefixKey)
	}

	if messages := firstRawJSONMember(root, "messages"); messages.Exists() {
		if messages.IsArray() {
			forEachRawJSONMember(messages, feed)
		}
		return
	}
	input := firstRawJSONMember(root, "input")
	if input.IsArray() {
		forEachRawJSONMember(input, feed)
		return
	}
	if input.Type == gjson.String && !rawJSONStringBlank(input.Raw) {
		feed(gjson.Result{}, input)
	}
}

func openAICyberTranscriptItemStartsUserTurn(item gjson.Result) bool {
	if strings.EqualFold(strings.TrimSpace(item.Get("role").String()), "user") {
		content := firstRawJSONMember(item, "content") // never unescaped (rr9)
		if content.IsArray() {
			hasUserContent := false
			forEachRawJSONMember(content, func(_, block gjson.Result) bool {
				switch strings.ToLower(strings.TrimSpace(block.Get("type").String())) {
				case "tool_result", "function_call_output", "custom_tool_call_output", "computer_call_output":
				default:
					hasUserContent = true
				}
				return !hasUserContent
			})
			return hasUserContent
		}
		return content.Exists()
	}
	return strings.EqualFold(strings.TrimSpace(item.Get("type").String()), "input_text")
}

func openAICyberTranscriptItemIsModelGenerated(item gjson.Result) bool {
	switch strings.ToLower(strings.TrimSpace(item.Get("role").String())) {
	case "assistant", "model":
		return true
	}
	switch strings.ToLower(strings.TrimSpace(item.Get("type").String())) {
	case "output_text", "function_call", "tool_call", "custom_tool_call", "computer_call":
		return true
	default:
		return false
	}
}

// cyberTranscriptBytesView is a read-only view of s for the hash, which
// only reads it: an item can be as large as the body (rr9: []byte(s)
// copied it).
func cyberTranscriptBytesView(s string) []byte {
	return unsafe.Slice(unsafe.StringData(s), len(s))
}

// cyberTranscriptCanonical is an item (or instructions) in the transcript
// hash: canonical JSON when budget allows it (derivedKeyBudget.canonicalJSON:
// at most contentSeedDecodeMaxBytes bytes and contentSeedDecodeMaxValues
// values, and within what is left of the derivation's budget), else the
// raw bytes. A decode costs about 150 bytes per value plus about 2-6 x the
// string bytes, and runs on every request while the session block is on
// (rr6 follow-up: one input item of many small parts; rr9: one item of a
// single large escaped string; rr10: thousands of such items). Block keys
// are written and looked up with the same form; a large item, or one past
// the budget, blocked before the upgrade is keyed again on its next block.
func cyberTranscriptCanonical(budget *derivedKeyBudget, raw string) string {
	if canonical, ok := budget.canonicalJSON(raw); ok {
		return canonical
	}
	return raw
}
