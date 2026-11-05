//go:build unit

package handler

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// Memory charge without double counting (design D4, task 7.2 follow-up).
// The fake cgroup's memory.current follows idle + outstanding charges, the way
// charged bytes become live in a real cgroup.

const chargeTestIdle = 45 << 20

// trackCgroup keeps memory.current = idle + outstanding until stop is closed.
func trackCgroup(t *testing.T, h *OpenAIGatewayHandler, env spoolTestEnv) (stop func()) {
	t.Helper()
	rt := bodySpoolRuntimeFor(h.cfg)
	require.NoError(t, rt.err)
	write := func() {
		v := strconv.FormatInt(chargeTestIdle+rt.meter.Outstanding(), 10)
		_ = os.WriteFile(filepath.Join(env.cgroup, "memory.current"), []byte(v+"\n"), 0o600)
	}
	write()
	done, exited := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(exited)
		tk := time.NewTicker(time.Millisecond)
		defer tk.Stop()
		for {
			select {
			case <-done:
				return
			case <-tk.C:
				write()
			}
		}
	}()
	return func() { close(done); <-exited }
}

// chunked hides the Content-Length, as a chunked upload does.
func chunked(c *gin.Context) {
	c.Request.ContentLength = -1
	c.Request.Body = io.NopCloser(io.MultiReader(c.Request.Body))
}

// TestOpenAIResponsesSingleTextBody16MiBAt320m: one 16 MiB text body (inline
// = S) at k_skel 15 on 320m with a 45 MiB idle is charged 240 MiB once and
// passes; the charge is held while the request runs and dropped after.
func TestOpenAIResponsesSingleTextBody16MiBAt320m(t *testing.T) {
	gin.SetMode(gin.TestMode)
	body := largeResponsesTextBody(16 << 20)
	for _, mode := range []string{"content_length", "chunked"} {
		t.Run(mode, func(t *testing.T) {
			upstream := &largeBodyDrainUpstream{block: make(chan struct{}), entered: make(chan struct{})}
			h := newLargeBodyTestHandler(t, upstream, "oauth")
			env := enableBodySpoolForTest(t, h.cfg, true, 320<<20, nil)
			stop := trackCgroup(t, h, env)
			defer stop()
			meter := bodySpoolRuntimeFor(h.cfg).meter

			c, rec := newLargeBodyTestContext("/v1/responses", body, "")
			if mode == "chunked" {
				chunked(c)
			}
			done := make(chan struct{})
			go func() { defer close(done); h.Responses(c) }()
			select {
			case <-upstream.entered:
			case <-done:
				t.Fatalf("finished before upstream: %d %s", rec.Code, rec.Body.String())
			}
			require.EqualValues(t, 240<<20, meter.Outstanding(), "15 x 16 MiB, held while the bytes are live")
			close(upstream.block)
			<-done
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			require.Zero(t, meter.Outstanding())
		})
	}
}

// TestOpenAIResponsesConcurrentTextBodies16MiB: while one 16 MiB text body
// holds its 240 MiB charge, others get 503 insufficient_memory (none is
// admitted past memory.max - 32 MiB); after it returns, the next one passes.
func TestOpenAIResponsesConcurrentTextBodies16MiB(t *testing.T) {
	gin.SetMode(gin.TestMode)
	body := largeResponsesTextBody(16 << 20)
	upstream := &largeBodyDrainUpstream{block: make(chan struct{}), entered: make(chan struct{})}
	h := newLargeBodyTestHandler(t, upstream, "oauth")
	env := enableBodySpoolForTest(t, h.cfg, true, 320<<20, nil)
	stop := trackCgroup(t, h, env)
	defer stop()

	c1, rec1 := newLargeBodyTestContext("/v1/responses", body, "")
	done := make(chan struct{})
	go func() { defer close(done); h.Responses(c1) }()
	<-upstream.entered
	for _, mode := range []string{"content_length", "chunked"} {
		c, rec := newLargeBodyTestContext("/v1/responses", body, "")
		if mode == "chunked" {
			chunked(c)
		}
		h.Responses(c)
		requireCapacityError(t, rec, http.StatusServiceUnavailable, "insufficient_memory", "sub2api")
	}
	close(upstream.block)
	<-done
	require.Equal(t, http.StatusOK, rec1.Code, rec1.Body.String())
	// The released bytes leave the fake cgroup on the next tracker tick.
	require.Eventually(t, func() bool {
		b, _ := os.ReadFile(filepath.Join(env.cgroup, "memory.current"))
		return string(b) == strconv.Itoa(chargeTestIdle)+"\n"
	}, time.Second, time.Millisecond)

	c3, rec3 := newLargeBodyTestContext("/v1/responses", body, "")
	h.Responses(c3)
	require.Equal(t, http.StatusOK, rec3.Code, rec3.Body.String())
	require.EqualValues(t, 2, upstream.calls.Load())
	require.Zero(t, bodySpoolRuntimeFor(h.cfg).meter.Outstanding())
}

// TestBodySpoolChargeHeldUntilChainReturns: with the production middleware,
// the handler's own return does not drop the charge; outer middleware still
// runs with the request's bytes, and only the middleware releases.
func TestBodySpoolChargeHeldUntilChainReturns(t *testing.T) {
	gin.SetMode(gin.TestMode)
	upstream := &largeBodyDrainUpstream{}
	h := newLargeBodyTestHandler(t, upstream, "oauth")
	enableBodySpoolForTest(t, h.cfg, true, 320<<20, nil)
	meter := bodySpoolRuntimeFor(h.cfg).meter
	var afterHandler int64 = -1
	groupID := int64(3131)
	r := gin.New()
	r.POST("/v1/responses", func(c *gin.Context) {
		c.Set(string(middleware2.ContextKeyAPIKey), &service.APIKey{
			ID: 99, GroupID: &groupID,
			Group: &service.Group{ID: groupID, Platform: service.PlatformOpenAI},
			User:  &service.User{ID: 100},
		})
		c.Set(string(middleware2.ContextKeyUser), middleware2.AuthSubject{UserID: 100})
	}, BodySpoolIngest(h.cfg), func(c *gin.Context) {
		c.Next()
		afterHandler = meter.Outstanding()
	}, h.Responses)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(largeResponsesTextBody(4<<20)))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.EqualValues(t, 15*4<<20, afterHandler, "still charged after the handler returned")
	require.Zero(t, meter.Outstanding())
}
