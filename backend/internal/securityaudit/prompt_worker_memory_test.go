package securityaudit

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/bodyspool"
	"github.com/stretchr/testify/require"
)

// fakeMemoryBudget is a bodyspool meter on a fake cgroup of max bytes with
// nothing in use.
func fakeMemoryBudget(t *testing.T, max int64) *bodyspool.Meter {
	t.Helper()
	dir := t.TempDir()
	for name, value := range map[string]string{
		"memory.max": strconv.FormatInt(max, 10) + "\n", "memory.current": "0\n",
		"memory.stat": "anon 0\nfile 0\nactive_file 0\ninactive_file 0\n",
	} {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(value), 0o600))
	}
	meter, err := bodyspool.OpenMeter(dir)
	require.NoError(t, err)
	meter.Reclaim, meter.ReleaseReclaim = nil, nil
	t.Cleanup(meter.Close)
	return meter
}

// TestWorkerChargeVisibleWhenQueueOutlivesRequest (follow-up 6, item 2): the
// job is audited after the request that enqueued it has ended and released
// its charge. While the worker reads and scans the payload, its charge
// (workerChargeBytes) is outstanding on the same budget; it is released
// when the audit is done.
func TestWorkerChargeVisibleWhenQueueOutlivesRequest(t *testing.T) {
	meter := fakeMemoryBudget(t, 512<<20)
	request, err := meter.NewCharge()
	require.NoError(t, err)
	require.NoError(t, request.Add(64<<20))
	request.Release() // the request ends before the queue gets to its job
	require.Zero(t, meter.Outstanding())

	text := "abcdefgh"
	scanning, proceed := make(chan int64, 1), make(chan struct{})
	scanner := PromptScannerFunc(func(_ context.Context, ep ActiveEndpoint, _ string, _ []string) (*NormalizedResult, error) {
		select {
		case scanning <- meter.Outstanding():
			<-proceed
		default:
		}
		return &NormalizedResult{Decision: EventPass, RiskLevel: RiskLow, Action: ActionAllow, GuardEndpointID: ep.ID}, nil
	})
	repo := &fakeJobRepository{}
	runner := NewRunner(&fakeConfigStore{cfg: asyncConfig(), active: true}, repo, &fakePayloadStore{values: map[int64]string{51: text}}, scanner, nil)
	runner.clock = fixedClock{now: time.Unix(100, 0).UTC()}
	runner.memory = meter
	done := make(chan error, 1)
	go func() { done <- runner.processJob(context.Background(), 0, asyncConfig(), workerJob(1, 3)) }()
	require.Equal(t, workerChargeBytes(int64(len(text)), asyncConfig().Endpoints[0].InputLimit), <-scanning,
		"the worker's charge is outstanding while it scans")
	close(proceed)
	require.NoError(t, <-done)
	require.NotNil(t, repo.completedResult)
	require.Zero(t, meter.Outstanding(), "released when the audit is done")
}

// TestWorkerWaitsForMemoryAndReleasesOnEveryExit: a worker whose charge
// does not fit waits (keeping its lease) until a request releases memory,
// then audits; nothing is dropped. A shutdown while waiting leaves the job
// to the reclaimer, not failed. A panic while scanning releases the charge.
func TestWorkerWaitsForMemoryAndReleasesOnEveryExit(t *testing.T) {
	meter := fakeMemoryBudget(t, 128<<20)
	hog, err := meter.NewCharge()
	require.NoError(t, err)
	require.NoError(t, hog.Add(95<<20)) // 1 MiB free: below the worker's fixed bound
	pass := PromptScannerFunc(func(_ context.Context, ep ActiveEndpoint, _ string, _ []string) (*NormalizedResult, error) {
		return &NormalizedResult{Decision: EventPass, RiskLevel: RiskLow, Action: ActionAllow, GuardEndpointID: ep.ID}, nil
	})
	newRunner := func(scanner PromptScanner) (*Runner, *fakeJobRepository) {
		repo := &fakeJobRepository{}
		runner := NewRunner(&fakeConfigStore{cfg: asyncConfig(), active: true}, repo, &fakePayloadStore{values: map[int64]string{51: "abcdef"}}, scanner, nil)
		runner.clock = fixedClock{now: time.Unix(100, 0).UTC()}
		runner.memory = meter
		return runner, repo
	}

	runner, repo := newRunner(pass)
	done := make(chan error, 1)
	go func() { done <- runner.processJob(context.Background(), 0, asyncConfig(), workerJob(1, 3)) }()
	require.Eventually(t, func() bool { repo.mu.Lock(); defer repo.mu.Unlock(); return repo.refreshes > 0 }, 5*time.Second, time.Millisecond,
		"the waiting worker keeps its lease")
	select {
	case err := <-done:
		t.Fatalf("audited without memory: %v", err)
	default:
	}
	hog.Release() // a request ends
	require.NoError(t, <-done)
	require.NotNil(t, repo.completedResult)
	require.Zero(t, meter.Outstanding())

	require.NoError(t, hog.Add(95<<20))
	runner, repo = newRunner(pass)
	ctx, cancel := context.WithCancel(context.Background())
	go func() { done <- runner.processJob(ctx, 0, asyncConfig(), workerJob(1, 3)) }()
	require.Eventually(t, func() bool { repo.mu.Lock(); defer repo.mu.Unlock(); return repo.refreshes > 0 }, 5*time.Second, time.Millisecond)
	cancel()
	require.ErrorIs(t, <-done, context.Canceled)
	require.Zero(t, repo.failed, "a shutdown does not fail the audit")
	require.Nil(t, repo.completedResult)
	hog.Release()

	runner, repo = newRunner(PromptScannerFunc(func(context.Context, ActiveEndpoint, string, []string) (*NormalizedResult, error) {
		require.NotZero(t, meter.Outstanding())
		panic("scanner bug")
	}))
	runner.processSafely(context.Background(), 0, asyncConfig(), workerJob(1, 3))
	require.Equal(t, "worker_panic", repo.failedCode)
	require.Zero(t, meter.Outstanding(), "a panic releases the charge")
}
