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
// the guard reads, the guard's work on the request goroutine (validation,
// extraction, snapshot, and in blocking mode the chunks) allocates at most
// 3.5 × the body plus 512 KiB with GC off (review a4a7d843 blocker 2: it was
// 22-28 ×). What remains is the kept text (1 ×, in the segment arena) and
// the one scan-text join (1 ×), plus about 0.33 MiB for the preview head and
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

// TestPromptSnapshotShapeCostBounded: on every shape the guard's work
// allocates at most guardDecodeK (3.5) × the body + 512 KiB with GC off.
// Before follow-up 6 (json into any) small items took 12.6 ×.
func TestPromptSnapshotShapeCostBounded(t *testing.T) {
	for name, body := range guardShapeBodies(2 << 20) {
		protocol := strings.SplitN(name, "/", 2)[0]
		req := Request{Protocol: protocol, Body: body}
		cost := func(f func()) float64 {
			old := debug.SetGCPercent(-1)
			defer debug.SetGCPercent(old)
			runtime.GC()
			var a, b runtime.MemStats
			runtime.ReadMemStats(&a)
			f()
			runtime.ReadMemStats(&b)
			return (float64(b.TotalAlloc-a.TotalAlloc) - 512<<10) / float64(len(body))
		}
		var snap PromptSnapshot
		var err error
		async := cost(func() { snap, err = ExtractPromptSnapshot(req) })
		if err != nil {
			require.ErrorIs(t, err, ErrNoPromptText, name)
		}
		blocking := cost(func() {
			s, _ := ExtractBlockingPromptSnapshot(req, true)
			_ = SplitRunes(s.ScanText, DefaultInputLimit)
		})
		t.Logf("%-32s body %8d B, scan %8d B, %7d msgs: async %.2f, blocking %.2f", name, len(body), len(snap.ScanText), snap.MessageCount, async, blocking)
		require.LessOrEqual(t, async, 3.5, name)
		require.LessOrEqual(t, blocking, 3.5, name)
	}
}
