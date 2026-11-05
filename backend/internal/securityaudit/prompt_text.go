package securityaudit

import (
	"crypto/sha256"
	"strings"
	"unicode"
	"unicode/utf8"
)

// promptPieces is the text texts[0] + joiner + texts[1] + ..., read in place.
// The snapshot computes the metadata text's hash, rune count, preview head
// and stored prompt from it without ever building the joined text (review
// a4a7d843 blocker 2: the joined copy, a []rune copy of it and regex passes
// over it made the guard's cost a multiple of the body).
type promptPieces struct {
	texts  []string
	joiner string
}

// each calls f with every piece of the text in order, joiners included, until
// f returns false.
func (p promptPieces) each(f func(string) bool) {
	for index, text := range p.texts {
		if index > 0 && p.joiner != "" && !f(p.joiner) {
			return
		}
		if !f(text) {
			return
		}
	}
}

// sha256 is sha256.Sum256 of the joined text, hashed through a small buffer.
func (p promptPieces) sha256() [sha256.Size]byte {
	h := sha256.New()
	var buf [8 << 10]byte
	p.each(func(s string) bool {
		for len(s) > 0 {
			n := copy(buf[:], s)
			_, _ = h.Write(buf[:n])
			s = s[n:]
		}
		return true
	})
	var sum [sha256.Size]byte
	h.Sum(sum[:0])
	return sum
}

// runeCount is utf8.RuneCountInString of the joined text. The joiner is
// ASCII, so no rune spans two pieces.
func (p promptPieces) runeCount() int {
	count := 0
	p.each(func(s string) bool {
		count += utf8.RuneCountInString(s)
		return true
	})
	return count
}

// head is the joined text's first n bytes at most, cut back to a rune start.
// It copies at most n bytes, and nothing when the first piece is long enough.
func (p promptPieces) head(n int) string {
	if len(p.texts) > 0 && len(p.texts[0]) >= n {
		return cutToRuneStart(p.texts[0], n)
	}
	var b strings.Builder
	p.each(func(s string) bool {
		if room := n - b.Len(); len(s) > room {
			b.WriteString(cutToRuneStart(s, room))
			return false
		}
		b.WriteString(s)
		return true
	})
	return b.String()
}

func cutToRuneStart(s string, n int) string {
	if n >= len(s) {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// fullPrompt is BuildFullPrompt of the joined text: NUL bytes stripped,
// spaces trimmed, at most maxRunes runes and then "…". It walks the pieces
// and copies only the runes it keeps.
func (p promptPieces) fullPrompt(maxRunes int) string {
	if maxRunes <= 0 {
		maxRunes = DefaultFullPromptMaxRunes
	}
	valid := true
	p.each(func(s string) bool {
		valid = utf8.ValidString(s)
		return valid
	})
	if !valid {
		// Stripping NULs can join invalid bytes into a rune; keep the exact
		// legacy result for such text (JSON-decoded text is always valid).
		var b strings.Builder
		p.each(func(s string) bool { b.WriteString(s); return true })
		return TrimRunes(strings.TrimSpace(strings.ReplaceAll(b.String(), "\x00", "")), maxRunes)
	}
	// Two walks: the first finds how many bytes are kept, so the second
	// allocates exactly that (at most maxRunes runes).
	walk := func(b *strings.Builder, limit int) (size int, truncated bool) {
		started := false
		count, written := 0, 0 // runes and bytes kept so far
		p.each(func(s string) bool {
			for len(s) > 0 {
				r, n := utf8.DecodeRuneInString(s)
				piece := s[:n]
				s = s[n:]
				if r == 0 {
					continue
				}
				space := unicode.IsSpace(r)
				if !started {
					if space {
						continue
					}
					started = true
				}
				if count == maxRunes {
					if space {
						continue
					}
					truncated = true
					return false
				}
				count++
				written += n
				if b != nil && written <= limit {
					b.WriteString(piece)
				}
				if !space {
					size = written
				}
			}
			return true
		})
		if truncated {
			size = written
		}
		return size, truncated
	}
	size, truncated := walk(nil, 0)
	var b strings.Builder
	if truncated {
		b.Grow(size + len("…"))
		walk(&b, size)
		b.WriteString("…")
	} else {
		b.Grow(size)
		walk(&b, size)
	}
	return b.String()
}
