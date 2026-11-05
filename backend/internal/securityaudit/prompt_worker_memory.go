package securityaudit

import "context"

// MemoryBudget is the hop's memory budget that requests charge (the body
// spool meter, bodyspool.Meter). The async worker's audit belongs to no
// request, so it charges the budget itself before it reads a payload back.
type MemoryBudget interface {
	// Acquire charges n bytes, waiting for released bytes while they do not
	// fit. wait runs before each wait; its error, ctx, or an unreadable
	// budget end it. release returns the bytes, once.
	Acquire(ctx context.Context, n int64, wait func() error) (release func(), err error)
}

// SetMemoryBudget makes the async worker charge b before it reads a payload
// back. Call it before Start; nil (the default) charges nothing, as when the
// body spool, and with it the budget, is off.
func (s *PromptService) SetMemoryBudget(b MemoryBudget) {
	if s != nil && s.runner != nil {
		s.runner.memory = b
	}
}

// What the worker holds while it audits one payload, besides the payload.
// Measured with the real guard client (TestWorkerChargeCoversAudit): about
// 330 B kept per chunk, and for the chunk being scanned at most 4.8 MB at
// 100,000 runes of "<" (38 B per rune for the escaped request and its
// buffers, 8 B per rune kept in the encoder pool) and 1.0 MB at 128 runes
// (a response of almost maxGuardResponseBytes, read and parsed).
const (
	// fullPromptBound: FullPromptFromScanText keeps at most
	// DefaultFullPromptMaxRunes runes (4 bytes each) and "…".
	fullPromptBound = 4*DefaultFullPromptMaxRunes + 3
	// workerChunkLive: per chunk, its string in SplitRunes' result, its
	// result pointer and the result kept for AggregateResults.
	workerChunkLive = 512
	// workerChunkWorkPerRune and workerChunkWorkFixed: the one chunk being
	// scanned: the guard request (json escapes up to 6 bytes per rune) and
	// its encoder buffer, the HTTP exchange, the response read and parsed,
	// and the client's own state.
	workerChunkWorkPerRune = 48
	workerChunkWorkFixed   = 4*maxGuardResponseBytes + 1<<20
)

// workerChargeBytes bounds the worker's memory for a payload of n bytes
// scanned in chunks of at most limit runes: the payload as read (n + 2), the
// stored prompt, the chunk list and results (a chunk has at least one rune,
// so there are at most n/limit + 1), and the one chunk being scanned.
func workerChargeBytes(n int64, limit int) int64 {
	if limit <= 0 {
		limit = DefaultInputLimit
	}
	chunks := n/int64(limit) + 1
	return n + 2 + fullPromptBound + chunks*workerChunkLive +
		workerChunkWorkPerRune*int64(limit) + workerChunkWorkFixed
}

// chargePayloadRead takes the worker's charge for job before its payload is
// read: it waits while the budget is short, keeping the job's lease at each
// wait. The audit is never dropped for memory: a lost lease or a shutdown
// leaves the job to the reclaimer and a later attempt.
func (r *Runner) chargePayloadRead(ctx context.Context, job *Job, n int64, limit int) (release func(), err error) {
	if r.memory == nil {
		return func() {}, nil
	}
	return r.memory.Acquire(ctx, workerChargeBytes(n, limit), func() error {
		return r.repo.RefreshLease(ctx, job.ID, job.ClaimVersion, r.clock.Now())
	})
}
