//go:build unit

package handler

import (
	"bytes"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/bodyspool"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// TestBodySpoolMemoryRefusalNamesRequestNeed (exit gate 7.5 finding 2): the
// limit_bytes of a memory 503 is the bytes the request needs, never the
// refused step alone (it was 24,576 B, one 6 x 4 KiB step, for a chunked
// 16 MiB body that needed about 96 MiB).
//   - Content-Length: the whole charge is taken before reading, so
//     limit_bytes is that full charge.
//   - Chunked: the total is unknown, so limit_bytes is a lower bound: the
//     bytes charged so far plus the refused step. It is above the headroom
//     the refused step did not fit, and at most one step beyond it.
func TestBodySpoolMemoryRefusalNamesRequestNeed(t *testing.T) {
	gin.SetMode(gin.TestMode)
	textBody := func(path string, n int) []byte {
		head, tail := `{"model":"gpt-5.5","input":"`, `"}`
		if path == "/v1/chat/completions" {
			head, tail = `{"model":"gpt-5.5","messages":[{"role":"user","content":"`, `"}]}`
		}
		return []byte(head + string(bytes.Repeat([]byte("a"), n-len(head)-len(tail))) + tail)
	}
	const mib = 1 << 20
	cases := []struct {
		name    string
		path    string
		size    int
		chunked bool
		current int64 // the cgroup working set; memory.max is 320 MiB
		want    func(t *testing.T, need, free int64, k float64)
	}{
		{name: "covered/content-length", path: "/v1/responses", size: 16 * mib, current: 240 * mib,
			want: func(t *testing.T, need, _ int64, k float64) {
				full := chargeBytes(k, roundUpGranule(16*mib)) + bodyspool.ReadBufferBytes
				require.Equal(t, full, need, "the whole up-front charge: k_skel x N + the read buffer")
			}},
		// rr6: the k_path fallback route is compact (Chat Completions is k_conv).
		{name: "fallback/content-length", path: "/v1/responses/compact", size: 8 * mib, current: 200 * mib,
			want: func(t *testing.T, need, _ int64, k float64) {
				require.Equal(t, chargeBytes(k, roundUpGranule(8*mib+1)), need, "the whole up-front charge: k_path x (N+1)")
			}},
		{name: "covered/chunked", path: "/v1/responses", size: 16 * mib, chunked: true, current: 240 * mib,
			want: func(t *testing.T, need, free int64, _ float64) {
				require.Greater(t, need, free, "charged so far + the refused step")
				require.LessOrEqual(t, need, free+mib)
			}},
		{name: "fallback/chunked", path: "/v1/responses/compact", size: 8 * mib, chunked: true, current: 200 * mib,
			want: func(t *testing.T, need, free int64, _ float64) {
				require.Greater(t, need, free, "charged so far + the refused step")
				require.LessOrEqual(t, need, free+mib)
			}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newLargeBodyTestHandler(t, &largeBodyDrainUpstream{}, "oauth")
			env := enableBodySpoolForTest(t, h.cfg, true, 320*mib, nil)
			rt := bodySpoolRuntimeFor(h.cfg)
			require.NoError(t, rt.err)
			require.NoError(t, os.WriteFile(filepath.Join(env.cgroup, "memory.current"), []byte(strconv.FormatInt(tc.current, 10)+"\n"), 0o600))
			free := 320*mib - tc.current - bodyspool.ChargeMargin
			k := rt.cfg.KPath
			if tc.path == "/v1/responses" {
				k = rt.cfg.KSkel
			}

			c, rec := newLargeBodyTestContext(tc.path, textBody(tc.path, tc.size), "")
			if tc.chunked {
				c.Request.Body = io.NopCloser(bytes.NewReader(textBody(tc.path, tc.size)))
				c.Request.ContentLength = -1
			}
			require.False(t, ingestRequestBody(c, h.cfg))
			requireCapacityError(t, rec, http.StatusServiceUnavailable, bodyspool.CodeInsufficientMemory)
			need := gjson.Get(rec.Body.String(), "error.limit_bytes").Int()
			t.Logf("%s: limit_bytes %d, free %d, message %s", tc.name, need, free, gjson.Get(rec.Body.String(), "error.message").String())
			require.Greater(t, need, int64(64<<10), "never the refused step alone")
			tc.want(t, need, free, k)
			require.Zero(t, rt.meter.Outstanding(), "the refusal released the charge")
		})
	}
}
