package service

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"net/http"
	"strings"
	"unicode"
	"unsafe"

	"github.com/Wei-Shaw/sub2api/internal/pkg/bodyspool"
	"github.com/cespare/xxhash/v2"
	"github.com/gin-gonic/gin"
)

// Large-request-capacity body spool (design D2/D4) on the service side: the
// handler ingests the body into a spool and hands every later stage the
// small skeleton; egress splices the blobs back, and the session hash and
// content seed are computed over the expanded bytes.

type requestBodySpoolKey struct{}

type requestBodySpool struct {
	sp     *bodyspool.Spool
	ticket string
}

// WithRequestBodySpool attaches the request's spool and leased ticket id.
func WithRequestBodySpool(ctx context.Context, sp *bodyspool.Spool, ticket string) context.Context {
	return context.WithValue(ctx, requestBodySpoolKey{}, requestBodySpool{sp: sp, ticket: ticket})
}

func requestBodySpoolFromContext(ctx context.Context) (*bodyspool.Spool, string) {
	if ctx == nil {
		return nil, ""
	}
	v, _ := ctx.Value(requestBodySpoolKey{}).(requestBodySpool)
	return v.sp, v.ticket
}

func requestBodySpoolFromGin(c *gin.Context) (*bodyspool.Spool, string) {
	if c == nil || c.Request == nil {
		return nil, ""
	}
	return requestBodySpoolFromContext(c.Request.Context())
}

// RequestBodyLogicalSize is the size of body with its blobs spliced back.
func RequestBodyLogicalSize(c *gin.Context, body []byte) int {
	if sp, _ := requestBodySpoolFromGin(c); sp != nil {
		if _, n, err := sp.Reader(body); err == nil {
			return int(n)
		}
	}
	return len(body)
}

// deriveOpenAISessionHashesForRequest hashes the expanded session seed, so
// a seed taken from the skeleton hashes exactly like the legacy body.
func deriveOpenAISessionHashesForRequest(c *gin.Context, sessionID string) (string, string) {
	normalized := strings.TrimSpace(sessionID)
	if sp, _ := requestBodySpoolFromGin(c); sp.Contains(normalized) {
		xh, sh := xxhash.New(), sha256.New()
		if err := sp.WriteExpanded(io.MultiWriter(xh, sh), normalized); err == nil {
			return fmt.Sprintf("%016x", xh.Sum64()), hex.EncodeToString(sh.Sum(nil))
		}
	}
	return deriveOpenAISessionHashes(sessionID)
}

// hashOpenAIContentSessionSeed is
// deriveOpenAISessionHashesForRequest(c, deriveOpenAIContentSessionSeed(body))
// computed while the seed is written, so the seed, as large as the body
// when it has raw: fields, is never held (rr10). ok=false: no seed.
func hashOpenAIContentSessionSeed(c *gin.Context, body []byte) (currentHash, legacyHash string, ok bool) {
	fields, ok := scanContentSessionSeedFields(body)
	if !ok {
		return "", "", false
	}
	sp, _ := requestBodySpoolFromGin(c)
	h := newSeedHashWriter(sp)
	_, _ = h.WriteString(contentSessionSeedPrefix)
	if !writeContentSessionSeed(h, &fields) {
		return "", "", false
	}
	currentHash, legacyHash = h.sums()
	return currentHash, legacyHash, true
}

// seedHashWriter hashes a session seed as deriveOpenAISessionHashesForRequest
// hashes the whole string: white space at the end is held back and dropped
// (strings.TrimSpace; the seed starts with its prefix, never with space),
// and with a spool the expanded seed is hashed when it holds a marker and
// expands cleanly, else the seed as written. A marker never spans two
// writes (each is a label or a whole value) and holds no white space.
type seedHashWriter struct {
	sp            *bodyspool.Spool
	rawX, expX    *xxhash.Digest
	rawS, expS    hash.Hash
	raw, exp      io.Writer
	marker        bool
	expErr        error
	pendingSpaces []string
}

func newSeedHashWriter(sp *bodyspool.Spool) *seedHashWriter {
	h := &seedHashWriter{sp: sp, rawX: xxhash.New(), rawS: sha256.New()}
	h.raw = io.MultiWriter(h.rawX, h.rawS)
	if sp != nil {
		h.expX, h.expS = xxhash.New(), sha256.New()
		h.exp = io.MultiWriter(h.expX, h.expS)
	}
	return h
}

func (h *seedHashWriter) WriteString(s string) (int, error) {
	end := len(strings.TrimRightFunc(s, unicode.IsSpace))
	if end == 0 {
		h.pendingSpaces = append(h.pendingSpaces, s)
		return len(s), nil
	}
	for _, p := range h.pendingSpaces {
		h.emit(p)
	}
	h.pendingSpaces = h.pendingSpaces[:0]
	h.emit(s[:end])
	if end < len(s) {
		h.pendingSpaces = append(h.pendingSpaces, s[end:])
	}
	return len(s), nil
}

func (h *seedHashWriter) emit(s string) {
	// The hashes only read the bytes: a view, no copy.
	_, _ = h.raw.Write(unsafe.Slice(unsafe.StringData(s), len(s)))
	if h.sp == nil {
		return
	}
	if h.sp.Contains(s) {
		h.marker = true
	}
	if h.expErr == nil {
		h.expErr = h.sp.WriteExpanded(h.exp, s)
	}
}

func (h *seedHashWriter) sums() (currentHash, legacyHash string) {
	if h.marker && h.expErr == nil {
		return fmt.Sprintf("%016x", h.expX.Sum64()), hex.EncodeToString(h.expS.Sum(nil))
	}
	return fmt.Sprintf("%016x", h.rawX.Sum64()), hex.EncodeToString(h.rawS.Sum(nil))
}

// newOpenAIUpstreamBodyRequest builds the upstream POST. With a spool the
// body is the splice stream (exact Content-Length, re-readable GetBody); an
// unresolved marker fails closed with an explicit 500 and nothing is sent.
func newOpenAIUpstreamBodyRequest(ctx context.Context, c *gin.Context, method, url string, body []byte) (*http.Request, error) {
	if sp, _ := requestBodySpoolFromContext(ctx); sp == nil {
		return http.NewRequestWithContext(ctx, method, url, bytes.NewReader(body))
	}
	req, err := http.NewRequestWithContext(ctx, method, url, nil)
	if err != nil {
		return nil, err
	}
	if err := setOpenAIUpstreamBody(req, body, false); err != nil {
		if errors.Is(err, bodyspool.ErrMarkerUnresolved) && c != nil && !c.Writer.Written() {
			MarkResponseCommitted(c)
			c.JSON(http.StatusInternalServerError, gin.H{"error": gin.H{
				"type": "server_error", "code": "marker_unresolved", "hop": "sub2api",
				"message": "sub2api: a request rewrite left an unresolved spool marker; nothing was sent upstream",
			}})
		}
		return nil, err
	}
	return req, nil
}

// setOpenAIUpstreamBody sets req's body from the (skeleton) bytes. With
// copyBody, GetBody replays a private copy (the legacy GPT-5.5 Lite rewrite);
// otherwise it replays body itself, as http.NewRequest does.
func setOpenAIUpstreamBody(req *http.Request, body []byte, copyBody bool) error {
	saved := body
	if copyBody {
		saved = append([]byte(nil), body...)
	}
	sp, _ := requestBodySpoolFromContext(req.Context())
	if sp == nil {
		req.Body = io.NopCloser(bytes.NewReader(body))
		req.ContentLength = int64(len(body))
		req.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(saved)), nil }
		return nil
	}
	r, n, err := sp.Reader(body)
	if err != nil {
		return err
	}
	req.Body = spliceBody{r}
	req.ContentLength = n
	req.GetBody = func() (io.ReadCloser, error) {
		r, _, err := sp.Reader(saved)
		return spliceBody{r}, err
	}
	return nil
}

// spliceBody marks a request body built by the splicer.
type spliceBody struct{ io.Reader }

func (spliceBody) Close() error { return nil }

// errSpoolEgressUnspliced: a request of a spooled body was about to be sent
// with a body that did not go through the splicer, which would leak markers.
// Only bodies with blobs carry a spool; only the OpenAI Responses (forward,
// passthrough) and Chat Completions builders splice.
var errSpoolEgressUnspliced = errors.New("sub2api: this upstream protocol cannot carry a body with spooled media strings (64 KiB or more); only OpenAI Responses and Chat Completions upstreams can; nothing was sent")

// CodeSpooledBodyUnsupported names the request-scoped refusal of a spooled
// body on a request builder that does not splice.
const CodeSpooledBodyUnsupported = "spooled_body_unsupported"

// writeSpoolEgressRefusal answers 422 spooled_body_unsupported (if nothing
// was written yet) and returns a plain error: the request stops on this
// attempt, with no failover and no account or pool record.
func writeSpoolEgressRefusal(c *gin.Context, err error) error {
	if c != nil && !c.Writer.Written() {
		MarkResponseCommitted(c)
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": gin.H{
			"type": "invalid_request_error", "code": CodeSpooledBodyUnsupported, "hop": "sub2api", "message": err.Error(),
		}})
	}
	return err
}

// checkSpoolEgress fails closed when a spooled request would send a body
// that is not the splice stream.
func checkSpoolEgress(req *http.Request) error {
	if sp, _ := requestBodySpoolFromContext(req.Context()); sp == nil || req.Body == nil || req.Body == http.NoBody {
		return nil
	}
	if _, ok := req.Body.(spliceBody); !ok {
		return errSpoolEgressUnspliced
	}
	return nil
}

// applyCapacityTicketHeader forwards the leased ticket only to a configured
// internal upstream (apisix-models) on a direct connection; it is stripped
// on every other path, including any proxy (the Squid relay).
func (s *OpenAIGatewayService) applyCapacityTicketHeader(account *Account, req *http.Request) {
	req.Header.Del(bodyspool.TicketHeader)
	_, ticket := requestBodySpoolFromContext(req.Context())
	if ticket == "" || s == nil || s.cfg == nil || account == nil || (account.ProxyID != nil && account.Proxy != nil) {
		return
	}
	for _, host := range s.cfg.Gateway.BodySpool.TicketForwardHosts {
		if strings.EqualFold(strings.TrimSpace(host), req.URL.Host) {
			req.Header.Set(bodyspool.TicketHeader, ticket)
			return
		}
	}
}
