package bodyspool

import (
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
)

func fakeCgroup(t *testing.T, max, current string) (string, *Meter) {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "memory.max"), []byte(max+"\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "memory.current"), []byte(current+"\n"), 0o600))
	m, err := OpenMeter(dir)
	if err != nil {
		return dir, nil
	}
	return dir, m
}

const mib = 1 << 20

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
	a := m.NewCharge()
	require.NoError(t, a.Add(200*mib)) // 320 − 45 − 0 − 32 = 243 ≥ 200
	b := m.NewCharge()
	err := b.Add(12 * mib) // 243 − 200 = 43 ≥ 12
	require.NoError(t, err)
	err = b.Add(32 * mib) // 31 < 32
	var ime *InsufficientMemoryError
	require.ErrorAs(t, err, &ime)
	require.EqualValues(t, 32*mib, ime.Need)
	require.EqualValues(t, 31*mib, ime.Free)
	require.EqualValues(t, 212*mib, m.Outstanding())

	// memory.current is read on every step: growth elsewhere refuses too.
	require.NoError(t, os.WriteFile(filepath.Join(dir, "memory.current"), []byte(strconv.Itoa(70*mib)), 0o600))
	require.Error(t, b.Add(10*mib)) // 320 − 70 − 212 − 32 = 6 < 10
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
			c := m.NewCharge()
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

// TestTicketedChargeUnderSmallLoad: one ticketed 256 MiB request (k_mem 0.1 ×
// N + P_h 16 MiB + k_skel 15 × 8 MiB inline) against concurrent small
// requests at Sub2API's 320m: whatever interleaving, the sum stays inside
// the budget and every refusal is an explicit InsufficientMemoryError.
func TestTicketedChargeUnderSmallLoad(t *testing.T) {
	_, m := fakeCgroup(t, strconv.Itoa(320*mib), strconv.Itoa(45*mib))
	const budget = (320 - 45 - 32) * mib
	ticketed := int64(256*mib/10 + 16*mib)
	var wg sync.WaitGroup
	var bigErr error
	wg.Add(1)
	go func() {
		defer wg.Done()
		c := m.NewCharge()
		defer c.Release()
		if bigErr = c.Add(ticketed); bigErr != nil {
			return
		}
		for i := 0; i < 8 && bigErr == nil; i++ {
			bigErr = c.Add(15 * mib)
		}
	}()
	var otherErrs atomic.Int64
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c := m.NewCharge()
			defer c.Release()
			for s := 0; s < 4; s++ {
				if err := c.Add(15 * mib); err != nil {
					var ime *InsufficientMemoryError
					require.ErrorAs(t, err, &ime)
					otherErrs.Add(1)
					return
				}
				require.LessOrEqual(t, m.Outstanding(), int64(budget))
			}
		}()
	}
	wg.Wait()
	if bigErr != nil {
		var ime *InsufficientMemoryError
		require.ErrorAs(t, bigErr, &ime)
	}
	require.Zero(t, m.Outstanding())
}
