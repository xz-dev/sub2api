package bodyspool

import (
	"bytes"
	"io"
	"strconv"
	"strings"
	"unsafe"
)

// Reader returns the egress body for a (possibly rewritten) skeleton: each
// escaped marker is replaced by its blob, read from the spool with
// positional reads, so concurrent and repeated attempts are independent.
// The length is exact (for Content-Length). Every occurrence of the nonce
// tag must be a well-formed escaped marker with a known index; otherwise
// ErrMarkerUnresolved is returned and nothing may be sent.
func (s *Spool) Reader(skel []byte) (io.Reader, int64, error) {
	var parts []io.Reader
	var n int64
	err := s.walk(skel, false, func(lit []byte, b *blob) {
		if len(lit) > 0 {
			parts = append(parts, bytes.NewReader(lit))
			n += int64(len(lit))
		}
		if b != nil {
			parts = append(parts, s.file.Section(b.off, b.end-b.off))
			n += b.end - b.off
		}
	})
	if err != nil {
		return nil, 0, err
	}
	return io.MultiReader(parts...), n, nil
}

// Contains reports whether text holds a marker of this spool.
func (s *Spool) Contains(text string) bool {
	return s != nil && strings.Contains(text, string(s.tag))
}

// WriteExpanded writes text to w with every marker, escaped (\u0000…) or
// decoded (NUL…NUL, as in a Go string taken from the skeleton), replaced by
// its blob. Blobs are canonical, so decoded and raw bytes are equal. Used to
// hash or measure content exactly as the legacy path saw it.
func (s *Spool) WriteExpanded(w io.Writer, text string) error {
	var werr error
	// walk and io.Writer.Write only read the bytes: view text without a copy.
	err := s.walk(unsafe.Slice(unsafe.StringData(text), len(text)), true, func(lit []byte, b *blob) {
		if werr != nil {
			return
		}
		if len(lit) > 0 {
			_, werr = w.Write(lit)
		}
		if b != nil && werr == nil {
			_, werr = io.Copy(w, s.file.Section(b.off, b.end-b.off))
		}
	})
	if err != nil {
		return err
	}
	return werr
}

// walk splits src into literal runs and blobs. raw also accepts NUL-framed
// markers.
func (s *Spool) walk(src []byte, raw bool, emit func(lit []byte, b *blob)) error {
	i := 0
	for {
		j := bytes.Index(src[i:], s.tag)
		if j < 0 {
			emit(src[i:], nil)
			return nil
		}
		j += i
		var open, close string
		switch {
		case j-len(markerEsc) >= i && string(src[j-len(markerEsc):j]) == markerEsc && !oddBackslashes(src[:j-len(markerEsc)]):
			open, close = markerEsc, markerEsc
		case raw && j-1 >= i && src[j-1] == 0:
			open, close = "\x00", "\x00"
		default:
			return ErrMarkerUnresolved
		}
		k := j + len(s.tag)
		e := k
		for e < len(src) && e-k < 10 && src[e] >= '0' && src[e] <= '9' {
			e++
		}
		idx, err := strconv.Atoi(string(src[k:e]))
		if err != nil || idx < 0 || idx >= len(s.blobs) || !bytes.HasPrefix(src[e:], []byte(close)) {
			return ErrMarkerUnresolved
		}
		emit(src[i:j-len(open)], &s.blobs[idx])
		i = e + len(close)
	}
}

// oddBackslashes reports whether b ends in an odd run of backslashes, which
// means the following \u0000 was itself escaped by a rewrite.
func oddBackslashes(b []byte) bool {
	n := 0
	for i := len(b) - 1; i >= 0 && b[i] == '\\'; i-- {
		n++
	}
	return n%2 == 1
}
