package service

import (
	"bytes"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/bodyspool"
	"github.com/tidwall/gjson"
)

// contentSessionSeedPrefix prevents collisions between content-derived seeds
// and explicit session IDs (e.g. "sess-xxx" or "compat_cc_xxx").
const contentSessionSeedPrefix = "compat_cs_"

// contentStablePrefixSessionSeedPrefix distinguishes cache identities derived
// only from request fields that remain stable across independent prompts.
const contentStablePrefixSessionSeedPrefix = "compat_csp_"

// deriveOpenAIContentSessionSeed builds a stable session seed from an
// OpenAI-format request body. Only fields constant across conversation turns
// are included: model, tools/functions definitions, the leading system/developer
// prompt prefix in Chat messages, instructions (Responses API), and the first
// user message.
// Supports both Chat Completions (messages) and Responses API (input).
func deriveOpenAIContentSessionSeed(body []byte) string {
	if len(body) == 0 {
		return ""
	}

	const (
		modelField = iota
		toolsField
		functionsField
		instructionsField
		messagesField
		inputField
		contentSessionSeedFieldCount
		allContentSessionSeedFields = 1<<contentSessionSeedFieldCount - 1
	)
	var fields [contentSessionSeedFieldCount]gjson.Result
	var seen uint8
	// Match gjson.GetBytes by starting at the first root container, even when
	// malformed input has a non-JSON prefix.
	root := body
	for i := 0; i < len(body); i++ {
		switch body[i] {
		case '{':
			root = body[i:]
			goto scanRoot
		case '[':
			return ""
		}
	}
	return ""

scanRoot:
	nextKeyOffset := 1
	parseRawJSONView(root).ForEach(func(key, value gjson.Result) bool {
		if key.Index < nextKeyOffset || key.Index > len(root) {
			return false
		}
		// Result.ForEach can continue after the root '}' on malformed input.
		// The separator range excludes braces inside the preceding parsed value.
		if bytes.IndexByte(root[nextKeyOffset:key.Index], '}') >= 0 {
			return false
		}
		nextKeyOffset = value.Index + len(value.Raw)

		field := -1
		switch key.Str {
		case "model":
			field = modelField
		case "tools":
			field = toolsField
		case "functions":
			field = functionsField
		case "instructions":
			field = instructionsField
		case "messages":
			field = messagesField
		case "input":
			field = inputField
		}
		if field < 0 {
			return true
		}
		mask := uint8(1 << field)
		if seen&mask == 0 {
			fields[field] = value
			seen |= mask
		}
		return seen != allContentSessionSeedFields
	})

	// The prefix goes first so the (possibly body-sized) seed is not copied
	// again at the end.
	var b strings.Builder
	_, _ = b.WriteString(contentSessionSeedPrefix)

	if model := fields[modelField].String(); model != "" {
		_, _ = b.WriteString("model=")
		_, _ = b.WriteString(model)
	}

	if tools := fields[toolsField]; tools.Exists() && tools.IsArray() && tools.Raw != "[]" {
		_, _ = b.WriteString("|tools=")
		writeContentSeedJSON(&b, tools.Raw)
	}

	if funcs := fields[functionsField]; funcs.Exists() && funcs.IsArray() && funcs.Raw != "[]" {
		_, _ = b.WriteString("|functions=")
		writeContentSeedJSON(&b, funcs.Raw)
	}

	if instr := fields[instructionsField].String(); instr != "" {
		_, _ = b.WriteString("|instructions=")
		_, _ = b.WriteString(instr)
	}

	firstUserCaptured := false

	msgs := fields[messagesField]
	if msgs.Exists() && msgs.IsArray() {
		systemPrefixOpen := true
		msgs.ForEach(func(_, msg gjson.Result) bool {
			role := msg.Get("role").String()
			switch role {
			case "system", "developer":
				if systemPrefixOpen {
					_, _ = b.WriteString("|system=")
					if c := firstRawJSONMember(msg, "content"); c.Exists() {
						writeContentSeedJSON(&b, c.Raw)
					}
				}
			case "user":
				systemPrefixOpen = false
				if !firstUserCaptured {
					_, _ = b.WriteString("|first_user=")
					if c := firstRawJSONMember(msg, "content"); c.Exists() {
						writeContentSeedJSON(&b, c.Raw)
					}
					firstUserCaptured = true
				}
			default:
				systemPrefixOpen = false
			}
			return true
		})
	} else if inp := fields[inputField]; inp.Exists() {
		if inp.Type == gjson.String {
			_, _ = b.WriteString("|input=")
			_, _ = b.WriteString(inp.String())
		} else if inp.IsArray() {
			inp.ForEach(func(_, item gjson.Result) bool {
				role := item.Get("role").String()
				switch role {
				case "system", "developer":
					_, _ = b.WriteString("|system=")
					if c := firstRawJSONMember(item, "content"); c.Exists() {
						writeContentSeedJSON(&b, c.Raw)
					}
				case "user":
					if !firstUserCaptured {
						_, _ = b.WriteString("|first_user=")
						if c := firstRawJSONMember(item, "content"); c.Exists() {
							writeContentSeedJSON(&b, c.Raw)
						}
						firstUserCaptured = true
					}
				}
				if !firstUserCaptured && item.Get("type").String() == "input_text" {
					_, _ = b.WriteString("|first_user=")
					if text := item.Get("text").String(); text != "" {
						_, _ = b.WriteString(text)
					}
					firstUserCaptured = true
				}
				return true
			})
		}
	}

	if b.Len() == len(contentSessionSeedPrefix) {
		return ""
	}
	return b.String()
}

// contentSeedDecodeMaxValues: the most JSON values a content-seed field
// may have to be decoded into its canonical form. A decode costs about
// 150 bytes per value (rr6 follow-up: a 16 MiB tools array, or a developer
// message of tiny parts, cost k 22 on the verified-raw path, charged k_skel
// 6); 4096 values keep it under about 600 KiB.
const contentSeedDecodeMaxValues = 4096

// contentSeedDecodeMaxBytes: the most bytes a JSON value may have to be
// decoded on the verified-raw path without a charge (content seed,
// transcript, tool schema, turn metadata). The value cap alone let one
// large string through (rr9: a 16 MiB first user message of one escaped
// string cost k 7-8 on the verified-raw path, charged k_skel 6): the
// decode copies the string and the re-encode writes it again, up to 6 x
// for < > & (json.Marshal escapes them). Measured at 32 KiB: about 170 KB
// allocated for escaped text, about 1.2 MB for a string of only '<'. It
// must not exceed bodyspool's threshold T (64 KiB by default, the smallest
// spooled string), which keeps the decision the same on the skeleton and
// on the whole body; see contentSeedDecodeBounded.
const contentSeedDecodeMaxBytes = 32 << 10

// contentSeedDecodeBounded reports whether the JSON text raw may be
// decoded into its canonical form: at most contentSeedDecodeMaxBytes bytes
// and contentSeedDecodeMaxValues values, and no spool marker. A field with
// a spooled string is above the byte cap on the whole body (the string
// alone has at least T bytes), so the marker test gives the skeleton the
// same answer; a field without one is the same bytes on both. (A body
// that holds the text \u0000lrc itself is treated alike on both.)
func contentSeedDecodeBounded(raw string) bool {
	return len(raw) <= contentSeedDecodeMaxBytes && !bodyspool.MayHoldMarker(raw) &&
		!rawJSONValuesAbove(raw, contentSeedDecodeMaxValues)
}

// writeContentSeedJSON writes one field of the content session seed:
// canonical JSON (decoded and re-encoded, keys sorted) when
// contentSeedDecodeBounded, else "raw:" and the field as sent, which
// allocates nothing beyond the copy. The decision is the same on the
// skeleton and on the whole body, and a raw field's markers are expanded
// when the seed is hashed (deriveOpenAISessionHashesForRequest), so both
// give the same hash. A field above either cap gets a seed part that
// differs from the old canonical one (one sticky-session miss after the
// upgrade) and that is stable while the client resends the same bytes.
func writeContentSeedJSON(b *strings.Builder, raw string) {
	if !contentSeedDecodeBounded(raw) {
		_, _ = b.WriteString("raw:")
		_, _ = b.WriteString(raw)
		return
	}
	_, _ = b.WriteString(normalizeCompatSeedJSON(rawJSONView(raw)))
}

// rawJSONValuesAbove reports whether the JSON text raw has more than max
// values (counted as the '{', '[' and ',' outside strings, plus one).
func rawJSONValuesAbove(raw string, max int) bool {
	n, inString, escaped := 1, false, false
	for i := 0; i < len(raw); i++ {
		ch := raw[i]
		switch {
		case escaped:
			escaped = false
		case inString:
			escaped = ch == '\\'
			inString = ch != '"'
		case ch == '"':
			inString = true
		case ch == '{' || ch == '[' || ch == ',':
			if n++; n > max {
				return true
			}
		}
	}
	return false
}

// deriveOpenAIAnchoredContentSessionSeed returns the legacy content-derived
// seed only when it contains a meaningful user/input anchor. This preserves
// the existing session derivation while preventing model-only requests from
// becoming a tenant-wide cache routing identity.
func deriveOpenAIAnchoredContentSessionSeed(body []byte) string {
	if !hasOpenAIContentSessionUserAnchor(body) {
		return ""
	}
	return deriveOpenAIContentSessionSeed(body)
}

func hasOpenAIContentSessionUserAnchor(body []byte) bool {
	if len(body) == 0 {
		return false
	}

	if messages := gjson.GetBytes(body, "messages"); messages.Exists() && messages.IsArray() {
		anchored := false
		messages.ForEach(func(_, message gjson.Result) bool {
			if strings.TrimSpace(message.Get("role").String()) != "user" {
				return true
			}
			anchored = hasMeaningfulOpenAIContent(message.Get("content"))
			return false
		})
		return anchored
	}

	input := gjson.GetBytes(body, "input")
	if !input.Exists() {
		return false
	}
	if input.Type == gjson.String {
		return strings.TrimSpace(input.String()) != ""
	}
	if !input.IsArray() {
		return false
	}

	anchored := false
	input.ForEach(func(_, item gjson.Result) bool {
		if strings.TrimSpace(item.Get("role").String()) == "user" {
			anchored = hasMeaningfulOpenAIContent(item.Get("content"))
			return false
		}
		if strings.TrimSpace(item.Get("type").String()) == "input_text" {
			anchored = strings.TrimSpace(item.Get("text").String()) != ""
			return false
		}
		return true
	})
	return anchored
}

func hasMeaningfulOpenAIContent(content gjson.Result) bool {
	if !content.Exists() || content.Type == gjson.Null {
		return false
	}
	if content.Type == gjson.String {
		return strings.TrimSpace(content.String()) != ""
	}
	if !content.IsArray() {
		normalized, ok := normalizeNonEmptyCompatSeedJSON(content)
		return ok && strings.TrimSpace(normalized) != ""
	}

	meaningful := false
	content.ForEach(func(_, item gjson.Result) bool {
		if item.Type == gjson.String {
			meaningful = strings.TrimSpace(item.String()) != ""
		} else if text := item.Get("text"); text.Exists() {
			meaningful = strings.TrimSpace(text.String()) != ""
		} else {
			_, meaningful = normalizeNonEmptyCompatSeedJSON(item)
		}
		return !meaningful
	})
	return meaningful
}

// deriveOpenAIStablePrefixSessionSeed builds a seed from the reusable prefix
// of an OpenAI-format request. User and assistant content are deliberately
// excluded so independent prompts with the same system/tool prefix can share
// an upstream prompt-cache routing identity.
//
// An empty result means the request has no meaningful stable prefix. Callers
// must then use a narrower fallback instead of grouping all requests by tenant
// and model alone.
func deriveOpenAIStablePrefixSessionSeed(body []byte) string {
	if len(body) == 0 {
		return ""
	}

	var b strings.Builder
	hasStablePrefix := false
	appendJSON := func(label string, value gjson.Result) {
		normalized, ok := normalizeNonEmptyCompatSeedJSON(value)
		if !ok {
			return
		}
		_, _ = b.WriteString("|")
		_, _ = b.WriteString(label)
		_, _ = b.WriteString("=")
		_, _ = b.WriteString(normalized)
		hasStablePrefix = true
	}

	if tools := gjson.GetBytes(body, "tools"); tools.Exists() && tools.IsArray() {
		appendJSON("tools", tools)
	}
	if funcs := gjson.GetBytes(body, "functions"); funcs.Exists() && funcs.IsArray() {
		appendJSON("functions", funcs)
	}
	if instructions := gjson.GetBytes(body, "instructions"); strings.TrimSpace(instructions.String()) != "" {
		appendJSON("instructions", instructions)
	}

	appendSystemMessages := func(items gjson.Result) {
		items.ForEach(func(_, item gjson.Result) bool {
			role := strings.TrimSpace(item.Get("role").String())
			switch role {
			case "system", "developer":
				appendJSON(role, item.Get("content"))
			}
			return true
		})
	}

	if messages := gjson.GetBytes(body, "messages"); messages.Exists() && messages.IsArray() {
		appendSystemMessages(messages)
	} else if input := gjson.GetBytes(body, "input"); input.Exists() && input.IsArray() {
		appendSystemMessages(input)
	}

	if !hasStablePrefix {
		return ""
	}
	return contentStablePrefixSessionSeedPrefix + b.String()
}

func normalizeNonEmptyCompatSeedJSON(value gjson.Result) (string, bool) {
	if !value.Exists() || value.Type == gjson.Null {
		return "", false
	}
	normalized := normalizeCompatSeedJSON(rawJSONView(value.Raw))
	switch normalized {
	case "", `""`, "[]", "{}", "null":
		return "", false
	default:
		return normalized, true
	}
}
