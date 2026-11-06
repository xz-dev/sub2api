package service

import (
	"encoding/json"
	"strings"
	"unicode"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/Wei-Shaw/sub2api/internal/pkg/bodyspool"
	"github.com/tidwall/gjson"
)

// derivedKeyBudgetBytes is the work one derivation of a key from request
// bytes may do on the verified-raw path (rr10): the content session seed
// (deriveOpenAIContentSessionSeed) and the cyber transcript keys
// (deriveOpenAICyberTranscriptBlockKeys) each get one derivedKeyBudget of
// this size. Every canonical form (decode and re-encode), every unescape
// and every re-escape they make of request bytes is paid from it: the
// bytes read, contentSeedValueCost per JSON value read, and the bytes
// written. A value that does not fit, or that alone is above
// contentSeedDecodeMaxBytes or contentSeedDecodeMaxValues, is used as sent
// (the "raw:" form in the seed, the raw bytes in the transcript), which
// allocates nothing beyond the copy into the key.
//
// Per-site caps alone did not bound a request (rr9 review: thousands of
// developer items of 4-32 KiB of '<' each passed the 32 KiB cap and cost
// k 19-24, 258 of 301 MB in the seed's canonical copies). With the budget a
// derivation allocates O(derivedKeyBudgetBytes), whatever the body: about
// 5.4 x the budget for strings of '<' (32 KiB in, 192 KiB out) plus one
// value past it.
//
// One budget per derivation, not one shared by the whole request: the
// seed is derived several times per request (handler, Forward, retries)
// and must come out the same each time, and with or without the session
// block, so a derivation cannot depend on what ran before it.
//
// A value's decision depends only on its own bytes and on the values
// before it, so it is the same on the skeleton and on the whole body: a
// value with a spooled string holds a marker on the skeleton and is above
// contentSeedDecodeMaxBytes on the whole body (T >= 32 KiB), and both
// are used as sent without paying; every other value is the same bytes on
// both.
const derivedKeyBudgetBytes = 128 << 10

// contentSeedValueCost: what one decoded JSON value costs the budget
// (a decode allocates about 150 bytes per value).
const contentSeedValueCost = 32

// derivedKeyBudget is one derivation's budget; see derivedKeyBudgetBytes.
type derivedKeyBudget struct{ left int }

func newDerivedKeyBudget() *derivedKeyBudget {
	return &derivedKeyBudget{left: derivedKeyBudgetBytes}
}

// canonicalJSON is the canonical form of the JSON text raw
// (normalizeCompatSeedJSON: decoded and re-encoded, keys sorted) when raw
// is within contentSeedDecodeMaxBytes and contentSeedDecodeMaxValues,
// holds no spool marker and its bytes and values fit in what is left;
// ok=false otherwise, and the caller uses raw as sent.
func (b *derivedKeyBudget) canonicalJSON(raw string) (string, bool) {
	if len(raw) > contentSeedDecodeMaxBytes || bodyspool.MayHoldMarker(raw) {
		return "", false
	}
	values := rawJSONValueCount(raw, contentSeedDecodeMaxValues)
	if values > contentSeedDecodeMaxValues {
		return "", false
	}
	in := len(raw) + contentSeedValueCost*values
	if in > b.left {
		return "", false
	}
	out := normalizeCompatSeedJSON(rawJSONView(raw))
	b.left -= in + len(out)
	return out, true
}

// stringValue is gjson's String of v, a member value from
// forEachRawJSONMember or firstRawJSONMember (whose Str is not filled).
// Free when v is not a string (a number is already parsed, bounded; an
// object or array is its Raw), when it has no escape (a view of Raw), or
// when it is the small skeleton form of a spooled string (an escaped
// marker; its whole-body form has no escape, so neither pays). Otherwise
// it is unescaped when Raw is within contentSeedDecodeMaxBytes and fits,
// else ok=false and the caller uses Raw as sent.
func (b *derivedKeyBudget) stringValue(v gjson.Result) (string, bool) {
	raw := v.Raw
	if v.Type != gjson.String || len(raw) < 2 {
		return v.String(), true
	}
	if strings.IndexByte(raw, '\\') < 0 {
		return raw[1 : len(raw)-1], true
	}
	if len(raw) > contentSeedDecodeMaxBytes {
		return "", false
	}
	if bodyspool.MayHoldMarker(raw) {
		return gjson.Parse(raw).Str, true
	}
	if len(raw) > b.left {
		return "", false
	}
	s := gjson.Parse(raw).Str
	b.left -= len(raw) + len(s)
	return s, true
}

// marshalString is json.Marshal of the value of the JSON string v (the
// transcript's form of a string), paid like stringValue plus the bytes
// written; ok=false: use v.Raw as sent.
func (b *derivedKeyBudget) marshalString(v gjson.Result) (string, bool) {
	if raw := v.Raw; len(raw) > contentSeedDecodeMaxBytes || bodyspool.MayHoldMarker(raw) || len(raw) > b.left {
		return "", false
	}
	s, _ := b.stringValue(v)
	encoded, _ := json.Marshal(s)
	b.left -= len(encoded)
	return string(encoded), true
}

// rawJSONValueCount is the number of values of the JSON text raw (the
// '{', '[' and ',' outside strings, plus one), counted up to max+1.
func rawJSONValueCount(raw string, max int) int {
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
				return n
			}
		}
	}
	return n
}

// rawJSONStringBlank reports whether strings.TrimSpace of gjson's value of
// the JSON string raw is "", without unescaping it: runes are decoded in
// place exactly as gjson unescapes them (an escaped string stops at a
// control byte, an unknown escape or a short \u escape, dropping the rest;
// a surrogate takes the next \u escape whatever it is).
func rawJSONStringBlank(raw string) bool {
	if len(raw) < 2 || raw[0] != '"' {
		return false
	}
	s := raw[1 : len(raw)-1]
	escaped := strings.IndexByte(s, '\\') >= 0
	for i := 0; i < len(s); {
		c := s[i]
		if !escaped || (c != '\\' && c >= ' ') {
			r, n := utf8.DecodeRuneInString(s[i:])
			if !unicode.IsSpace(r) {
				return false
			}
			i += n
			continue
		}
		if c < ' ' {
			return true
		}
		i++
		if i >= len(s) {
			return true
		}
		var r rune
		switch s[i] {
		case '\\', '/', '"', 'b':
			r = 'x' // not space
		case 'f':
			r = '\f'
		case 'n':
			r = '\n'
		case 'r':
			r = '\r'
		case 't':
			r = '\t'
		case 'u':
			if i+5 > len(s) {
				return true
			}
			r = rawJSONHex4(s[i+1 : i+5])
			i += 4
			if utf16.IsSurrogate(r) && len(s[i+1:]) >= 6 && s[i+1] == '\\' && s[i+2] == 'u' {
				r = utf16.DecodeRune(r, rawJSONHex4(s[i+3:i+7]))
				i += 6
			}
		default:
			return true
		}
		i++
		if !unicode.IsSpace(r) {
			return false
		}
	}
	return true
}

// rawJSONHex4 decodes four hex digits as gjson does (0 when one is not
// hex).
func rawJSONHex4(h string) rune {
	var r rune
	for i := 0; i < 4; i++ {
		c := h[i]
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
