package bodyspool

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
)

// Per-hop memory charge (design D4): an atomic byte counter of what in-flight
// requests may still allocate, checked against the hop's own cgroup before
// each step. No count cap, no lock beyond the counter, no timer, no queue.
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

// Meter reads memory.max and memory.current through fds opened once.
type Meter struct {
	max, cur    *os.File
	outstanding atomic.Int64
}

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
	m := &Meter{max: mx, cur: cur}
	if _, _, err := m.read(); err != nil {
		_ = mx.Close()
		_ = cur.Close()
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
	current, err = preadInt(m.cur)
	return limit, current, err
}

// Outstanding is the sum of live charges.
func (m *Meter) Outstanding() int64 { return m.outstanding.Load() }

// Charge is one request's share of the meter.
type Charge struct {
	m *Meter
	n atomic.Int64
}

// NewCharge starts an empty charge.
func (m *Meter) NewCharge() *Charge { return &Charge{m: m} }

// Add takes step bytes after checking
// memory.max − memory.current − outstanding ≥ step + 32 MiB. memory.current
// already includes bytes in use, so the double count errs toward refusing.
// The check and the add are one CAS on the counter.
func (c *Charge) Add(step int64) error {
	if step <= 0 {
		return nil
	}
	limit, current, err := c.m.read()
	if err != nil {
		return err
	}
	for {
		o := c.m.outstanding.Load()
		free := limit - current - o - ChargeMargin
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
