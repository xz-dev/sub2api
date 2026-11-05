//go:build linux

package main

import "golang.org/x/sys/unix"

// Transparent huge pages are disabled for this process (PR_SET_THP_DISABLE).
//
// With THP set to "always", the Go heap is backed by 2 MiB pages. The
// runtime returns freed heap in smaller chunks (madvise MADV_DONTNEED), and a
// partly returned huge page stays charged to the cgroup until the kernel
// splits it under pressure. Freed request memory therefore stayed in
// memory.current after the request, after GC and even after
// debug.FreeOSMemory: on a 320m hop the working set was 72 MiB after
// FreeOSMemory with 24 MiB of live heap, and the large-request memory charge
// (bodyspool.Meter) counts that working set as in use. With THP off the same
// point reads 30 MiB.
//
// The flag only changes how anonymous memory is faulted in. If the kernel
// rejects it (very old kernels), the process runs as before.
func init() {
	_ = unix.Prctl(unix.PR_SET_THP_DISABLE, 1, 0, 0, 0)
}
