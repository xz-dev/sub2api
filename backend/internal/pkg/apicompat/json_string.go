package apicompat

import (
	"unicode/utf8"
)

// marshalJSONString returns the same bytes as json.Marshal(s) in one
// allocation of the exact size. json.Marshal grows its buffer while it
// escapes, which cost up to ~30 x a large text of '<' on the Responses ->
// Chat conversion (rr10, adv_developer_part_lt); each '<', '>' and '&' is
// six bytes out (HTML escaping, as json.Marshal does).
func marshalJSONString(s string) []byte {
	n := 2
	for i := 0; i < len(s); {
		c := s[i]
		if c < utf8.RuneSelf {
			n += jsonASCIILen(c)
			i++
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		switch {
		case r == utf8.RuneError && size == 1:
			n += 3 // U+FFFD, as json.Marshal writes it
		case r == '\u2028' || r == '\u2029':
			n += 6
		default:
			n += size
		}
		i += size
	}
	out := make([]byte, 0, n)
	out = append(out, '"')
	const hex = "0123456789abcdef"
	start := 0
	for i := 0; i < len(s); {
		c := s[i]
		if c < utf8.RuneSelf {
			if jsonASCIILen(c) == 1 {
				i++
				continue
			}
			out = append(out, s[start:i]...)
			switch c {
			case '"', '\\':
				out = append(out, '\\', c)
			case '\b':
				out = append(out, '\\', 'b')
			case '\f':
				out = append(out, '\\', 'f')
			case '\n':
				out = append(out, '\\', 'n')
			case '\r':
				out = append(out, '\\', 'r')
			case '\t':
				out = append(out, '\\', 't')
			default:
				out = append(out, '\\', 'u', '0', '0', hex[c>>4], hex[c&0xf])
			}
			i++
			start = i
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		switch {
		case r == utf8.RuneError && size == 1:
			out = append(out, s[start:i]...)
			out = append(out, "\uFFFD"...)
		case r == '\u2028' || r == '\u2029':
			out = append(out, s[start:i]...)
			out = append(out, '\\', 'u', '2', '0', '2', hex[r&0xf])
		default:
			i += size
			continue
		}
		i += size
		start = i
	}
	out = append(out, s[start:]...)
	return append(out, '"')
}

// jsonASCIILen: the bytes json.Marshal writes for the ASCII byte c.
func jsonASCIILen(c byte) int {
	switch {
	case c == '"' || c == '\\' || c == '\b' || c == '\f' || c == '\n' || c == '\r' || c == '\t':
		return 2
	case c < 0x20 || c == '<' || c == '>' || c == '&':
		return 6
	}
	return 1
}
