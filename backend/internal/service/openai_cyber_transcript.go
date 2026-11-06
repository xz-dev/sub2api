package service

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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
	// Model and tool definitions are request configuration rather than history.
	// Root instructions remain model-visible context and participate in identity.
	for _, field := range []string{"instructions"} {
		v := root.Get(field)
		if !v.Exists() || (v.Type == gjson.String && strings.TrimSpace(v.String()) == "") {
			continue
		}
		canonical := cyberTranscriptCanonical(v.Raw)
		if v.Type == gjson.String {
			canonical = v.String()
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
			encoded, _ := json.Marshal(item.String())
			canonical = string(encoded)
		case gjson.JSON:
			canonical = cyberTranscriptCanonical(item.Raw)
		}
		if strings.TrimSpace(canonical) == "" {
			return true
		}
		_, _ = h.Write([]byte("|item="))
		_, _ = h.Write(cyberTranscriptBytesView(canonical))
		length++
		return visit(item, length, prefixKey)
	}

	if messages := root.Get("messages"); messages.Exists() {
		if messages.IsArray() {
			messages.ForEach(feed)
		}
		return
	}
	input := root.Get("input")
	if input.IsArray() {
		input.ForEach(feed)
		return
	}
	if input.Type == gjson.String && strings.TrimSpace(input.String()) != "" {
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
// hash: canonical JSON when contentSeedDecodeBounded (at most
// contentSeedDecodeMaxBytes bytes and contentSeedDecodeMaxValues values),
// else the raw bytes. A decode costs about 150 bytes per value plus about
// 2-6 x the string bytes, and runs on every request while the session
// block is on (rr6 follow-up: one input item of many small parts; rr9: one
// item of a single large escaped string; both on the verified-raw path).
// Block keys are written and looked up with the same form; a large item
// blocked before the upgrade is keyed again on its next block.
func cyberTranscriptCanonical(raw string) string {
	if !contentSeedDecodeBounded(raw) {
		return raw
	}
	return normalizeCompatSeedJSON(json.RawMessage(raw))
}
