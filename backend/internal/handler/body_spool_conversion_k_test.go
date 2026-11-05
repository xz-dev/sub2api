//go:build unit

package handler

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// TestBodySpoolConversionLiveK measures k_conv in-process: the peak live heap
// (/memory/classes/heap/objects:bytes, sampled every 1 ms, over a post-GC
// baseline) of a /v1/responses request that the account serves through the
// Responses->Chat converter, divided by the body bytes. Default GOGC, no
// memory limit. In a container, peak - idle memory.current is not a valid k
// measure once the charge equals the room: Go lets garbage grow into the
// free memory, so the reading tracks the room, not the request.
//
// SUB2API_KCONV_BODIES names the corpus files (comma-separated paths); the
// test skips without it.
func TestBodySpoolConversionLiveK(t *testing.T) {
	list := os.Getenv("SUB2API_KCONV_BODIES")
	if list == "" {
		t.Skip("set SUB2API_KCONV_BODIES to a comma-separated list of Responses bodies")
	}
	gin.SetMode(gin.TestMode)
	for _, path := range strings.Split(list, ",") {
		body, err := os.ReadFile(path)
		require.NoError(t, err)
		worst := 0.0
		for run := 1; run <= 3; run++ {
			upstream := &largeBodyDrainUpstream{}
			h := newLargeBodyTestHandler(t, upstream, "apikey_chat")
			enableBodySpoolForTest(t, h.cfg, true, 8<<30, nil)
			c, rec := newLargeBodyTestContext("/v1/responses", body, "")
			delta := measurePeakHeap(func() { h.Responses(c); releaseRequestBodySpool(c) })
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			require.EqualValues(t, 1, upstream.calls.Load())
			k := float64(delta) / float64(len(body))
			worst = max(worst, k)
			t.Logf("KCONV %s run %d bytes %d peak_live %.1f MiB k %.2f", filepath.Base(path), run, len(body), float64(delta)/(1<<20), k)
		}
		t.Logf("KCONV %s worst %.2f", filepath.Base(path), worst)
	}
}
