package bodyspool

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"testing"
	"unsafe"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

// D3 rule tests.

func TestOpenDirRefusesTmpfs(t *testing.T) {
	var st unix.Statfs_t
	if err := unix.Statfs("/dev/shm", &st); err != nil || st.Type != tmpfsMagic {
		t.Skip("no tmpfs at /dev/shm")
	}
	p, err := os.MkdirTemp("/dev/shm", "bodyspool-")
	require.NoError(t, err)
	defer os.RemoveAll(p)
	require.NoError(t, os.Chmod(p, 0o700))
	_, err = OpenDir(p, 1)
	require.ErrorContains(t, err, "memory-backed")
}

func TestOpenDirRefusesBadModeAndNamedFiles(t *testing.T) {
	p := filepath.Join(t.TempDir(), "spool")
	require.NoError(t, os.Mkdir(p, 0o755))
	_, err := OpenDir(p, 1)
	require.ErrorContains(t, err, "mode 0700")

	require.NoError(t, os.Chmod(p, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(p, "left-over"), []byte("x"), 0o600))
	_, err = OpenDir(p, 1)
	require.ErrorContains(t, err, `regular file "left-over"`)
}

// TestSpoolFileIsAnonymousPrivateCloexec: O_TMPFILE (no name in the dir),
// mode 0600, close-on-exec, and freed on Close.
func TestSpoolFileIsAnonymousPrivateCloexec(t *testing.T) {
	d := testDir(t)
	f, err := d.Create()
	require.NoError(t, err)
	_, err = f.Write([]byte("secret prompt"))
	require.NoError(t, err)
	entries, err := os.ReadDir(d.path)
	require.NoError(t, err)
	require.Empty(t, entries, "spool files never have a name")
	var st unix.Stat_t
	require.NoError(t, unix.Fstat(int(f.f.Fd()), &st))
	require.EqualValues(t, 0o600, st.Mode&0o777)
	require.EqualValues(t, 0, st.Nlink)
	flags, err := unix.FcntlInt(f.f.Fd(), unix.F_GETFD, 0)
	require.NoError(t, err)
	require.NotZero(t, flags&unix.FD_CLOEXEC)
	require.NoError(t, f.Close())
	require.NoError(t, f.Close(), "Close is idempotent")
}

func TestDiskFloorRefusesWrite(t *testing.T) {
	p := filepath.Join(t.TempDir(), "spool")
	require.NoError(t, os.Mkdir(p, 0o700))
	d, err := OpenDir(p, 1<<62) // reserve larger than any disk
	require.NoError(t, err)
	f, err := d.Create()
	require.NoError(t, err)
	defer f.Close()
	_, err = f.Write([]byte("x"))
	require.ErrorIs(t, err, ErrInsufficientStorage)
	require.Zero(t, f.Size(), "a refused write never falls back to memory or partial bytes")
}

// residentPages counts the file's pages in the page cache (mincore).
func residentPages(t *testing.T, f *File, size int64) int {
	t.Helper()
	m, err := unix.Mmap(int(f.f.Fd()), 0, int(size), unix.PROT_READ, unix.MAP_SHARED)
	require.NoError(t, err)
	defer unix.Munmap(m)
	vec := make([]byte, (size+int64(os.Getpagesize())-1)/int64(os.Getpagesize()))
	_, _, errno := unix.Syscall(unix.SYS_MINCORE, uintptr(unsafe.Pointer(&m[0])), uintptr(len(m)), uintptr(unsafe.Pointer(&vec[0])))
	require.Zero(t, errno)
	n := 0
	for _, v := range vec {
		n += int(v & 1)
	}
	return n
}

// TestPageCacheBounded: writing and reading 64 MiB through the spool keeps
// at most ~2 x 8 MiB of its pages cached (fdatasync + DONTNEED per 8 MiB).
func TestPageCacheBounded(t *testing.T) {
	d := testDir(t)
	var fs unix.Statfs_t
	require.NoError(t, unix.Statfs(d.path, &fs))
	if fs.Type == 0x2fc12fc1 { // ZFS caches in its ARC, not the page cache
		t.Skip("spool dir is on ZFS; run with TMPDIR on ext2/ext4 to check the page-cache bound")
	}
	f, err := d.Create()
	require.NoError(t, err)
	defer f.Close()
	chunk := bytes.Repeat([]byte("z"), 256<<10)
	const total = 64 << 20
	for i := 0; i < total/len(chunk); i++ {
		_, err := f.Write(chunk)
		require.NoError(t, err)
	}
	page := int64(os.Getpagesize())
	bound := int((16 << 20) / page) // D3: about 16 MiB of file pages per spool
	require.LessOrEqual(t, residentPages(t, f, total), bound, "write pass")

	n, err := io.Copy(io.Discard, f.Section(0, total))
	require.NoError(t, err)
	require.EqualValues(t, total, n)
	require.LessOrEqual(t, residentPages(t, f, total), bound, "read pass")
}

func TestSectionReadsExactRange(t *testing.T) {
	d := testDir(t)
	f, err := d.Create()
	require.NoError(t, err)
	defer f.Close()
	_, err = f.Write([]byte("0123456789"))
	require.NoError(t, err)
	b, err := io.ReadAll(f.Section(3, 4))
	require.NoError(t, err)
	require.Equal(t, "3456", string(b))
	_, err = io.ReadAll(f.Section(8, 5))
	require.ErrorIs(t, err, io.ErrUnexpectedEOF)
}
