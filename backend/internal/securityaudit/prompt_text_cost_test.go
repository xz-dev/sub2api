//go:build !race

package securityaudit

import (
	"runtime"
	"runtime/debug"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestPromptSnapshotTextCostBounded: on a body whose bulk is message text
// the guard reads, the guard's work on the request goroutine (decode,
// snapshot, and in blocking mode the chunks) allocates at most 3.5 × the body
// plus 512 KiB with GC off (review a4a7d843 blocker 2: it was 22-28 ×). The
// decode (json into any: unquote buffer and string, 2 ×) and the one
// scan-text join (1 ×) remain, plus about 0.33 MiB for the preview head and
// the stored prompt, whatever the body size. handler.guardDecodeK and
// handler.guardFixedBytes charge this.
func TestPromptSnapshotTextCostBounded(t *testing.T) {
	line := "plain prose of a pasted document, with a newline\n a \"quote\" and ünïcödé "
	text := strings.Repeat(line, (64<<10)/len(line))
	var parts [][2]string
	for len(parts) < 64 {
		parts = append(parts, [2]string{"user", text}, [2]string{"assistant", text[:2048]})
	}
	body := responsesTextBody(parts...)
	measure := func(f func()) float64 {
		old := debug.SetGCPercent(-1)
		defer debug.SetGCPercent(old)
		runtime.GC()
		var a, b runtime.MemStats
		runtime.ReadMemStats(&a)
		f()
		runtime.ReadMemStats(&b)
		return (float64(b.TotalAlloc-a.TotalAlloc) - 512<<10) / float64(len(body))
	}
	req := Request{Protocol: "openai_responses", Body: body}
	var keep PromptSnapshot
	var chunks []string
	async := measure(func() { keep, _ = ExtractPromptSnapshot(req) })
	blocking := measure(func() {
		s, _ := ExtractBlockingPromptSnapshot(req, false)
		chunks = SplitRunes(s.ScanText, DefaultInputLimit)
	})
	t.Logf("body %d B, scan text %d B, %d chunks: (allocated - 512 KiB) / body: async %.2f, blocking %.2f", len(body), len(keep.ScanText), len(chunks), async, blocking)
	require.Greater(t, len(keep.ScanText), len(body)*3/4)
	require.LessOrEqual(t, async, 3.5)
	require.LessOrEqual(t, blocking, 3.5)
}
