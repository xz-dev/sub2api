package securityaudit

import (
	"bytes"
	"encoding/json"
	"strconv"
	"unicode"
	"unicode/utf16"
	"unicode/utf8"
	"unsafe"
)

// The prompt guard reads the request body in place (follow-up 6: a body of
// many small items cost about 12.6 × its size as json into any, one map per
// object). The body is validated once; then the extractors walk raw values
// (exact spans of the body) and decode only the strings they keep. Nothing
// is built per JSON object, so the guard's allocation follows the text it
// keeps, not the number of items.

// jsonRaw is the exact span of one JSON value of a validated document. A nil
// jsonRaw is an absent value (a missing object member).
type jsonRaw []byte

// validJSONDocument reports whether json.Unmarshal into any accepts b:
// json.Valid, and every number fits a float64 (json.Unmarshal into any
// rejects 1e400, json.Valid does not). Duplicate names are allowed (the last
// one wins, as in a map) and invalid UTF-8 is rejected by both.
func validJSONDocument(b []byte) bool {
	if !json.Valid(b) {
		return false
	}
	for i := 0; i < len(b); {
		switch c := b[i]; {
		case c == '"':
			i = jsonStringEnd(b, i)
		case c == '-' || (c >= '0' && c <= '9'):
			j := jsonScalarEnd(b, i)
			// Without an exponent, 20 digits are far below the float64 range.
			if num := b[i:j]; len(num) > 20 || bytes.ContainsAny(num, "eE") {
				if _, err := strconv.ParseFloat(unsafe.String(unsafe.SliceData(num), len(num)), 64); err != nil {
					return false
				}
			}
			i = j
		default:
			i++
		}
	}
	return true
}

func jsonSkipSpace(b []byte, i int) int {
	for i < len(b) && (b[i] == ' ' || b[i] == '\t' || b[i] == '\n' || b[i] == '\r') {
		i++
	}
	return i
}

// jsonStringEnd returns the index after the string literal starting at i.
func jsonStringEnd(b []byte, i int) int {
	for i++; i < len(b); {
		j := bytes.IndexAny(b[i:], "\"\\")
		if j < 0 {
			return len(b)
		}
		i += j
		if b[i] == '\\' {
			i += 2
			continue
		}
		return i + 1
	}
	return len(b)
}

func jsonScalarEnd(b []byte, i int) int {
	for i < len(b) {
		switch b[i] {
		case ',', ']', '}', ' ', '\t', '\n', '\r':
			return i
		}
		i++
	}
	return i
}

// jsonValueEnd returns the index after the value starting at i.
func jsonValueEnd(b []byte, i int) int {
	switch b[i] {
	case '"':
		return jsonStringEnd(b, i)
	case '{', '[':
		depth := 0
		for i < len(b) {
			switch b[i] {
			case '"':
				i = jsonStringEnd(b, i)
				continue
			case '{', '[':
				depth++
			case '}', ']':
				if depth--; depth == 0 {
					return i + 1
				}
			}
			i++
		}
		return len(b)
	default:
		return jsonScalarEnd(b, i)
	}
}

// jsonDocument is the root value of a validated document.
func jsonDocument(b []byte) jsonRaw {
	i := jsonSkipSpace(b, 0)
	if i == len(b) {
		return nil
	}
	return jsonRaw(b[i:jsonValueEnd(b, i)])
}

func (r jsonRaw) isString() bool { return len(r) > 0 && r[0] == '"' }
func (r jsonRaw) isArray() bool  { return len(r) > 0 && r[0] == '[' }
func (r jsonRaw) isObject() bool { return len(r) > 0 && r[0] == '{' }
func (r jsonRaw) isNull() bool   { return string(r) == "null" }

// jsonIter walks the elements of an array or the members of an object.
type jsonIter struct {
	b jsonRaw
	i int
}

// elements iterates r's elements; nothing when r is not an array.
func (r jsonRaw) elements() jsonIter {
	if !r.isArray() {
		return jsonIter{}
	}
	return jsonIter{b: r, i: jsonSkipSpace(r, 1)}
}

// members iterates r's members in document order; nothing when r is not an
// object.
func (r jsonRaw) members() jsonIter {
	if !r.isObject() {
		return jsonIter{}
	}
	return jsonIter{b: r, i: jsonSkipSpace(r, 1)}
}

func (it *jsonIter) value() jsonRaw {
	end := jsonValueEnd(it.b, it.i)
	v := it.b[it.i:end]
	if it.i = jsonSkipSpace(it.b, end); it.i < len(it.b) && it.b[it.i] == ',' {
		it.i = jsonSkipSpace(it.b, it.i+1)
	}
	return v
}

func (it *jsonIter) next() (jsonRaw, bool) {
	if it.i >= len(it.b) || it.b[it.i] == ']' || it.b[it.i] == '}' {
		return nil, false
	}
	return it.value(), true
}

// nextMember returns the raw name literal and the value of the next member.
func (it *jsonIter) nextMember() (name, value jsonRaw, ok bool) {
	if it.i >= len(it.b) || it.b[it.i] != '"' {
		return nil, nil, false
	}
	end := jsonStringEnd(it.b, it.i)
	name = it.b[it.i:end]
	it.i = jsonSkipSpace(it.b, jsonSkipSpace(it.b, end)+1)
	return name, it.value(), true
}

// field returns the value of the last member named key (the one a map
// keeps), or nil when r is not an object or has no such member.
func (r jsonRaw) field(key string) jsonRaw {
	var found jsonRaw
	for it := r.members(); ; {
		name, value, ok := it.nextMember()
		if !ok {
			return found
		}
		if jsonNameIs(name, key) {
			found = value
		}
	}
}

// fields is field for several keys in one walk; out[i] is keys[i]'s value.
func (r jsonRaw) fields(out []jsonRaw, keys ...string) {
	for it := r.members(); ; {
		name, value, ok := it.nextMember()
		if !ok {
			return
		}
		for i, key := range keys {
			if jsonNameIs(name, key) {
				out[i] = value
			}
		}
	}
}

// jsonNameIs reports whether the name literal decodes to key, without
// allocating: an escape takes at most 6 bytes per decoded byte, so a longer
// literal cannot match, and a shorter one decodes into a stack buffer.
func jsonNameIs(name jsonRaw, key string) bool {
	inner := name[1 : len(name)-1]
	if bytes.IndexByte(inner, '\\') < 0 {
		return string(inner) == key
	}
	var buf [128]byte
	if len(inner) > 6*len(key) || len(inner) > len(buf) {
		return false
	}
	return string(appendJSONUnquote(buf[:0], name)) == key
}

// appendJSONUnquote appends the decoded text of the string literal lit
// (validated) to dst, as json.Unmarshal decodes it: an unpaired surrogate
// escape becomes U+FFFD.
func appendJSONUnquote(dst []byte, lit []byte) []byte {
	s := lit[1 : len(lit)-1]
	for len(s) > 0 {
		i := bytes.IndexByte(s, '\\')
		if i < 0 {
			return append(dst, s...)
		}
		dst, s = append(dst, s[:i]...), s[i:]
		c := s[1]
		s = s[2:]
		switch c {
		case 'b':
			dst = append(dst, '\b')
		case 'f':
			dst = append(dst, '\f')
		case 'n':
			dst = append(dst, '\n')
		case 'r':
			dst = append(dst, '\r')
		case 't':
			dst = append(dst, '\t')
		case 'u':
			r := jsonHex4(s)
			s = s[4:]
			if utf16.IsSurrogate(r) {
				r2 := rune(-1)
				if len(s) >= 6 && s[0] == '\\' && s[1] == 'u' {
					r2 = jsonHex4(s[2:])
				}
				if pair := utf16.DecodeRune(r, r2); pair != unicode.ReplacementChar {
					r, s = pair, s[6:]
				} else {
					r = unicode.ReplacementChar
				}
			}
			dst = utf8.AppendRune(dst, r)
		default: // '"', '\\', '/'
			dst = append(dst, c)
		}
	}
	return dst
}

func jsonHex4(s []byte) rune {
	var r rune
	for _, c := range s[:4] {
		switch {
		case c >= '0' && c <= '9':
			c -= '0'
		case c >= 'a' && c <= 'f':
			c -= 'a' - 10
		default:
			c -= 'A' - 10
		}
		r = r<<4 | rune(c)
	}
	return r
}
