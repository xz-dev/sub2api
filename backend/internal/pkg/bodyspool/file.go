package bodyspool

import (
	"errors"
	"fmt"
	"io"
	"os"
	"sync"

	"golang.org/x/sys/unix"
)

// D3 spool storage rules.
const (
	// DefaultDiskReserve is the free-space floor every spool write keeps.
	DefaultDiskReserve int64 = 1 << 30
	// cacheDropEvery bounds the spool's page cache: every ~8 MiB written is
	// fdatasync'ed and dropped, and every ~8 MiB read is dropped.
	cacheDropEvery int64 = 8 << 20

	tmpfsMagic = 0x01021994
	ramfsMagic = 0x858458f6
)

// ErrInsufficientStorage: a spool write would leave less than the reserve.
var ErrInsufficientStorage = errors.New("bodyspool: spool write would cross the disk reserve")

// Dir is a validated spool directory.
type Dir struct {
	path    string
	reserve int64
}

// OpenDir validates path as a spool directory and refuses, with an explicit
// configuration error, when it is on tmpfs or ramfs, is not mode 0700,
// holds a regular file (spools are anonymous, so none may exist), or its
// filesystem rejects O_TMPFILE. There is no sweep and no named fallback.
func OpenDir(path string, reserve int64) (*Dir, error) {
	if path == "" {
		return nil, errors.New("bodyspool: spool dir is not configured")
	}
	if reserve <= 0 {
		reserve = DefaultDiskReserve
	}
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return nil, fmt.Errorf("bodyspool: statfs %s: %w", path, err)
	}
	if st.Type == tmpfsMagic || st.Type == ramfsMagic {
		return nil, fmt.Errorf("bodyspool: spool dir %s is on a memory-backed filesystem (tmpfs/ramfs); use a disk volume", path)
	}
	fi, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("bodyspool: stat %s: %w", path, err)
	}
	if !fi.IsDir() || fi.Mode().Perm() != 0o700 {
		return nil, fmt.Errorf("bodyspool: spool dir %s must be a directory with mode 0700 (is %v)", path, fi.Mode())
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		return nil, fmt.Errorf("bodyspool: read %s: %w", path, err)
	}
	for _, e := range entries {
		if e.Type().IsRegular() {
			return nil, fmt.Errorf("bodyspool: spool dir %s holds regular file %q; spools are anonymous, remove it", path, e.Name())
		}
	}
	d := &Dir{path: path, reserve: reserve}
	f, err := d.Create()
	if err != nil {
		return nil, fmt.Errorf("bodyspool: spool dir %s does not support O_TMPFILE: %w", path, err)
	}
	_ = f.Close()
	return d, nil
}

// File is one anonymous spool (O_TMPFILE|O_CLOEXEC, mode 0600). Its blocks
// are freed when Close drops the last fd; a crash frees it too.
type File struct {
	dir       *Dir
	f         *os.File
	size      int64 // bytes written
	synced    int64 // bytes fdatasync'ed and dropped
	closeOnce sync.Once
}

// Create opens a new anonymous spool file in d.
func (d *Dir) Create() (*File, error) {
	fd, err := unix.Open(d.path, unix.O_TMPFILE|unix.O_RDWR|unix.O_CLOEXEC, 0o600)
	if err != nil {
		return nil, err
	}
	return &File{dir: d, f: os.NewFile(uintptr(fd), "bodyspool")}, nil
}

// Write appends p after checking the disk floor, and bounds dirty page cache.
func (s *File) Write(p []byte) (int, error) {
	var st unix.Statfs_t
	if err := unix.Fstatfs(int(s.f.Fd()), &st); err != nil {
		return 0, err
	}
	if free := int64(st.Bavail) * int64(st.Frsize); free-int64(len(p)) < s.dir.reserve {
		return 0, ErrInsufficientStorage
	}
	n, err := s.f.WriteAt(p, s.size)
	s.size += int64(n)
	if err == nil && s.size-s.synced >= cacheDropEvery {
		err = s.flush()
	}
	return n, err
}

// flush fdatasyncs the written tail and drops it from the page cache.
func (s *File) flush() error {
	if s.size == s.synced {
		return nil
	}
	if err := unix.Fdatasync(int(s.f.Fd())); err != nil {
		return err
	}
	_ = unix.Fadvise(int(s.f.Fd()), s.synced, s.size-s.synced, unix.FADV_DONTNEED)
	s.synced = s.size
	return nil
}

// Size is the number of bytes written.
func (s *File) Size() int64 { return s.size }

// ReadAt reads written bytes (positional, so attempts can overlap).
func (s *File) ReadAt(p []byte, off int64) (int, error) { return s.f.ReadAt(p, off) }

// Section returns a reader over [off, off+n) that drops each ~8 MiB it has
// read from the page cache.
func (s *File) Section(off, n int64) io.Reader {
	return &dropReader{f: s, off: off, end: off + n, dropped: off}
}

// Close frees the spool. It is safe to call more than once.
func (s *File) Close() error {
	var err error
	s.closeOnce.Do(func() { err = s.f.Close() })
	return err
}

type dropReader struct {
	f                 *File
	off, end, dropped int64
}

func (r *dropReader) Read(p []byte) (int, error) {
	if r.off >= r.end {
		r.drop()
		return 0, io.EOF
	}
	if rem := r.end - r.off; int64(len(p)) > rem {
		p = p[:rem]
	}
	n, err := r.f.f.ReadAt(p, r.off)
	r.off += int64(n)
	if r.off-r.dropped >= cacheDropEvery {
		r.drop()
	}
	if err == io.EOF && r.off < r.end {
		err = io.ErrUnexpectedEOF
	} else if err == io.EOF {
		err = nil
	}
	return n, err
}

func (r *dropReader) drop() {
	if r.off > r.dropped {
		_ = unix.Fadvise(int(r.f.f.Fd()), r.dropped, r.off-r.dropped, unix.FADV_DONTNEED)
		r.dropped = r.off
	}
}
