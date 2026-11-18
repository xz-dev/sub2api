//go:build unit

package service

import (
	"bytes"
	"os"
	"os/exec"
	"testing"
)

const allocIsolatedEnv = "SUB2API_ALLOC_TEST_ISOLATED"

// runAllocTestIsolated runs t again in a fresh test process and reports
// true when the caller should return. Allocation counters (AllocsPerRun,
// MemStats) are process-wide: background goroutines that earlier tests of
// this package leave running (workers, tickers) allocate while a bound is
// measured, which made these bounds flaky on many-core CI runners. In the
// child only this test runs, so the counters see the measured function.
func runAllocTestIsolated(t *testing.T) bool {
	t.Helper()
	if os.Getenv(allocIsolatedEnv) == "1" {
		return false
	}
	cmd := exec.Command(os.Args[0], "-test.run=^"+t.Name()+"$", "-test.count=1", "-test.v")
	cmd.Env = append(os.Environ(), allocIsolatedEnv+"=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("isolated %s failed: %v\n%s", t.Name(), err, out)
	}
	if !bytes.Contains(out, []byte("--- PASS: "+t.Name()+" ")) {
		t.Fatalf("isolated %s did not run:\n%s", t.Name(), out)
	}
	return true
}
