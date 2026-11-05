//go:build !race

package securityaudit

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"runtime"
	"runtime/debug"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestWorkerChargeCoversAudit: what the worker holds while it audits a
// payload with the real guard client stays within workerChargeBytes. The
// live set (GC'd, at each chunk) grows by the chunk list and the kept
// results; the chunk being scanned allocates its request (json escapes of
// "<" take 6 bytes), the HTTP exchange and a response of almost
// maxGuardResponseBytes (counted with GC off, an upper bound). The payload
// itself (go-redis reads n + 2 bytes) is in the charge and not measured
// here: the fake store hands out its own string.
func TestWorkerChargeCoversAudit(t *testing.T) {
	// Log records go nowhere, as to a stream in production; an earlier test
	// may have left the log package writing into a buffer.
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(io.Discard, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	pad := strings.Repeat("x", int(maxGuardResponseBytes)-200)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"pad":"`+pad+`","choices":[{"message":{"content":"Safety: Safe\nCategories: None"}}]}`)
	}))
	defer server.Close()
	real := NewOpenAICompatibleScanner()
	for _, tc := range []struct {
		text  string
		limit int
	}{
		{strings.Repeat("<", 256<<10), MinInputLimit},
		{strings.Repeat("<", 2<<20), MaxInputLimit},
		{strings.Repeat("😀", 512<<10), MaxInputLimit},
		{strings.Repeat("a", 1<<20), DefaultInputLimit},
	} {
		// The heap is the process's: other tests' goroutines can only add to
		// a measurement, so the least of three runs is kept.
		best := uint64(1<<63 - 1)
		for try := 0; try < 3; try++ {
			live, work := measureWorkerAudit(t, server.URL, real, tc.text, tc.limit)
			best = min(best, live+work)
		}
		n := int64(len(tc.text))
		charge := workerChargeBytes(n, tc.limit) - (n + 2)
		t.Logf("payload %8d B, limit %6d: live + chunk work %8d B; charge without the payload %8d B", n, tc.limit, best, charge)
		require.LessOrEqual(t, int64(best), charge, "limit %d", tc.limit)
	}
}

// measureWorkerAudit runs one audit of text and returns the most the live
// heap (GC'd) grew by at a chunk, and the most one guard call allocated
// (GC off).
func measureWorkerAudit(t *testing.T, url string, real PromptScanner, text string, limit int) (live, work uint64) {
	var base uint64
	scanner := PromptScannerFunc(func(ctx context.Context, ep ActiveEndpoint, chunk string, scanners []string) (*NormalizedResult, error) {
		var ms runtime.MemStats
		runtime.GC()
		runtime.ReadMemStats(&ms)
		live = max(live, ms.HeapAlloc-min(base, ms.HeapAlloc))
		old := debug.SetGCPercent(-1)
		defer debug.SetGCPercent(old)
		before := ms.TotalAlloc
		result, err := real.Scan(ctx, ep, chunk, scanners)
		runtime.ReadMemStats(&ms)
		work = max(work, ms.TotalAlloc-before)
		return result, err
	})
	cfg := asyncConfig()
	cfg.Endpoints = []ActiveEndpoint{{ID: "guard", Enabled: true, BaseURL: url, Model: DefaultGuardModel, TimeoutMS: 30000, InputLimit: limit}}
	payload := &fakePayloadStore{values: map[int64]string{51: text}}
	runner := NewRunner(&fakeConfigStore{cfg: cfg, active: true}, &fakeJobRepository{}, payload, scanner, nil)
	runner.clock = fixedClock{now: time.Unix(100, 0).UTC()}
	var ms runtime.MemStats
	runtime.GC()
	runtime.GC() // and the pools' victim caches of the previous run
	runtime.ReadMemStats(&ms)
	base = ms.HeapAlloc
	require.NoError(t, runner.processJob(context.Background(), 0, cfg, workerJob(1, 3)))
	return live, work
}
