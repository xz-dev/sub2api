// Package bodyspool implements the large-request-capacity skeleton + blob
// spool contract (design D2/D3/D4) for Go hops (Sub2API now, CPA later):
//
//   - the marker/nonce contract and the tier-A ingest tokenizer;
//   - the splice reader that puts blobs back at egress;
//   - anonymous on-disk spool files with bounded page cache;
//   - the capacity-ticket lease client;
//   - the per-hop memory charge against the hop's own cgroup.
//
// The package has no dependency on the rest of the fork so that it can be
// copied as a unit.
package bodyspool

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"strings"
)

// Marker contract.
//
// A blob is replaced inside its JSON string by
//
//	\u0000 lrc <32 lowercase hex nonce> : <decimal blob index> \u0000
//
// (no spaces), written as JSON escapes so the skeleton stays valid JSON and
// survives gjson/sjson edits and encoding/json decode+marshal unchanged
// (encoding/json writes U+0000 back as \u0000). Decoded Go strings carry the
// raw form, NUL tag idx NUL, which ExpandString also resolves.
//
// The nonce is 128 random bits chosen per request before the scan. A body
// that already contains the nonce text fails closed with ErrMarkerCollision.
// A marker that a rewrite changed (for example re-escaped into \\u0000) or
// whose index is unknown fails closed with ErrMarkerUnresolved: nothing is
// sent upstream.
const (
	NonceBytes   = 16
	markerEsc    = `\u0000`
	markerPrefix = "lrc"
)

var (
	// ErrMarkerCollision: the body already contains the request nonce.
	ErrMarkerCollision = errors.New("bodyspool: body contains the marker nonce")
	// ErrMarkerUnresolved: the skeleton holds a marker the splicer cannot resolve.
	ErrMarkerUnresolved = errors.New("bodyspool: unresolved or mangled marker in skeleton")
)

// Nonce is the per-request marker nonce.
type Nonce [NonceBytes]byte

// newNonce is replaceable in tests (collision tests need a known nonce).
var newNonce = NewNonce

// NewNonce draws a fresh 128-bit nonce.
func NewNonce() (Nonce, error) {
	var n Nonce
	_, err := rand.Read(n[:])
	return n, err
}

// Hex is the nonce text searched for collisions.
func (n Nonce) Hex() string { return hex.EncodeToString(n[:]) }

// tag is "lrc<hex>:"; it is preceded by \u0000 (escaped) or NUL (raw).
func (n Nonce) tag() []byte { return []byte(markerPrefix + n.Hex() + ":") }

// MayHoldMarker reports whether the JSON text holds what may be a marker of
// any spool (its escaped start, \u0000lrc). It needs no spool, so it gives
// the same answer on a skeleton and on a body that was never spooled.
func MayHoldMarker(jsonText string) bool {
	return strings.Contains(jsonText, markerEsc+markerPrefix)
}

// appendMarker appends the escaped marker for blob idx.
func (n Nonce) appendMarker(dst []byte, idx int) []byte {
	dst = append(dst, markerEsc...)
	dst = append(dst, n.tag()...)
	dst = appendDecimal(dst, idx)
	return append(dst, markerEsc...)
}

func appendDecimal(dst []byte, v int) []byte {
	var buf [20]byte
	i := len(buf)
	for {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
		if v == 0 {
			break
		}
	}
	return append(dst, buf[i:]...)
}

// matcher finds a fixed pattern in a byte stream (KMP), so the collision
// check works across read boundaries.
type matcher struct {
	pat  []byte
	fail []int
	k    int
}

func newMatcher(pat []byte) *matcher {
	fail := make([]int, len(pat))
	for i, k := 1, 0; i < len(pat); i++ {
		for k > 0 && pat[i] != pat[k] {
			k = fail[k-1]
		}
		if pat[i] == pat[k] {
			k++
		}
		fail[i] = k
	}
	return &matcher{pat: pat, fail: fail}
}

// feed reports whether the pattern ends at c.
func (m *matcher) feed(c byte) bool {
	for m.k > 0 && c != m.pat[m.k] {
		m.k = m.fail[m.k-1]
	}
	if c == m.pat[m.k] {
		m.k++
	}
	if m.k == len(m.pat) {
		m.k = m.fail[m.k-1]
		return true
	}
	return false
}

// canonicalBlobByte reports whether b is written identically by raw
// gjson/sjson copies, Go encoding/json (which escapes < > & and controls) and
// serde: printable ASCII except space, quote, backslash, < > &. Only strings
// made of these bytes are externalized, so the splice output equals what
// every serializer would write and needs no re-escaping (splice escaping
// rule). Base64 and data: URLs are canonical; anything else stays inline.
func canonicalBlobByte(b byte) bool {
	if b <= ' ' || b >= 0x7f {
		return false
	}
	switch b {
	case '"', '\\', '<', '>', '&':
		return false
	}
	return true
}
