package bodyspool

import (
	"errors"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// Capacity ticket contract (design D4).
const (
	// TicketHeader carries the ticket id from keeper inward. It is forwarded
	// only to internal upstreams and never on an egress path.
	TicketHeader = "X-AI-Gateway-Capacity-Ticket"
	// TicketThreshold: a body of at least this many bytes needs a ticket.
	TicketThreshold int64 = 64 << 20
	// ticketSuffix is the ledger file suffix written by keeper (<id>.ticket).
	ticketSuffix = ".ticket"
	ticketIDLen  = 32 // 128-bit id, lowercase hex
)

// Ticket refusal codes (503).
const (
	CodeTicketMissing = "capacity_ticket_missing"
	CodeTicketInvalid = "capacity_ticket_invalid"
)

// TicketError explains why a ticket lease was refused.
type TicketError struct {
	Code   string
	Reason string
}

func (e *TicketError) Error() string { return "capacity ticket refused: " + e.Reason }

// ValidTicketID reports whether id is a 128-bit lowercase hex ticket id.
func ValidTicketID(id string) bool {
	if len(id) != ticketIDLen {
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// Lease is a shared flock on a ticket file, held for the whole handler
// lifetime (spool and skeleton memory included).
type Lease struct {
	ID string
	f  *os.File
}

// AcquireLease opens ledgerDir/<id>.ticket read-only (O_RDONLY|O_CLOEXEC,
// which works on the read-only ledger mount), takes a blocking LOCK_SH and
// then checks st_nlink > 0: a ticket released while we waited has no link
// and is refused. There is no timeout and no TryLock.
func AcquireLease(ledgerDir, id string) (*Lease, error) {
	if ledgerDir == "" {
		return nil, &TicketError{Code: CodeTicketMissing, Reason: "no capacity ledger is configured"}
	}
	if id == "" {
		return nil, &TicketError{Code: CodeTicketMissing, Reason: "no capacity ticket was sent"}
	}
	if !ValidTicketID(id) {
		return nil, &TicketError{Code: CodeTicketInvalid, Reason: "malformed ticket id"}
	}
	f, err := os.OpenFile(filepath.Join(ledgerDir, id+ticketSuffix), os.O_RDONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, &TicketError{Code: CodeTicketInvalid, Reason: "cannot open ticket"}
	}
	for {
		err = unix.Flock(int(f.Fd()), unix.LOCK_SH)
		if !errors.Is(err, unix.EINTR) {
			break
		}
	}
	if err != nil {
		_ = f.Close()
		return nil, &TicketError{Code: CodeTicketInvalid, Reason: "cannot lock ticket"}
	}
	var st unix.Stat_t
	if err := unix.Fstat(int(f.Fd()), &st); err != nil || st.Nlink == 0 {
		_ = f.Close()
		return nil, &TicketError{Code: CodeTicketInvalid, Reason: "ticket already released"}
	}
	return &Lease{ID: id, f: f}, nil
}

// Release drops the lease (closing the fd drops the flock). Idempotent.
func (l *Lease) Release() {
	if l != nil && l.f != nil {
		_ = l.f.Close()
		l.f = nil
	}
}
