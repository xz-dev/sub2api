//go:build linux

package main

import (
	"testing"

	"golang.org/x/sys/unix"
)

func TestTransparentHugePagesDisabledForProcess(t *testing.T) {
	v, err := unix.PrctlRetInt(unix.PR_GET_THP_DISABLE, 0, 0, 0, 0)
	if err != nil {
		t.Skipf("PR_GET_THP_DISABLE unsupported: %v", err)
	}
	if v != 1 {
		t.Fatalf("PR_GET_THP_DISABLE = %d, want 1", v)
	}
}
