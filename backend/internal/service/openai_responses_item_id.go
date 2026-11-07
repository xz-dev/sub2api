package service

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// Input-item id checks in place (rr11). The input-ID sanitizer and the
// store=false replay read every item's id, call_id and type; gjson's Get
// unescapes an escaped id into a new string on each of the three walks,
// about 8.7 x the id bytes in total allocation
// (adv_rv1_many_reasoning_id_cap4096 k 9.08, _cap256 k 8.70). The checks
// need only a prefix and a length up to 64, so they read the id's raw
// JSON (firstRawJSONMember) and decode it here, byte by byte, without
// building the string.

// rawJSONStringDecodedBytes calls fn with each byte that gjson's unescape
// gives for the JSON string literal raw (quotes included), in order, and
// stops when fn returns false. Same decoding as gjson, also on malformed
// text: a raw byte is copied as is; in a literal with an escape (the only
// one gjson unescapes), a control byte, an unknown escape or a short \u
// escape ends the string; a bad hex digit counts as 0; a high
// surrogate followed by \u takes it as its pair (U+FFFD if invalid); a
// lone surrogate is U+FFFD.
func rawJSONStringDecodedBytes(raw string, fn func(b byte) bool) {
	if len(raw) < 2 || raw[0] != '"' {
		return
	}
	s := raw[1 : len(raw)-1]
	esc := strings.IndexByte(s, '\\') >= 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c < ' ' && esc:
			return
		case c != '\\':
			if !fn(c) {
				return
			}
			continue
		}
		i++
		if i >= len(s) {
			return
		}
		switch s[i] {
		case '\\', '/', '"':
			c = s[i]
		case 'b':
			c = '\b'
		case 'f':
			c = '\f'
		case 'n':
			c = '\n'
		case 'r':
			c = '\r'
		case 't':
			c = '\t'
		case 'u':
			if i+5 > len(s) {
				return
			}
			r := rawJSONHex4Lenient(s[i+1:])
			i += 5
			if utf16.IsSurrogate(r) && len(s)-i >= 6 && s[i] == '\\' && s[i+1] == 'u' {
				r = utf16.DecodeRune(r, rawJSONHex4Lenient(s[i+2:]))
				i += 6
			}
			i--
			var buf [utf8.UTFMax]byte
			n := utf8.EncodeRune(buf[:], r)
			for _, b := range buf[:n] {
				if !fn(b) {
					return
				}
			}
			continue
		default:
			return
		}
		if !fn(c) {
			return
		}
	}
}

// rawJSONHex4Lenient is gjson's runeit: the four hex digits at s, 0 if any
// is not a hex digit.
func rawJSONHex4Lenient(s string) rune {
	var r rune
	for _, c := range []byte(s[:4]) {
		switch {
		case c >= '0' && c <= '9':
			r = r<<4 | rune(c-'0')
		case c >= 'a' && c <= 'f':
			r = r<<4 | rune(c-'a'+10)
		case c >= 'A' && c <= 'F':
			r = r<<4 | rune(c-'A'+10)
		default:
			return 0
		}
	}
	return r
}

// rawItemString is item.Get(name).String() for a plain member name, without
// a copy of an unescaped value (rawJSONMemberString copies an escaped one;
// type values are short).
func rawItemString(item gjson.Result, name string) string {
	return rawJSONMemberString(firstRawJSONMember(item, name))
}

// rawItemIDTrimmedHasPrefix is
// strings.HasPrefix(strings.TrimSpace(item.Get("id").String()), prefix)
// for an ASCII prefix, without building the id.
func rawItemIDTrimmedHasPrefix(id gjson.Result, prefix string) bool {
	if id.Type != gjson.String {
		// A number, boolean, object, array or null: its String() starts
		// with a digit, '-', t, f, '{', '[' or is empty.
		return strings.HasPrefix(strings.TrimSpace(id.String()), prefix)
	}
	if strings.IndexByte(id.Raw, '\\') < 0 {
		return strings.HasPrefix(strings.TrimSpace(id.Raw[1:len(id.Raw)-1]), prefix)
	}
	var pending [utf8.UTFMax]byte
	np, matched, leading, ok := 0, 0, true, false
	rawJSONStringDecodedBytes(id.Raw, func(b byte) bool {
		if leading {
			pending[np] = b
			np++
			if !utf8.FullRune(pending[:np]) {
				return true
			}
			r, size := utf8.DecodeRune(pending[:np])
			if unicode.IsSpace(r) && size == np {
				np = 0
				return true
			}
			leading = false
			for _, p := range pending[:np] {
				if matched == len(prefix) {
					break
				}
				if p != prefix[matched] {
					return false
				}
				matched++
			}
		} else if b != prefix[matched] {
			return false
		} else {
			matched++
		}
		ok = matched == len(prefix)
		return !ok
	})
	return ok
}

// shouldStripOpenAIResponsesInputItemIDRaw is
// shouldStripOpenAIResponsesInputItemID(itemType, id.String()) for a string
// id, reading at most 65 decoded bytes of it.
func shouldStripOpenAIResponsesInputItemIDRaw(itemType string, id gjson.Result) bool {
	prefix, constrained := openAIResponsesInputItemIDPrefix(itemType)
	if !constrained {
		return false
	}
	var head [65]byte
	n := 0
	rawJSONStringDecodedBytes(id.Raw, func(b byte) bool {
		head[n] = b
		n++
		return n < len(head)
	})
	return n == 0 || n > 64 || n < len(prefix) || string(head[:len(prefix)]) != prefix
}

func openAIResponsesInputItemIDPrefix(itemType string) (string, bool) {
	switch strings.TrimSpace(itemType) {
	case "message":
		return "msg", true
	case "reasoning":
		return "rs", true
	case "web_search_call":
		return "ws", true
	case "custom_tool_call":
		return openAIResponsesToolCallIDPrefix(itemType), true
	case "tool_search_call":
		return openAIResponsesToolCallIDPrefix(itemType), true
	case "custom_tool_call_output":
		// Although custom calls use ctc IDs, OpenAI validates replayed custom
		// call output item IDs against the generic fc namespace.
		return "fc", true
	default:
		if isCodexToolCallInputType(itemType) {
			return openAIResponsesToolCallIDPrefix(itemType), true
		}
		return "", false
	}
}

func openAIResponsesToolCallIDPrefix(itemType string) string {
	switch strings.TrimSpace(itemType) {
	case "custom_tool_call", "custom_tool_call_output":
		return "ctc"
	case "tool_search_call", "tool_search_output":
		return "tsc"
	default:
		return "fc"
	}
}

// Invalid replayed IDs are removed rather than rewritten because a fabricated
// ID may point at a different upstream object.
func shouldStripOpenAIResponsesInputItemID(itemType, id string) bool {
	prefix, constrained := openAIResponsesInputItemIDPrefix(itemType)
	if !constrained {
		return false
	}
	return id == "" || len(id) > 64 || !strings.HasPrefix(id, prefix)
}

func shouldStripOpenAIResponsesNonPairCallID(itemType string) bool {
	switch strings.TrimSpace(itemType) {
	case "message", "reasoning", "image_generation_call":
		return true
	default:
		return false
	}
}

func sanitizeOpenAIResponsesInputItemIDs(body []byte) ([]byte, bool, error) {
	input := firstRawJSONMember(parseRawJSONView(body), "input")
	if !input.IsArray() {
		return body, false, nil
	}

	type inputItem struct {
		raw         string
		stripID     bool
		stripCallID bool
	}

	parse := func(item gjson.Result) inputItem {
		parsed := inputItem{raw: item.Raw}
		if item.IsObject() {
			// In place: no copy of an escaped id or call_id (rr11).
			id := firstRawJSONMember(item, "id")
			trimmedItemType := strings.TrimSpace(rawItemString(item, "type"))
			parsed.stripCallID = firstRawJSONMember(item, "call_id").Exists() && shouldStripOpenAIResponsesNonPairCallID(trimmedItemType)
			if id.Type == gjson.String {
				parsed.stripID = shouldStripOpenAIResponsesInputItemIDRaw(trimmedItemType, id)
			}
		}
		return parsed
	}
	// Find out first, keeping nothing, whether any item changes (review
	// rr3: the item list of a body of many small items cost about 7 x the
	// body on every passthrough request that changed nothing).
	hasSanitization := false
	input.ForEach(func(_, item gjson.Result) bool {
		parsed := parse(item)
		hasSanitization = parsed.stripID || parsed.stripCallID
		return !hasSanitization
	})
	if !hasSanitization {
		return body, false, nil
	}
	// One output buffer, no list of the items (rr6 follow-up: the two lists
	// cost about 3 x a body of many small items on the verified-raw path).
	index := -1
	out, err := rebuildOpenAIRawInput(body, input, func(raw gjson.Result) (string, bool, error) {
		index++
		item := parse(raw)
		if !item.stripID && !item.stripCallID {
			return item.raw, true, nil
		}
		edited := item.raw
		var err error
		if item.stripID {
			if edited, err = sjson.Delete(edited, "id"); err != nil {
				return "", false, fmt.Errorf("delete input.%d.id: %w", index, err)
			}
		}
		if item.stripCallID {
			if edited, err = sjson.Delete(edited, "call_id"); err != nil {
				return "", false, fmt.Errorf("delete input.%d.call_id: %w", index, err)
			}
		}
		return edited, true, nil
	})
	if err != nil {
		return nil, false, err
	}
	return out, true, nil
}
