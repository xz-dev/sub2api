//go:build unit

package handler

import (
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// TestMessagesTotalAllocIsLinear (rr10): /v1/messages on the Codex OAuth
// account allocates the same per body byte at 2, 8 and 16 MiB of tiny
// messages. The digest-session lookup was quadratic (k 12.9k / 50.8k /
// 101k, 1.7 TB at 16 MiB); this keeps it from coming back silently.
func TestMessagesTotalAllocIsLinear(t *testing.T) {
	gin.SetMode(gin.TestMode)
	var cb coverCombo
	for _, c := range coverOpenAICombos() {
		if c.route == "v1/messages" && c.account == "oauth" {
			cb = c
		}
	}
	require.NotEmpty(t, cb.path)
	h, cfg, up := cb.setup(t)
	srv := coverServer{h: h, cfg: cfg, up: up}
	p := coverProtos[cb.proto]
	runCoverCase(t, srv, cb.path, coverBody(p, coverBases[0], 64<<10, cb.model, "", ""), false, nil)
	var k []float64
	for _, size := range []int{2 << 20, 8 << 20, 16 << 20} {
		r := runCoverCaseBody(t, srv, cb.path, coverBody(p, coverBases[0], size, cb.model, "", ""), false, nil, false)
		require.Equal(t, 200, r.status, r.reply)
		k = append(k, float64(r.TotalAlloc)/float64(r.bytes))
	}
	t.Logf("total-alloc k at 2/8/16 MiB: %.2f / %.2f / %.2f", k[0], k[1], k[2])
	require.Less(t, k[1], k[0]*1.25, "8 MiB allocates more per byte than 2 MiB")
	require.Less(t, k[2], k[0]*1.25, "16 MiB allocates more per byte than 2 MiB")
}
