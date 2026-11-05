package bodyspool

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func fakeCgroup(t *testing.T, max, current string) (string, *Meter) {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "memory.max"), []byte(max+"\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "memory.current"), []byte(current+"\n"), 0o600))
	writeStat(t, dir, 0)
	m, err := OpenMeter(dir)
	if err != nil {
		return dir, nil
	}
	return dir, m
}

const mib = 1 << 20

func writeStat(t *testing.T, dir string, inactiveFile int64) {
	t.Helper()
	stat := "anon 1000\nfile 2000\nactive_file 300\ninactive_file " + strconv.FormatInt(inactiveFile, 10) + "\nslab 10\n"
	require.NoError(t, os.WriteFile(filepath.Join(dir, "memory.stat"), []byte(stat), 0o600))
}

func TestWorkingSetBytes(t *testing.T) {
	for _, tc := range []struct {
		name    string
		current int64
		stat    string
		want    int64
		wantErr bool
	}{
		{"subtracts inactive_file", 1000, "anon 200\nactive_file 100\ninactive_file 700\n", 300, false},
		{"active_file is not a match", 1000, "active_file 900\ninactive_file 0\n", 1000, false},
		{"inactive_file above current keeps current", 500, "inactive_file 700\n", 500, false},
		{"no trailing newline", 1000, "inactive_file 1000", 0, false},
		{"missing line", 1000, "anon 1000\nfile 0\n", 0, true},
		{"empty stat", 1000, "", 0, true},
		{"unparsable value", 1000, "inactive_file x\n", 0, true},
		{"negative value", 1000, "inactive_file -1\n", 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := workingSetBytes(tc.current, []byte(tc.stat))
			if tc.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.want, got)
		})
	}
}

// TestChargeBaseIsWorkingSet: the fresh-hop case. 85 MiB current of which
// 40 MiB is inactive page cache leaves room for the 224 MiB text-16 charge.
func TestChargeBaseIsWorkingSet(t *testing.T) {
	dir, m := fakeCgroup(t, strconv.Itoa(320*mib), strconv.Itoa(85*mib))
	require.NotNil(t, m)
	c := newCharge(t, m)
	require.Error(t, c.Add(224*mib), "320 − 85 − 32 = 203 < 224")
	c.Release()
	m.Reclaim = nil
	writeStat(t, dir, 40*mib)
	c = newCharge(t, m)
	require.NoError(t, c.Add(224*mib), "320 − (85 − 40) − 32 = 243 ≥ 224")
	c.Release()
}

// TestChargeRefusesUnreadableStat: without memory.stat the meter does not
// open, and a stat that later turns unreadable refuses the request.
func TestChargeRefusesUnreadableStat(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "memory.max"), []byte(strconv.Itoa(320*mib)), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "memory.current"), []byte("0"), 0o600))
	_, err := OpenMeter(dir)
	require.ErrorContains(t, err, "memory.stat")

	require.NoError(t, os.WriteFile(filepath.Join(dir, "memory.stat"), []byte("anon 0\n"), 0o600))
	_, err = OpenMeter(dir)
	require.ErrorContains(t, err, "no inactive_file line")

	writeStat(t, dir, 0)
	m, err := OpenMeter(dir)
	require.NoError(t, err)
	defer m.Close()
	require.NoError(t, os.Truncate(filepath.Join(dir, "memory.stat"), 0))
	_, err = m.NewCharge()
	require.ErrorContains(t, err, "memory.stat")
	require.Zero(t, m.Outstanding())

	// The reclaim re-read refuses too: it never falls back to admitting.
	writeStat(t, dir, 0)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "memory.current"), []byte(strconv.Itoa(300*mib)), 0o600))
	c, err := m.NewCharge()
	require.NoError(t, err)
	m.Reclaim = func() { _ = os.Truncate(filepath.Join(dir, "memory.stat"), 0) }
	require.ErrorContains(t, c.Add(mib), "memory.stat")
	require.Zero(t, m.Outstanding())
}

func newCharge(t *testing.T, m *Meter) *Charge {
	t.Helper()
	c, err := m.NewCharge()
	require.NoError(t, err)
	return c
}

func TestMeterRefusesUnlimitedOrMissingCgroup(t *testing.T) {
	_, m := fakeCgroup(t, "max", "0")
	require.Nil(t, m, "an unlimited memory.max is a configuration error, never a soft check")
	_, err := OpenMeter(t.TempDir())
	require.Error(t, err)
}

// TestChargeRefusesBeforeCrossingMargin: max − current − outstanding must
// cover step + 32 MiB; the refusal names the bytes and frees nothing else.
func TestChargeRefusesBeforeCrossingMargin(t *testing.T) {
	dir, m := fakeCgroup(t, strconv.Itoa(320*mib), strconv.Itoa(45*mib))
	a := newCharge(t, m)
	require.NoError(t, a.Add(200*mib)) // 320 − 45 − 0 − 32 = 243 ≥ 200
	b := newCharge(t, m)
	err := b.Add(12 * mib) // 243 − 200 = 43 ≥ 12
	require.NoError(t, err)
	err = b.Add(32 * mib) // 31 < 32
	var ime *InsufficientMemoryError
	require.ErrorAs(t, err, &ime)
	require.EqualValues(t, 32*mib, ime.Need)
	require.EqualValues(t, 31*mib, ime.Free)
	require.EqualValues(t, 212*mib, m.Outstanding())

	// A new charge snapshots memory.current: growth before it refuses it.
	require.NoError(t, os.WriteFile(filepath.Join(dir, "memory.current"), []byte(strconv.Itoa(70*mib)), 0o600))
	c := newCharge(t, m)
	require.Error(t, c.Add(10*mib)) // 320 − 70 − 212 − 32 = 6 < 10
	require.NoError(t, b.Add(10*mib), "b keeps its own snapshot (45 MiB): 31 − 0 ≥ 10")
	c.Release()
	a.Release()
	b.Release()
	b.Release()
	require.Zero(t, m.Outstanding())
}

// TestConcurrentChargeNeverCrossesLimit: racing chargers never let the
// outstanding total pass max − current − 32 MiB; losers get the 503 error.
func TestConcurrentChargeNeverCrossesLimit(t *testing.T) {
	_, m := fakeCgroup(t, strconv.Itoa(320*mib), strconv.Itoa(45*mib))
	const budget = (320 - 45 - 32) * mib
	var wg, attempted sync.WaitGroup
	var refused, peak atomic.Int64
	hold := make(chan struct{}) // every charge is held until all have tried
	for i := 0; i < 64; i++ {
		wg.Add(1)
		attempted.Add(1)
		go func() {
			defer wg.Done()
			c := newCharge(t, m)
			defer c.Release()
			defer func() { attempted.Done(); <-hold }()
			for step := 0; step < 16; step++ { // a 16 MiB text body at k_skel 1, per MiB
				if err := c.Add(mib); err != nil {
					refused.Add(1)
					return
				}
				for o := m.Outstanding(); ; {
					if p := peak.Load(); o <= p || peak.CompareAndSwap(p, o) {
						break
					}
				}
			}
		}()
	}
	attempted.Wait()
	close(hold)
	wg.Wait()
	require.LessOrEqual(t, peak.Load(), int64(budget))
	require.Positive(t, refused.Load(), "64 x 16 MiB cannot fit 243 MiB")
	require.Zero(t, m.Outstanding())
}

// TestTicketedChargeUnderSmallLoad (task 4.2): one ticketed 256 MiB request
// (k_mem 0.1 × N + P_h 16 MiB + k_skel 15 × 8 MiB inline, 161.6 MiB) holds
// its charge at Sub2API's 320m with a 45 MiB working set while 120 truly
// small requests (a 40-byte body each) ingest concurrently with the default
// charge step and hold their charges, as during a streamed response. All of
// them, and the ticketed one, are admitted: the charge follows k × the real
// inline bytes in small granules, not a per-request minimum.
func TestTicketedChargeUnderSmallLoad(t *testing.T) {
	_, m := fakeCgroup(t, strconv.Itoa(320*mib), strconv.Itoa(45*mib))
	m.Reclaim = nil
	const budget = (320 - 45 - 32) * mib
	const kSkel = 15
	big := newCharge(t, m)
	require.NoError(t, big.Add(256*mib/10+16*mib+kSkel*8*mib), "the ticketed request is admitted")

	d := testDir(t)
	body := []byte(`{"model":"gpt-5.5","input":"hi there"}`)
	const small = 120
	var ingested, hold sync.WaitGroup
	ingested.Add(small)
	hold.Add(1)
	errs := make(chan error, small)
	var done sync.WaitGroup
	for i := 0; i < small; i++ {
		done.Add(1)
		go func() {
			defer done.Done()
			c, err := m.NewCharge()
			if err != nil {
				errs <- err
				ingested.Done()
				return
			}
			defer c.Release()
			f, err := d.Create()
			if err != nil {
				errs <- err
				ingested.Done()
				return
			}
			defer func() { _ = f.Close() }()
			_, skel, err := Ingest(bytes.NewReader(body), f, Options{Charge: func(n int64) error { return c.Add(kSkel * n) }})
			if err == nil && !bytes.Equal(skel, body) {
				err = errors.New("skeleton differs from the body")
			}
			if err != nil {
				errs <- err
			}
			ingested.Done()
			hold.Wait() // keep the charge, as a streamed response does
		}()
	}
	ingested.Wait()
	require.LessOrEqual(t, m.Outstanding(), int64(budget))
	hold.Done()
	done.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err, "a small request was refused next to the ticketed one")
	}
	big.Release()
	require.Zero(t, m.Outstanding())
}

// TestChargeCountsOwnBytesOnce: memory.current grows with the bytes a request
// has charged and allocated. A single 16 MiB text body at k_skel 15 (240 MiB)
// on 320m with a 45 MiB idle must fit, charged in 1 MiB steps or at once; a
// fresh memory.current read per step refused it at about 11 MiB.
func TestChargeCountsOwnBytesOnce(t *testing.T) {
	const idle = 45 * mib
	for _, steps := range []int64{16, 1} {
		dir, m := fakeCgroup(t, strconv.Itoa(320*mib), strconv.Itoa(idle))
		c := newCharge(t, m)
		per := 15 * 16 * mib / steps
		for i := int64(0); i < steps; i++ {
			require.NoError(t, c.Add(per), "step %d of %d", i+1, steps)
			// The charged bytes become live in the cgroup.
			require.NoError(t, os.WriteFile(filepath.Join(dir, "memory.current"), []byte(strconv.FormatInt(idle+c.Bytes(), 10)), 0o600))
		}
		require.EqualValues(t, 240*mib, c.Bytes())
		// A second request starting now sees idle + 240 MiB and the 240 MiB
		// charge: refused, even for 1 MiB.
		other := newCharge(t, m)
		var ime *InsufficientMemoryError
		require.ErrorAs(t, other.Add(mib), &ime)
		c.Release()
		require.Zero(t, m.Outstanding())
	}
}

// TestChargeReclaimsOnceBeforeRefusing: a first charge that does not fit
// returns freed heap to the OS once and re-reads memory.current; a charge
// that already holds bytes never re-snapshots (its bytes would count twice),
// and reclaiming is rate-limited across refusals.
func TestChargeReclaimsOnceBeforeRefusing(t *testing.T) {
	dir, m := fakeCgroup(t, strconv.Itoa(320*mib), strconv.Itoa(95*mib))
	var reclaims int
	m.Reclaim = func() {
		reclaims++
		require.NoError(t, os.WriteFile(filepath.Join(dir, "memory.current"), []byte(strconv.Itoa(50*mib)), 0o600))
	}
	c := newCharge(t, m)
	require.NoError(t, c.Add(224*mib), "320 − 50 − 32 = 238 ≥ 224 after reclaiming (193 before)")
	require.Equal(t, 1, reclaims)

	d := newCharge(t, m) // 238 − 224 = 14 free
	require.NoError(t, d.Add(10*mib))
	var ime *InsufficientMemoryError
	require.ErrorAs(t, d.Add(10*mib), &ime, "d holds bytes: no re-snapshot")
	require.Equal(t, 1, reclaims)

	e := newCharge(t, m)
	require.ErrorAs(t, e.Add(10*mib), &ime) // 4 free; reclaim is rate-limited
	require.Equal(t, 1, reclaims)
	c.Release()
	d.Release()
	require.Zero(t, m.Outstanding())
}

// TestReclaimReleasedIsRateLimited: ReclaimReleased starts one background
// ReleaseReclaim; a second call within reclaimInterval does nothing. Release
// itself never reclaims (the request may still hold its buffers), and the
// refusal path's Reclaim is not involved.
func TestReclaimReleasedIsRateLimited(t *testing.T) {
	_, m := fakeCgroup(t, strconv.Itoa(320*mib), strconv.Itoa(40*mib))
	runs := make(chan struct{}, 8)
	m.ReleaseReclaim = func() { runs <- struct{}{} }
	m.Reclaim = func() { t.Error("refusal-path Reclaim called") }

	large := newCharge(t, m)
	require.NoError(t, large.Add(ReclaimAfterRelease))
	large.Release()
	select {
	case <-runs:
		t.Fatal("Release reclaimed by itself")
	case <-time.After(100 * time.Millisecond):
	}

	m.ReclaimReleased()
	select {
	case <-runs:
	case <-time.After(5 * time.Second):
		t.Fatal("ReclaimReleased did not reclaim")
	}
	m.ReclaimReleased()
	select {
	case <-runs:
		t.Fatal("a second call within reclaimInterval reclaimed")
	case <-time.After(200 * time.Millisecond):
	}
	require.Zero(t, m.Outstanding())
}

// TestChargeDetachOutlivesRelease: detached bytes stay outstanding after the
// request's Release until their own release, which runs once.
func TestChargeDetachOutlivesRelease(t *testing.T) {
	_, m := fakeCgroup(t, strconv.Itoa(320*mib), strconv.Itoa(40*mib))
	c, err := m.NewCharge()
	require.NoError(t, err)
	require.NoError(t, c.Add(10*mib))
	release := c.Detach(3 * mib)
	require.EqualValues(t, 7*mib, c.Bytes())
	require.EqualValues(t, 10*mib, m.Outstanding())
	c.Release()
	require.EqualValues(t, 3*mib, m.Outstanding(), "detached bytes outlive the request's release")
	release()
	release()
	require.Zero(t, m.Outstanding())

	c, err = m.NewCharge()
	require.NoError(t, err)
	require.NoError(t, c.Add(mib))
	release = c.Detach(5 * mib) // at most what is charged
	require.Zero(t, c.Bytes())
	c.Release()
	require.EqualValues(t, mib, m.Outstanding())
	release()
	require.Zero(t, m.Outstanding())
}

// TestMeterAcquireWaitsForRelease: background work that does not fit waits,
// running wait before each wait, until a charge is released; then it holds
// its bytes until its release func runs, once. ctx and wait end the wait.
func TestMeterAcquireWaitsForRelease(t *testing.T) {
	_, m := fakeCgroup(t, strconv.Itoa(100*mib), "0")
	m.Reclaim = nil
	req, err := m.NewCharge()
	require.NoError(t, err)
	require.NoError(t, req.Add(50*mib)) // free: 100 - 32 - 50 = 18 MiB

	waits := make(chan struct{}, 8)
	type result struct {
		release func()
		err     error
	}
	got := make(chan result, 1)
	go func() {
		release, err := m.Acquire(context.Background(), 30*mib, func() error { waits <- struct{}{}; return nil })
		got <- result{release, err}
	}()
	<-waits
	select {
	case r := <-got:
		t.Fatalf("acquired %v while 30 MiB did not fit", r.err)
	default:
	}
	require.EqualValues(t, 50*mib, m.Outstanding())
	req.Release() // the request ends: the waiting work wakes and fits
	r := <-got
	require.NoError(t, r.err)
	require.EqualValues(t, 30*mib, m.Outstanding())
	r.release()
	r.release()
	require.Zero(t, m.Outstanding())

	ctx, cancel := context.WithCancel(context.Background())
	big := make(chan error, 1)
	go func() {
		_, err := m.Acquire(ctx, 80*mib, func() error { waits <- struct{}{}; return nil })
		big <- err
	}()
	<-waits
	cancel()
	require.ErrorIs(t, <-big, context.Canceled)
	stop := errors.New("lease lost")
	_, err = m.Acquire(context.Background(), 80*mib, func() error { return stop })
	require.ErrorIs(t, err, stop)
	require.Zero(t, m.Outstanding())
}

// TestSharedMeterIsOnePerCgroup: every caller for a cgroup shares one
// outstanding counter.
func TestSharedMeterIsOnePerCgroup(t *testing.T) {
	dir, _ := fakeCgroup(t, strconv.Itoa(100*mib), "0")
	a, err := SharedMeter(dir)
	require.NoError(t, err)
	b, err := SharedMeter(dir)
	require.NoError(t, err)
	require.Same(t, a, b)
}
