package handler

import (
	"sync"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/bodyspool"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

// openAIWSReadBudget is the body-spool charge of one native WS ingress
// connection's client messages (service.OpenAIWSClientReadBudget; exit gate
// 7.5 finding 1). The ingress is not a covered path for this hop: Responses
// turns large enough to matter reach Sub2API through the bridge as HTTP
// POSTs. So a native WS message is bounded like a fallback body (design:
// "a native non-Responses WS message above L gets a named
// request_too_large error event and a normal close"):
//   - the limit is L, the effective fallback limit (11,735,040 B at 320m),
//     or gateway.openai_ws.client_read_limit_bytes when that is lower;
//   - each message is charged (k_path + the prompt guard's decode) x its
//     bytes, plus the guard's fixed bytes, the charge L is derived from, so
//     a message of L fits an idle hop. The WS turn's guard audit, normalizers
//     and forwarding run on that charge, as a fallback body's do.
type openAIWSReadBudget struct {
	meter *bodyspool.Meter
	limit int64
	k     float64

	mu     sync.Mutex
	charge *bodyspool.Charge
}

var _ service.OpenAIWSClientReadBudget = (*openAIWSReadBudget)(nil)

// newOpenAIWSReadBudget returns nil, nil when the body spool is off: the
// library read limit (client_read_limit_bytes) then applies as before.
func newOpenAIWSReadBudget(cfg *config.Config) (*openAIWSReadBudget, error) {
	rt := bodySpoolRuntimeFor(cfg)
	if rt == nil {
		return nil, nil
	}
	if rt.err != nil {
		return nil, rt.err
	}
	probe, err := rt.meter.NewCharge() // reads memory.max; charges nothing
	if err != nil {
		return nil, err
	}
	limit := min(fallbackLimit(rt.cfg, probe.Limit()), service.ResolveOpenAIWSClientReadLimitBytes(cfg))
	return &openAIWSReadBudget{meter: rt.meter, limit: limit, k: rt.cfg.KPath + guardDecodeK}, nil
}

func (b *openAIWSReadBudget) Limit() int64 { return b.limit }

func (b *openAIWSReadBudget) Begin() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.endLocked()
	c, err := b.meter.NewCharge() // before any of the message's payload exists
	if err != nil {
		return err
	}
	b.charge = c
	return nil
}

func (b *openAIWSReadBudget) Cover(n, alloc int64) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.charge == nil {
		return nil
	}
	target := max(chargeBytes(b.k, roundUpGranule(n))+guardFixedBytes, alloc)
	if have := b.charge.Bytes(); target > have {
		return b.charge.Add(target - have)
	}
	return nil
}

func (b *openAIWSReadBudget) End() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.endLocked()
}

// endLocked releases the current charge. After a large one it returns the
// freed heap to the OS, through the meter's single-flight, rate-limited
// release (the same event-triggered release a large HTTP request ends with).
func (b *openAIWSReadBudget) endLocked() {
	if b.charge == nil {
		return
	}
	n := b.charge.Bytes()
	b.charge.Release()
	b.charge = nil
	if n >= bodyspool.ReclaimAfterRelease {
		b.meter.ReclaimReleased()
	}
}
