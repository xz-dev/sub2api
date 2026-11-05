package bodyspool

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

const testTicket = "0123456789abcdef0123456789abcdef"

func writeTicket(t *testing.T, dir, id string) string {
	t.Helper()
	p := filepath.Join(dir, id+ticketSuffix)
	require.NoError(t, os.WriteFile(p, []byte(`{"id":"`+id+`"}`), 0o440)) // read-only, like the :ro mount
	return p
}

func TestLeaseRefusals(t *testing.T) {
	dir := t.TempDir()
	for name, tc := range map[string]struct {
		dir, id, code string
	}{
		"no_ledger":   {"", testTicket, CodeTicketMissing},
		"no_ticket":   {dir, "", CodeTicketMissing},
		"malformed":   {dir, "../../etc/passwd", CodeTicketInvalid},
		"upper_hex":   {dir, "0123456789ABCDEF0123456789ABCDEF", CodeTicketInvalid},
		"not_present": {dir, testTicket, CodeTicketInvalid},
	} {
		_, err := AcquireLease(tc.dir, tc.id)
		var te *TicketError
		require.ErrorAs(t, err, &te, name)
		require.Equal(t, tc.code, te.Code, name)
	}
}

// TestLeaseHoldsSharedLock: the lease is a LOCK_SH on a read-only fd that
// keeps keeper's exclusive release waiter out until Release.
func TestLeaseHoldsSharedLock(t *testing.T) {
	dir := t.TempDir()
	p := writeTicket(t, dir, testTicket)
	l, err := AcquireLease(dir, testTicket)
	require.NoError(t, err)
	flags, err := unix.FcntlInt(l.f.Fd(), unix.F_GETFL, 0)
	require.NoError(t, err)
	require.Equal(t, unix.O_RDONLY, flags&unix.O_ACCMODE)

	w, err := os.Open(p)
	require.NoError(t, err)
	defer w.Close()
	require.ErrorIs(t, unix.Flock(int(w.Fd()), unix.LOCK_EX|unix.LOCK_NB), unix.EWOULDBLOCK)
	l2, err := AcquireLease(dir, testTicket) // shared: several hops lease at once
	require.NoError(t, err)
	l.Release()
	l.Release()
	require.ErrorIs(t, unix.Flock(int(w.Fd()), unix.LOCK_EX|unix.LOCK_NB), unix.EWOULDBLOCK)
	l2.Release()
	require.NoError(t, unix.Flock(int(w.Fd()), unix.LOCK_EX|unix.LOCK_NB))
}

// TestLeaseBlocksThenRefusesReleasedTicket: LOCK_SH blocks (no TryLock, no
// timer) while keeper holds LOCK_EX; a ticket unlinked meanwhile is refused.
func TestLeaseBlocksThenRefusesReleasedTicket(t *testing.T) {
	dir := t.TempDir()
	p := writeTicket(t, dir, testTicket)
	keeper, err := os.Open(p)
	require.NoError(t, err)
	require.NoError(t, unix.Flock(int(keeper.Fd()), unix.LOCK_EX))

	got := make(chan error, 1)
	go func() {
		l, err := AcquireLease(dir, testTicket)
		l.Release()
		got <- err
	}()
	select {
	case err := <-got:
		t.Fatalf("lease did not block: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	require.NoError(t, os.Remove(p))
	require.NoError(t, keeper.Close())
	err = <-got
	var te *TicketError
	require.ErrorAs(t, err, &te)
	require.Equal(t, CodeTicketInvalid, te.Code)
	require.Contains(t, te.Reason, "released")
}
