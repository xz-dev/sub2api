package bodyspool

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Per-hop memory charge (design D4): an atomic byte counter of what in-flight
// requests may still allocate, checked against the hop's own cgroup before
// each step. No count cap, no lock beyond the counter, no timer, no queue.
//
// Each charge checks against the memory.current it read when it started,
// before any of its own bytes existed. A fresh read would count the
// request's already-charged bytes twice (once in memory.current, once in its
// own charge) and refuse a 16 MiB text body at about 11 MiB on 320m. Other
// requests' live bytes are in the snapshot and in their charges, so that
// double count stays and errs toward refusing.
//
// The snapshot is the working set, memory.current − inactive_file (keeper
// design D3): a fresh hop's memory.current is mostly reclaimable page cache
// from startup, which the kernel drops before it OOM-kills. Active file
// pages stay counted. An unreadable memory.stat refuses; it never admits.
const ChargeMargin int64 = 32 << 20

// CodeInsufficientMemory is the 503 refusal code.
const CodeInsufficientMemory = "insufficient_memory"

// InsufficientMemoryError: the step does not fit the cgroup headroom.
type InsufficientMemoryError struct {
	Need, Free int64
}

func (e *InsufficientMemoryError) Error() string {
	return fmt.Sprintf("insufficient memory: %d bytes are needed and %d are free in the cgroup after outstanding charges and the %d-byte margin", e.Need, e.Free, ChargeMargin)
}

// Meter reads memory.max, memory.current and memory.stat through fds opened
// once.
type Meter struct {
	max, cur    *os.File
	stat        *os.File
	outstanding atomic.Int64

	// Reclaim returns freed heap to the OS (debug.FreeOSMemory). A first
	// charge that does not fit calls it once and re-reads memory.current:
	// garbage the GC has not yet returned is not memory in use.
	Reclaim       func()
	reclaimMu     sync.Mutex
	lastReclaimed time.Time
}

// reclaimInterval rate-limits Reclaim across concurrent refusals.
const reclaimInterval = time.Second

// OpenMeter opens the cgroup v2 memory files in dir, or in the process's own
// cgroup when dir is empty. A missing file or an unlimited memory.max is an
// explicit configuration error: without a limit there is nothing to charge
// against, and the charge is never soft.
func OpenMeter(dir string) (*Meter, error) {
	if dir == "" {
		b, err := os.ReadFile("/proc/self/cgroup")
		if err != nil {
			return nil, fmt.Errorf("bodyspool: read /proc/self/cgroup: %w", err)
		}
		for _, line := range strings.Split(string(b), "\n") {
			if p, ok := strings.CutPrefix(line, "0::"); ok {
				dir = filepath.Join("/sys/fs/cgroup", p)
			}
		}
		if dir == "" {
			return nil, errors.New("bodyspool: no cgroup v2 membership for the memory charge")
		}
	}
	mx, err := os.Open(filepath.Join(dir, "memory.max"))
	if err != nil {
		return nil, fmt.Errorf("bodyspool: memory charge: %w", err)
	}
	cur, err := os.Open(filepath.Join(dir, "memory.current"))
	if err != nil {
		_ = mx.Close()
		return nil, fmt.Errorf("bodyspool: memory charge: %w", err)
	}
	stat, err := os.Open(filepath.Join(dir, "memory.stat"))
	if err != nil {
		_ = mx.Close()
		_ = cur.Close()
		return nil, fmt.Errorf("bodyspool: memory charge: %w", err)
	}
	m := &Meter{max: mx, cur: cur, stat: stat, Reclaim: debug.FreeOSMemory}
	if _, _, err := m.read(); err != nil {
		m.Close()
		return nil, err
	}
	return m, nil
}

func preadInt(f *os.File) (int64, error) {
	var buf [32]byte
	n, _ := f.ReadAt(buf[:], 0)
	s := string(bytes.TrimSpace(buf[:n]))
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("bodyspool: memory charge: cgroup value %q of %s is not a byte limit", s, f.Name())
	}
	return v, nil
}

func (m *Meter) read() (limit, current int64, err error) {
	if limit, err = preadInt(m.max); err != nil {
		return 0, 0, err
	}
	if current, err = preadInt(m.cur); err != nil {
		return 0, 0, err
	}
	// memory.stat is about 1.5 KiB on current kernels.
	var buf [16 << 10]byte
	n, rerr := m.stat.ReadAt(buf[:], 0)
	if n == 0 && rerr != nil {
		return 0, 0, fmt.Errorf("bodyspool: memory charge: read %s: %w", m.stat.Name(), rerr)
	}
	if current, err = workingSetBytes(current, buf[:n]); err != nil {
		return 0, 0, fmt.Errorf("bodyspool: memory charge: %s: %w", m.stat.Name(), err)
	}
	return limit, current, nil
}

// workingSetBytes is memory.current minus the inactive_file of memory.stat.
// A stat without a parsable inactive_file line is an error. inactive_file
// above current (the two files are read separately) keeps current.
func workingSetBytes(current int64, stat []byte) (int64, error) {
	for _, line := range bytes.Split(stat, []byte("\n")) {
		v, ok := bytes.CutPrefix(line, []byte("inactive_file "))
		if !ok {
			continue
		}
		n, err := strconv.ParseInt(string(bytes.TrimSpace(v)), 10, 64)
		if err != nil || n < 0 {
			return 0, fmt.Errorf("inactive_file %q is not a byte count", v)
		}
		if n > current {
			return current, nil
		}
		return current - n, nil
	}
	return 0, errors.New("no inactive_file line")
}

// Close releases the cgroup file descriptors.
func (m *Meter) Close() {
	_ = m.max.Close()
	_ = m.cur.Close()
	_ = m.stat.Close()
}

// Outstanding is the sum of live charges.
func (m *Meter) Outstanding() int64 { return m.outstanding.Load() }

// Charge is one request's share of the meter.
type Charge struct {
	m           *Meter
	limit, base int64 // memory.max and the working set at NewCharge
	n           atomic.Int64
}

// NewCharge starts an empty charge. Call it before the request's body is
// read: it snapshots the working set while none of the request's bytes exist.
func (m *Meter) NewCharge() (*Charge, error) {
	limit, current, err := m.read()
	if err != nil {
		return nil, err
	}
	return &Charge{m: m, limit: limit, base: current}, nil
}

// Add takes step bytes after checking
// memory.max − base − outstanding ≥ step + 32 MiB, where base is the
// snapshot from NewCharge and outstanding includes this charge, so the
// request's own bytes count once. The check and the add are one CAS.
func (c *Charge) Add(step int64) error {
	err := c.add(step)
	var ime *InsufficientMemoryError
	if !errors.As(err, &ime) || c.n.Load() != 0 || c.m.Reclaim == nil {
		return err
	}
	// Nothing of this request is charged or allocated yet, so a fresh
	// snapshot after reclaiming still excludes its own bytes.
	c.m.reclaim()
	limit, current, rerr := c.m.read()
	if rerr != nil {
		return rerr
	}
	c.limit, c.base = limit, current
	return c.add(step)
}

func (m *Meter) reclaim() {
	m.reclaimMu.Lock()
	defer m.reclaimMu.Unlock()
	if time.Since(m.lastReclaimed) >= reclaimInterval {
		m.Reclaim()
		m.lastReclaimed = time.Now()
	}
}

func (c *Charge) add(step int64) error {
	if step <= 0 {
		return nil
	}
	for {
		o := c.m.outstanding.Load()
		free := c.limit - c.base - o - ChargeMargin
		if free < step {
			return &InsufficientMemoryError{Need: step, Free: max(0, free)}
		}
		if c.m.outstanding.CompareAndSwap(o, o+step) {
			c.n.Add(step)
			return nil
		}
	}
}

// Bytes is the amount charged so far.
func (c *Charge) Bytes() int64 { return c.n.Load() }

// Release returns the whole charge. Idempotent.
func (c *Charge) Release() {
	if c == nil {
		return
	}
	if n := c.n.Swap(0); n > 0 {
		c.m.outstanding.Add(-n)
	}
}
