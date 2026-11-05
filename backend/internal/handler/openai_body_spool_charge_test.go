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

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/bodyspool"
	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
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
// = S) at the default k_skel (13) on 320m with a 45 MiB idle is charged
// 208 MiB once and
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
			require.EqualValues(t, int64(config.DefaultBodySpoolKSkel*16)<<20+bodyspool.ReadBufferBytes, meter.Outstanding(), "k_skel x 16 MiB plus the read buffer, held while the bytes are live")
			close(upstream.block)
			<-done
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			require.Zero(t, meter.Outstanding())
		})
	}
}

// TestOpenAIResponsesConcurrentTextBodies16MiB: while one 16 MiB text body
// holds its 224 MiB charge, others get 503 insufficient_memory (none is
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
	require.EqualValues(t, int64(config.DefaultBodySpoolKSkel*4)<<20+bodyspool.ReadBufferBytes, afterHandler, "still charged after the handler returned")
	require.Zero(t, meter.Outstanding())
}

// TestOpenAIResponsesFreshHopBaseIsWorkingSet: on a fresh hop memory.current
// is 85 MiB, 40 MiB of it inactive page cache. The 224 MiB charge fits
// against the working set (45 MiB). An unreadable memory.stat refuses the
// request explicitly before any byte reaches upstream.
func TestOpenAIResponsesFreshHopBaseIsWorkingSet(t *testing.T) {
	gin.SetMode(gin.TestMode)
	body := largeResponsesTextBody(16 << 20)
	upstream := &largeBodyDrainUpstream{}
	h := newLargeBodyTestHandler(t, upstream, "oauth")
	env := enableBodySpoolForTest(t, h.cfg, true, 320<<20, nil)
	require.NoError(t, os.WriteFile(filepath.Join(env.cgroup, "memory.current"), []byte(strconv.Itoa(85<<20)+"\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(env.cgroup, "memory.stat"), []byte("anon 30000000\ninactive_file "+strconv.Itoa(40<<20)+"\n"), 0o600))
	rt := bodySpoolRuntimeFor(h.cfg)
	require.NoError(t, rt.err)
	rt.meter.Reclaim = nil

	c, rec := newLargeBodyTestContext("/v1/responses", body, "")
	h.Responses(c)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.EqualValues(t, 1, upstream.calls.Load())

	require.NoError(t, os.WriteFile(filepath.Join(env.cgroup, "memory.stat"), nil, 0o600))
	c, rec = newLargeBodyTestContext("/v1/responses", body, "")
	h.Responses(c)
	requireCapacityError(t, rec, http.StatusInternalServerError, "body_spool_misconfigured", "sub2api")
	require.EqualValues(t, 1, upstream.calls.Load())
	require.Zero(t, rt.meter.Outstanding())
}

// repeatByte is an endless reader of one byte value.
type repeatByte byte

func (r repeatByte) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = byte(r)
	}
	return len(p), nil
}

// gatedReader signals started on its first Read and then blocks until
// release is closed.
type gatedReader struct {
	r                io.Reader
	started, release chan struct{}
	once             bool
}

func (g *gatedReader) Read(p []byte) (int, error) {
	if !g.once {
		g.once = true
		close(g.started)
		<-g.release
	}
	return g.r.Read(p)
}

// TestBodySpoolTinyRequestsBesideTicketed256MiB (task 4.2, review blocker 1):
// at 320m with a 45 MiB working set, one ticketed 256 MiB image request is
// admitted and holds its charge mid-body while 120 tiny requests ingest
// concurrently and hold theirs (as during streamed responses). None is
// refused for memory, and the ticketed body then completes.
func TestBodySpoolTinyRequestsBesideTicketed256MiB(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := newLargeBodyTestHandler(t, &largeBodyDrainUpstream{}, "oauth")
	env := enableBodySpoolForTest(t, h.cfg, true, 320<<20, nil)
	require.NoError(t, os.WriteFile(filepath.Join(env.cgroup, "memory.current"), []byte(strconv.Itoa(chargeTestIdle)+"\n"), 0o600))
	rt := bodySpoolRuntimeFor(h.cfg)
	require.NoError(t, rt.err)
	rt.meter.Reclaim = nil
	const budget = 320<<20 - chargeTestIdle - bodyspool.ChargeMargin

	const size = 256 << 20
	head := `{"model":"gpt-5.1","stream":true,"input":[{"type":"message","role":"user","content":[{"type":"input_image","image_url":"data:image/png;base64,`
	tail := `"}]}]}`
	gate := &gatedReader{
		r:       io.MultiReader(io.LimitReader(repeatByte('A'), int64(size-len(head)-len(tail))), bytes.NewBufferString(tail)),
		started: make(chan struct{}), release: make(chan struct{}),
	}
	big, bigRec := newLargeBodyTestContext("/v1/responses", nil, writeTestTicket(t, env.ledger, size))
	big.Request.Body = io.NopCloser(io.MultiReader(bytes.NewBufferString(head), gate))
	big.Request.ContentLength = size
	bigOK := make(chan bool, 1)
	go func() { bigOK <- ingestRequestBody(big, h.cfg) }()
	select {
	case <-gate.started:
	case ok := <-bigOK:
		t.Fatalf("ticketed request finished early (%v): %d %s", ok, bigRec.Code, bigRec.Body.String())
	}
	ticketed := rt.meter.Outstanding()
	require.Positive(t, ticketed)

	const tiny = 120
	type result struct {
		ok   bool
		code int
		body string
		c    *gin.Context
	}
	results := make(chan result, tiny)
	for i := 0; i < tiny; i++ {
		path := "/v1/responses"
		if i%3 == 2 {
			path = "/v1/chat/completions" // fallback path
		}
		go func() {
			c, rec := newLargeBodyTestContext(path, []byte(`{"model":"gpt-5.5","stream":true,"input":"hi"}`), "")
			if i%2 == 1 {
				chunked(c)
			}
			ok := ingestRequestBody(c, h.cfg)
			results <- result{ok, rec.Code, rec.Body.String(), c}
		}()
	}
	var held []*gin.Context
	for i := 0; i < tiny; i++ {
		r := <-results
		require.True(t, r.ok, "tiny request %d refused: %d %s", i, r.code, r.body)
		held = append(held, r.c)
	}
	require.LessOrEqual(t, rt.meter.Outstanding(), int64(budget))
	t.Logf("ticketed charge %d MiB; with %d tiny requests held: %d MiB", ticketed>>20, tiny, rt.meter.Outstanding()>>20)

	close(gate.release)
	require.True(t, <-bigOK, "ticketed request: %d %s", bigRec.Code, bigRec.Body.String())
	releaseRequestBodySpool(big)
	for _, c := range held {
		releaseRequestBodySpool(c)
	}
	require.Zero(t, rt.meter.Outstanding())
}

// panicReader yields n bytes of r and then panics.
type panicReader struct {
	r io.Reader
	n int
}

func (p *panicReader) Read(b []byte) (int, error) {
	if p.n <= 0 {
		panic("ingest panic")
	}
	if len(b) > p.n {
		b = b[:p.n]
	}
	n, err := p.r.Read(b)
	p.n -= n
	return n, err
}

// TestBodySpoolPanicDuringIngestReleases (review minor): a panic while the
// body is ingested still drops the charge, the ticket lease and the spool,
// through the middleware and through the handler's own ingest.
func TestBodySpoolPanicDuringIngestReleases(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, entry := range []string{"middleware", "handler"} {
		t.Run(entry, func(t *testing.T) {
			h := newLargeBodyTestHandler(t, &largeBodyDrainUpstream{}, "oauth")
			env := enableBodySpoolForTest(t, h.cfg, true, 4<<30, nil)
			ticket := writeTestTicket(t, env.ledger, 1<<20)
			meter := bodySpoolRuntimeFor(h.cfg).meter
			body := largeResponsesImagesBody(1 << 20)
			c, _ := newLargeBodyTestContext("/v1/responses", body, ticket)
			c.Request.Body = io.NopCloser(&panicReader{r: bytes.NewReader(body), n: 300 << 10})
			func() {
				defer func() {
					r := recover()
					if entry == "middleware" {
						require.Equal(t, "ingest panic", r)
					} // the Responses handler recovers its own panics
				}()
				if entry == "middleware" {
					BodySpoolIngest(h.cfg)(c)
				} else {
					h.Responses(c)
				}
			}()
			require.Zero(t, meter.Outstanding(), "charge released")
			v, ok := c.Get(bodySpoolStateKey)
			require.True(t, ok)
			st := v.(*bodySpoolState)
			require.True(t, st.released)
			require.NotNil(t, st.file)
			_, err := st.file.ReadAt(make([]byte, 1), 0)
			require.Error(t, err, "spool file closed")
			f, err := os.Open(filepath.Join(env.ledger, ticket+".ticket"))
			require.NoError(t, err)
			defer func() { _ = f.Close() }()
			require.NoError(t, unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB), "lease released")
		})
	}
}
