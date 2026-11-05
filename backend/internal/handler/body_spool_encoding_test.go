//go:build unit

package handler

import (
	"bytes"
	"compress/gzip"
	"compress/zlib"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/klauspost/compress/zstd"
	"github.com/stretchr/testify/require"
)

func encodeBody(t *testing.T, enc string, plain []byte) []byte {
	t.Helper()
	var z bytes.Buffer
	switch enc {
	case "gzip":
		w := gzip.NewWriter(&z)
		_, _ = w.Write(plain)
		require.NoError(t, w.Close())
	case "deflate":
		w := zlib.NewWriter(&z)
		_, _ = w.Write(plain)
		require.NoError(t, w.Close())
	case "zstd":
		w, err := zstd.NewWriter(&z)
		require.NoError(t, err)
		_, _ = w.Write(plain)
		require.NoError(t, w.Close())
	}
	return z.Bytes()
}

// TestBodySpoolEncodedBodyChargedDecoded is the review probe (a4a7d843
// blocker 1): a 96 KiB gzip body that decodes to 48 MiB. Before the fix it was
// charged 1.66 MiB on its compressed bytes and expanded to 48 MiB in memory,
// past S and L. Now L applies to the decoded bytes: it gets the named 413
// with hop and limit_bytes, having held at most L decoded bytes, all charged.
func TestBodySpoolEncodedBodyChargedDecoded(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := newLargeBodyTestHandler(t, &largeBodyDrainUpstream{}, "oauth")
	enableBodySpoolForTest(t, h.cfg, true, 320<<20, nil)
	rt := bodySpoolRuntimeFor(h.cfg)
	require.NoError(t, rt.err)
	limit := fallbackLimit(rt.cfg, 320<<20)
	plain := []byte(`{"model":"gpt-5.5","input":"` + strings.Repeat("a", 48<<20) + `"}`)
	for _, enc := range []string{"gzip", "zstd", "deflate"} {
		z := encodeBody(t, enc, plain)
		require.Less(t, len(z), 1<<20, enc)
		for _, path := range []string{"/v1/responses", "/v1/chat/completions"} {
			c, rec := newLargeBodyTestContext(path, z, "")
			c.Request.Header.Set("Content-Encoding", enc)
			require.False(t, ingestRequestBody(c, h.cfg), "%s %s: an encoded body above L is refused", enc, path)
			v, _ := c.Get(bodySpoolStateKey)
			peak := v.(*bodySpoolState).chargedAtRelease // the charge when the refusal released it
			require.Equal(t, http.StatusRequestEntityTooLarge, rec.Code, rec.Body.String())
			var resp struct {
				Error struct {
					Code       string `json:"code"`
					Hop        string `json:"hop"`
					LimitBytes int64  `json:"limit_bytes"`
				} `json:"error"`
			}
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
			require.Equal(t, "request_too_large", resp.Error.Code)
			require.Equal(t, "sub2api", resp.Error.Hop)
			require.Equal(t, limit, resp.Error.LimitBytes)
			// Charged k_path × the decoded bytes read (L plus one read), not
			// the compressed size.
			require.GreaterOrEqual(t, peak, chargeBytes(rt.cfg.KPath, limit), "%s %s: charged on decoded bytes", enc, path)
			require.Zero(t, rt.meter.Outstanding())
		}
	}
}

// TestBodySpoolEncodedBodyBelowLimitIsDecoded: an encoded body at or below L
// is not refused (Codex may compress its requests). The handlers get the
// decoded bytes with no Content-Encoding, and the charge covers k_path × the
// decoded size plus the decoder's state.
func TestBodySpoolEncodedBodyBelowLimitIsDecoded(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := newLargeBodyTestHandler(t, &largeBodyDrainUpstream{}, "oauth")
	enableBodySpoolForTest(t, h.cfg, true, 4<<30, nil)
	rt := bodySpoolRuntimeFor(h.cfg)
	require.NoError(t, rt.err)
	plain := largeResponsesTextBody(2 << 20)
	for _, enc := range []string{"gzip", "x-gzip", "zstd", "deflate"} {
		wire := enc
		if enc == "x-gzip" {
			enc = "gzip"
		}
		z := encodeBody(t, enc, plain)
		for _, path := range []string{"/v1/responses", "/v1/chat/completions"} {
			c, rec := newLargeBodyTestContext(path, z, "")
			c.Request.Header.Set("Content-Encoding", wire)
			require.True(t, ingestRequestBody(c, h.cfg), rec.Body.String())
			require.Empty(t, c.Request.Header.Get("Content-Encoding"))
			require.EqualValues(t, len(plain), c.Request.ContentLength)
			body, err := readPrereadBody(c)
			require.NoError(t, err)
			require.Equal(t, plain, body, "%s %s", wire, path)
			v, _ := c.Get(bodySpoolStateKey)
			st := v.(*bodySpoolState)
			require.False(t, st.covered, "an encoded body is a fallback body")
			require.GreaterOrEqual(t, st.charge.Bytes(), chargeBytes(rt.cfg.KPath, int64(len(plain))), "%s %s", wire, path)
			releaseRequestBodySpool(c)
			require.Zero(t, rt.meter.Outstanding())
		}
	}
}

// TestBodySpoolEncodedBodyUndecodable: a body that does not decode gets a
// named 400 with nothing charged afterwards.
func TestBodySpoolEncodedBodyUndecodable(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := newLargeBodyTestHandler(t, &largeBodyDrainUpstream{}, "oauth")
	enableBodySpoolForTest(t, h.cfg, true, 4<<30, nil)
	rt := bodySpoolRuntimeFor(h.cfg)
	for _, enc := range []string{"gzip", "zstd", "deflate", "br"} {
		c, rec := newLargeBodyTestContext("/v1/chat/completions", []byte(`{"model":"x"}`), "")
		c.Request.Header.Set("Content-Encoding", enc)
		require.False(t, ingestRequestBody(c, h.cfg), enc)
		require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
		require.Contains(t, rec.Body.String(), "decode Content-Encoding")
		require.Zero(t, rt.meter.Outstanding())
	}
}
