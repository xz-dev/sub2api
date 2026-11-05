//go:build unit

package handler

import (
	"context"
	"net/http"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/securityaudit"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// TestBodySpoolGuardDecodeChargedOnFallbackPaths: on a fallback path (no
// k_skel envelope) the prompt guard's decode is charged before it runs, at
// guardDecodeK × the body in granules plus guardFixedBytes, and only while the guard reads the
// body. A covered path's charge is unchanged by the guard: its decode runs
// inside the k_skel envelope, which was measured with the guard on.
func TestBodySpoolGuardDecodeChargedOnFallbackPaths(t *testing.T) {
	gin.SetMode(gin.TestMode)
	body := largeResponsesTextBody(256 << 10)
	held := func(path string, engine securityaudit.PromptEngine) int64 {
		upstream := &largeBodyDrainUpstream{block: make(chan struct{}), entered: make(chan struct{})}
		h := newLargeBodyTestHandler(t, upstream, "oauth")
		h.securityAuditCoordinator = securityaudit.NewCoordinator(nil, engine)
		enableBodySpoolForTest(t, h.cfg, true, 4<<30, nil)
		meter := bodySpoolRuntimeFor(h.cfg).meter
		c, rec := newLargeBodyTestContext(path, body, "")
		done := make(chan struct{})
		go func() { defer close(done); h.Responses(c) }()
		select {
		case <-upstream.entered:
		case <-done:
			t.Fatalf("%s: no upstream call: %d %s", path, rec.Code, rec.Body.String())
		}
		n := meter.Outstanding()
		close(upstream.block)
		<-done
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		require.Zero(t, meter.Outstanding())
		return n
	}
	want := chargeBytes(guardDecodeK, roundUpGranule(int64(len(body)))) + guardFixedBytes

	on, off := newGuardEngine(), &offPromptEngine{}
	fallbackOn, fallbackOff := held("/v1/responses/compact", on), held("/v1/responses/compact", off)
	require.Len(t, on.bodies, 1, "the guard read the fallback body")
	require.EqualValues(t, want, fallbackOn-fallbackOff, "fallback: the guard decode is charged at guardDecodeK × the body + guardFixedBytes")

	on = newGuardEngine()
	coveredOn, coveredOff := held("/v1/responses", on), held("/v1/responses", off)
	require.Len(t, on.bodies, 1, "the guard read the covered body")
	require.Equal(t, coveredOff, coveredOn, "covered: the k_skel envelope already holds the decode")
}

// asyncHoldEngine is an async prompt engine that keeps n bytes of the
// request's charge, like PromptService.Enqueue, until done closes.
type asyncHoldEngine struct {
	n    int64
	done chan struct{}
	held chan struct{}
}

func (e *asyncHoldEngine) EffectiveMode() securityaudit.Mode { return securityaudit.ModeAsync }
func (e *asyncHoldEngine) Enqueue(_ context.Context, req securityaudit.Request) error {
	release := req.RetainMemory(e.n)
	go func() { <-e.done; release(); close(e.held) }()
	return nil
}
func (e *asyncHoldEngine) Evaluate(context.Context, securityaudit.Request) (*securityaudit.PromptDecision, error) {
	return nil, nil
}

// TestBodySpoolAsyncAuditHoldOutlivesRequest (review a4a7d843 major): the
// bytes the async audit keeps stay charged after the request's charge is
// released at chain return, until the audit's goroutine drops them.
func TestBodySpoolAsyncAuditHoldOutlivesRequest(t *testing.T) {
	gin.SetMode(gin.TestMode)
	body := largeResponsesTextBody(256 << 10)
	for _, path := range []string{"/v1/responses", "/v1/responses/compact"} {
		h := newLargeBodyTestHandler(t, &largeBodyDrainUpstream{}, "oauth")
		engine := &asyncHoldEngine{n: 100 << 10, done: make(chan struct{}), held: make(chan struct{})}
		h.securityAuditCoordinator = securityaudit.NewCoordinator(nil, engine)
		enableBodySpoolForTest(t, h.cfg, true, 4<<30, nil)
		meter := bodySpoolRuntimeFor(h.cfg).meter
		c, rec := newLargeBodyTestContext(path, body, "")
		h.Responses(c)
		releaseRequestBodySpool(c) // what BodySpoolIngest does at chain return
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		require.EqualValues(t, roundUpGranule(engine.n), meter.Outstanding(), "%s: the audit's bytes outlive the request", path)
		close(engine.done)
		<-engine.held
		require.Zero(t, meter.Outstanding(), path)
	}
}
