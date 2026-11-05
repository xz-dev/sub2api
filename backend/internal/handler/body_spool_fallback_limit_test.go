//go:build unit

package handler

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/bodyspool"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// TestBodySpoolFallbackBodyAtLimitFits (review a4a7d843 minor): on an idle
// 320m hop (working set at the idle baseline) a fallback body of exactly L
// is admitted with its worst charge (k_path, the guard decode, and for an
// encoded body the largest decoder), and L+1 bytes get the 413 with
// limit_bytes = L. With L fixed at 16 MiB, bodies from about 12.6 MiB got
// 503 insufficient_memory on such a hop instead.
func TestBodySpoolFallbackBodyAtLimitFits(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := newLargeBodyTestHandler(t, &largeBodyDrainUpstream{}, "oauth")
	env := enableBodySpoolForTest(t, h.cfg, true, 320<<20, nil)
	rt := bodySpoolRuntimeFor(h.cfg)
	require.NoError(t, rt.err)
	idle := rt.cfg.IdleBaseline
	require.NoError(t, os.WriteFile(filepath.Join(env.cgroup, "memory.current"), []byte(strconv.FormatInt(idle, 10)+"\n"), 0o600))
	limit := fallbackLimit(rt.cfg, 320<<20)
	t.Logf("L at 320m = %d bytes (%.2f MiB)", limit, float64(limit)/(1<<20))
	require.Greater(t, limit, int64(10<<20))
	require.Less(t, limit, rt.cfg.FallbackLimit)

	body := func(n int64) []byte {
		head, tail := `{"model":"gpt-5.5","messages":[{"role":"user","content":"`, `"}]}`
		return []byte(head + string(bytes.Repeat([]byte("a"), int(n)-len(head)-len(tail))) + tail)
	}
	for _, enc := range []string{"", "zstd"} {
		wire := body(limit)
		if enc != "" {
			wire = encodeBody(t, enc, wire)
		}
		c, rec := newLargeBodyTestContext("/v1/chat/completions", wire, "")
		if enc != "" {
			c.Request.Header.Set("Content-Encoding", enc)
		}
		require.True(t, ingestRequestBody(c, h.cfg), "%q: a body of L fits: %s", enc, rec.Body.String())
		got, err := readPrereadBody(c)
		require.NoError(t, err)
		require.Len(t, got, int(limit))
		require.Nil(t, chargeGuardDecode(c, func() bool { return true }, got), "%q: the guard decode fits too", enc)
		releaseRequestBodySpool(c)
		require.Zero(t, rt.meter.Outstanding())

		wire = body(limit + 1)
		if enc != "" {
			wire = encodeBody(t, enc, wire)
		}
		c, rec = newLargeBodyTestContext("/v1/chat/completions", wire, "")
		if enc != "" {
			c.Request.Header.Set("Content-Encoding", enc)
		}
		require.False(t, ingestRequestBody(c, h.cfg), enc)
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
		require.Zero(t, rt.meter.Outstanding())
	}

	// Under load the body still gets the named 503 with the bytes needed.
	busy, err := rt.meter.NewCharge()
	require.NoError(t, err)
	require.NoError(t, busy.Add(100<<20))
	defer busy.Release()
	c, rec := newLargeBodyTestContext("/v1/chat/completions", body(limit), "")
	require.False(t, ingestRequestBody(c, h.cfg))
	require.Equal(t, http.StatusServiceUnavailable, rec.Code, rec.Body.String())
	var resp struct {
		Error struct {
			Code       string `json:"code"`
			LimitBytes int64  `json:"limit_bytes"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Equal(t, bodyspool.CodeInsufficientMemory, resp.Error.Code)
	require.Positive(t, resp.Error.LimitBytes)
}
