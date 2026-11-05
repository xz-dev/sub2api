package bodyspool

import (
	"bytes"
	"fmt"
	"io"
)

// Ingest defaults (design D2).
const (
	DefaultThreshold   = 64 << 10 // T
	DefaultKeepPrefix  = 64       // raw bytes kept inline before the marker
	DefaultChargeStep  = 1 << 20  // skeleton growth per Charge call
	readChunk          = 256 << 10
	maxTrackedKeyBytes = 32
	maxTrackedDepth    = 64 // deeper values are never tier A (bounded scratch)
)

// Options configure one ingest.
type Options struct {
	Threshold   int   // T; a tier-A string of at least T bytes is spooled
	KeepPrefix  int   // raw bytes of a spooled string kept inline
	InlineLimit int64 // S; skeleton bytes above it fail with *InlineLimitError
	ChargeStep  int64 // skeleton bytes per Charge call
	// Charge is called with ChargeStep before the skeleton may grow by that
	// many bytes; an error stops the ingest.
	Charge func(inlineBytes int64) error
	// Observe is called with the raw byte total after each read; an error
	// stops the ingest before the next read.
	Observe func(rawBytes int64) error
	// SizeHint, when positive, is the skeleton capacity allocated up front
	// (capped at InlineLimit), so the skeleton never regrows. The caller must
	// already have charged it.
	SizeHint int64
}

// InlineLimitError: the inline bytes (skeleton) passed the limit.
type InlineLimitError struct{ Limit int64 }

func (e *InlineLimitError) Error() string {
	return fmt.Sprintf("bodyspool: inline bytes exceed the %d-byte inline-bytes limit", e.Limit)
}

type blob struct{ off, end int64 }

// Spool is one request's spooled body: the raw body in an anonymous file
// plus the blob ranges the skeleton's markers point at.
type Spool struct {
	file  *File
	nonce Nonce
	tag   []byte
	blobs []blob
	size  int64
}

// Size is the raw body length.
func (s *Spool) Size() int64 { return s.size }

// Blobs is the number of externalized strings.
func (s *Spool) Blobs() int { return len(s.blobs) }

// Close frees the spool file.
func (s *Spool) Close() error { return s.file.Close() }

// frame is one open JSON container.
type frame struct {
	obj       bool
	key       []byte // last key of this object (nil when untracked)
	parentKey []byte // key under which this container is a value
}

// Tier-A allow-list for Sub2API Responses bodies (design D2). Any value that
// is a data: URL is also tier A.
func tierAKey(key, parent []byte) bool {
	switch string(key) {
	case "image_url", "file_data":
		return true
	case "url":
		return string(parent) == "image_url"
	case "data":
		return string(parent) == "source"
	}
	return false
}

const (
	strPending = iota // bytes buffered while shorter than T
	strInline         // copied straight into the skeleton
	strSpool          // only in the spool file
)

// Ingest streams r into file and returns the spool and the skeleton: every
// canonical tier-A string of at least T bytes is replaced by KeepPrefix raw
// bytes plus a marker. All other bytes stay inline and count against S.
// Scratch memory is the skeleton, T bytes of string and the read buffer.
func Ingest(r io.Reader, file *File, opt Options) (*Spool, []byte, error) {
	if opt.Threshold <= 0 {
		opt.Threshold = DefaultThreshold
	}
	if opt.KeepPrefix < 0 || opt.KeepPrefix > opt.Threshold {
		opt.KeepPrefix = DefaultKeepPrefix
	}
	if opt.ChargeStep <= 0 {
		opt.ChargeStep = DefaultChargeStep
	}
	nonce, err := newNonce()
	if err != nil {
		return nil, nil, err
	}
	in := &ingest{opt: opt, sp: &Spool{file: file, nonce: nonce, tag: nonce.tag()}, collide: newMatcher([]byte(nonce.Hex()))}
	if hint := opt.SizeHint; hint > 0 {
		if opt.InlineLimit > 0 {
			hint = min(hint, opt.InlineLimit)
		}
		in.skel = make([]byte, 0, hint)
	}
	if err := in.run(r); err != nil {
		return nil, nil, err
	}
	return in.sp, in.skel, nil
}

type ingest struct {
	opt     Options
	sp      *Spool
	collide *matcher
	skel    []byte
	charged int64

	stack     []frame
	deep      int // containers open beyond maxTrackedDepth
	expectKey bool

	inStr, esc, isKey, canon, tierAKey bool
	mode                               int
	str                                []byte
	strStart, strLen, blobStart        int64
}

func (in *ingest) run(r io.Reader) error {
	buf := make([]byte, readChunk)
	var pos int64
	for {
		n, rerr := r.Read(buf)
		if n > 0 {
			if _, err := in.sp.file.Write(buf[:n]); err != nil {
				return err
			}
			for i, c := range buf[:n] {
				if in.collide.feed(c) {
					return ErrMarkerCollision
				}
				if err := in.byte(c, pos+int64(i)); err != nil {
					return err
				}
			}
			pos += int64(n)
			in.sp.size = pos
			if in.opt.Observe != nil {
				if err := in.opt.Observe(pos); err != nil {
					return err
				}
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return rerr
		}
	}
	if in.inStr && in.mode == strPending {
		return in.emit(in.str...) // unterminated: the parser rejects the skeleton
	}
	if in.inStr && in.mode == strSpool {
		return in.endBlob(pos, false) // unterminated: the parser rejects the skeleton
	}
	return nil
}

// byte advances the tokenizer over raw byte c at file offset pos.
func (in *ingest) byte(c byte, pos int64) error {
	if !in.inStr {
		if err := in.emit(c); err != nil {
			return err
		}
		in.structural(c, pos)
		return nil
	}
	if in.mode == strSpool {
		if c == '"' {
			return in.endBlob(pos, true)
		}
		if canonicalBlobByte(c) {
			return nil
		}
		if err := in.demote(pos); err != nil { // not canonical: back inline
			return err
		}
	}
	closing := false
	switch {
	case in.esc:
		in.esc = false
	case c == '\\':
		in.esc = true
	case c == '"':
		closing = true
	}
	if closing {
		return in.endString()
	}
	in.canon = in.canon && canonicalBlobByte(c)
	in.strLen++
	if in.mode == strInline {
		return in.emit(c)
	}
	in.str = append(in.str, c)
	if in.strLen < int64(in.opt.Threshold) {
		return nil
	}
	// The string reached T: spool it if it is canonical tier A, else inline.
	if !in.isKey && in.deep == 0 && in.canon && (in.tierAKey || bytes.HasPrefix(in.str, []byte("data:"))) {
		keep := in.opt.KeepPrefix
		if err := in.emit(in.str[:keep]...); err != nil {
			return err
		}
		in.blobStart = in.strStart + int64(keep)
		in.mode = strSpool
	} else {
		if err := in.emit(in.str...); err != nil {
			return err
		}
		in.mode = strInline
	}
	in.str = in.str[:0]
	return nil
}

func (in *ingest) structural(c byte, pos int64) {
	switch c {
	case '{', '[':
		if in.deep > 0 || len(in.stack) >= maxTrackedDepth {
			in.deep++
			return
		}
		var parent []byte
		if n := len(in.stack); n > 0 {
			if in.stack[n-1].obj {
				parent = in.stack[n-1].key
			} else {
				parent = in.stack[n-1].parentKey
			}
		}
		in.stack = append(in.stack, frame{obj: c == '{', parentKey: parent})
		in.expectKey = c == '{'
	case '}', ']':
		if in.deep > 0 {
			in.deep--
			return
		}
		if n := len(in.stack); n > 0 {
			in.stack = in.stack[:n-1]
		}
		in.expectKey = false
	case ':':
		in.expectKey = false
	case ',':
		in.expectKey = in.deep == 0 && len(in.stack) > 0 && in.stack[len(in.stack)-1].obj
	case '"':
		top := (*frame)(nil)
		if n := len(in.stack); n > 0 && in.deep == 0 {
			top = &in.stack[n-1]
		}
		in.inStr, in.esc, in.canon = true, false, true
		in.isKey = top != nil && top.obj && in.expectKey
		in.tierAKey = !in.isKey && top != nil && top.obj && tierAKey(top.key, top.parentKey)
		in.mode, in.str, in.strLen, in.strStart = strPending, in.str[:0], 0, pos+1
	}
}

// endString closes a pending or inline string at its closing quote.
func (in *ingest) endString() error {
	in.inStr = false
	if in.mode == strPending {
		if in.isKey {
			top := &in.stack[len(in.stack)-1]
			if len(in.str) <= maxTrackedKeyBytes {
				top.key = append(top.key[:0], in.str...)
			} else {
				top.key = nil
			}
		}
		if err := in.emit(in.str...); err != nil {
			return err
		}
	} else if in.isKey {
		in.stack[len(in.stack)-1].key = nil // longer than T: never a tier-A key
	}
	in.str = in.str[:0]
	return in.emit('"')
}

// endBlob records the blob ending at pos and writes its marker.
func (in *ingest) endBlob(pos int64, closed bool) error {
	in.inStr = false
	in.sp.blobs = append(in.sp.blobs, blob{off: in.blobStart, end: pos})
	if err := in.emit(in.sp.nonce.appendMarker(nil, len(in.sp.blobs)-1)...); err != nil {
		return err
	}
	if closed {
		return in.emit('"')
	}
	return nil
}

// demote copies a spooled string's bytes [blobStart, pos) back into the
// skeleton and continues it inline.
func (in *ingest) demote(pos int64) error {
	var buf [32 << 10]byte
	for off := in.blobStart; off < pos; {
		n := int64(len(buf))
		if rem := pos - off; rem < n {
			n = rem
		}
		if _, err := in.sp.file.ReadAt(buf[:n], off); err != nil {
			return err
		}
		if err := in.emit(buf[:n]...); err != nil {
			return err
		}
		off += n
	}
	in.mode, in.canon = strInline, false
	return nil
}

// emit appends inline bytes, enforcing S and charging before growth.
func (in *ingest) emit(p ...byte) error {
	need := int64(len(in.skel) + len(p))
	if in.opt.InlineLimit > 0 && need > in.opt.InlineLimit {
		return &InlineLimitError{Limit: in.opt.InlineLimit}
	}
	for in.charged < need {
		if in.opt.Charge != nil {
			if err := in.opt.Charge(in.opt.ChargeStep); err != nil {
				return err
			}
		}
		in.charged += in.opt.ChargeStep
	}
	in.skel = append(in.skel, p...)
	return nil
}
