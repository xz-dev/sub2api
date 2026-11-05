package handler

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/bodyspool"
	pkghttputil "github.com/Wei-Shaw/sub2api/internal/pkg/httputil"
	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

// Large-request-capacity ingest (design D2-D4, task 7.2), behind
// gateway.body_spool.enabled. It runs before any stage reads the body: the
// BodySpoolIngest middleware in production, and again (as a no-op once
// done) from the Responses handler's body read.
//
//   - A ticket lease is taken before any byte is read; a body of 64 MiB or
//     more without a ticket, or without a ledger, gets 503
//     capacity_ticket_missing.
//   - Covered paths (bare /v1/responses, /responses, /backend-api/codex/
//     responses of an OpenAI group) spool tier-A media and hand on the
//     skeleton; inline bytes above S get 413.
//   - Every other POST is a fallback path: above L it gets a 413 naming the
//     path and L.
//   - The memory charge snapshots memory.current before any byte is read.
//     With a Content-Length, the whole worst case (k_skel x min(N, S), or
//     k_path x min(N, L), plus the ticketed k_mem x N + P_h) is charged once,
//     before reading. A chunked body is charged per 1 MiB as its inline (or
//     fallback) bytes cross each step, against the same snapshot. The charge
//     is released when the handler chain returns.

const bodySpoolHop = "sub2api"

type bodySpoolRuntime struct {
	cfg   config.GatewayBodySpoolConfig
	cap   int64
	dir   *bodyspool.Dir
	meter *bodyspool.Meter
	err   error
}

var bodySpoolRuntimes sync.Map // *config.Config -> *bodySpoolRuntime

func bodySpoolRuntimeFor(cfg *config.Config) *bodySpoolRuntime {
	if cfg == nil || !cfg.Gateway.BodySpool.Enabled {
		return nil
	}
	if v, ok := bodySpoolRuntimes.Load(cfg); ok {
		return v.(*bodySpoolRuntime)
	}
	rt := &bodySpoolRuntime{cfg: cfg.Gateway.BodySpool.Effective(), cap: gatewayMaxBodySize(cfg)}
	if rt.dir, rt.err = bodyspool.OpenDir(rt.cfg.Dir, rt.cfg.DiskReserve); rt.err == nil {
		rt.meter, rt.err = bodyspool.OpenMeter(rt.cfg.CgroupDir)
	}
	v, loaded := bodySpoolRuntimes.LoadOrStore(cfg, rt)
	if loaded && rt.meter != nil {
		rt.meter.Close()
	}
	return v.(*bodySpoolRuntime)
}

const bodySpoolStateKey = "large_request_body_spool_state"

// bodySpoolState is one request's spool, lease and charge.
type bodySpoolState struct {
	spool    *bodyspool.Spool
	lease    *bodyspool.Lease
	charge   *bodyspool.Charge
	released bool
}

// releaseRequestBodySpool closes the spool and drops the lease and the
// charge. Idempotent; the handler and the middleware both call it.
func releaseRequestBodySpool(c *gin.Context) {
	v, ok := c.Get(bodySpoolStateKey)
	if !ok {
		return
	}
	st := v.(*bodySpoolState)
	if st.released {
		return
	}
	st.released = true
	if st.spool != nil {
		_ = st.spool.Close()
	}
	st.lease.Release()
	st.charge.Release()
}

// BodySpoolIngest is the production entry: it must run before any
// middleware that reads the body.
func BodySpoolIngest(cfg *config.Config) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !ingestRequestBody(c, cfg) {
			c.Abort()
			return
		}
		defer releaseRequestBodySpool(c)
		c.Next()
	}
}

type capacityRefusal struct {
	status        int
	code, message string
}

func writeCapacityRefusal(c *gin.Context, r *capacityRefusal) {
	typ := "server_error"
	if r.status == http.StatusRequestEntityTooLarge {
		typ = "invalid_request_error"
	}
	service.MarkResponseCommitted(c)
	c.AbortWithStatusJSON(r.status, gin.H{"error": gin.H{"type": typ, "code": r.code, "hop": bodySpoolHop, "message": r.message}})
}

func tooLarge(format string, args ...any) *capacityRefusal {
	return &capacityRefusal{http.StatusRequestEntityTooLarge, "request_too_large", bodySpoolHop + ": " + fmt.Sprintf(format, args...)}
}

// refusalFor maps an ingest error to its explicit HTTP refusal.
func refusalFor(err error, inlineLimit int64) *capacityRefusal {
	var lim *bodyspool.InlineLimitError
	var mem *bodyspool.InsufficientMemoryError
	var tick *bodyspool.TicketError
	var maxErr *http.MaxBytesError
	switch {
	case errors.As(err, &lim):
		return tooLarge("inline bytes exceed the %d-byte inline-bytes limit (media is spooled, text is not)", inlineLimit)
	case errors.As(err, &mem):
		return &capacityRefusal{http.StatusServiceUnavailable, bodyspool.CodeInsufficientMemory, bodySpoolHop + ": " + mem.Error()}
	case errors.As(err, &tick):
		return &capacityRefusal{http.StatusServiceUnavailable, tick.Code, bodySpoolHop + ": " + tick.Error()}
	case errors.Is(err, bodyspool.ErrInsufficientStorage):
		return &capacityRefusal{http.StatusInsufficientStorage, "insufficient_storage", bodySpoolHop + ": " + err.Error()}
	case errors.Is(err, bodyspool.ErrMarkerCollision):
		return &capacityRefusal{http.StatusInternalServerError, "marker_collision", bodySpoolHop + ": " + err.Error()}
	case errors.As(err, &maxErr):
		return tooLarge("body exceeds the %d-byte request limit", maxErr.Limit)
	}
	return &capacityRefusal{http.StatusBadRequest, "invalid_request_error", "Failed to read request body"}
}

func isBodySpoolCoveredRequest(c *gin.Context) bool {
	if !isBareOpenAIResponsesPath(c) {
		return false
	}
	if enc := strings.ToLower(strings.TrimSpace(c.GetHeader("Content-Encoding"))); enc != "" && enc != "identity" {
		return false
	}
	apiKey, ok := middleware2.GetAPIKeyFromContext(c)
	return ok && apiKey != nil && apiKey.Group != nil && apiKey.Group.Platform == service.PlatformOpenAI
}

// readPrereadBody marks c's body as already read (fallback path).
func readPrereadBody(c *gin.Context) ([]byte, error) {
	body, err := pkghttputil.ReadRequestBodyWithPrealloc(c.Request)
	if err != nil {
		return nil, err
	}
	c.Request.Body = pkghttputil.NewPrereadBody(body)
	c.Request.ContentLength = int64(len(body))
	return body, nil
}

// ingestRequestBody prepares c's body. It returns false after writing an
// explicit refusal; the caller must stop.
func ingestRequestBody(c *gin.Context, cfg *config.Config) bool {
	rt := bodySpoolRuntimeFor(cfg)
	if rt == nil || c.Request == nil || c.Request.Method != http.MethodPost || c.Request.Body == nil {
		return true
	}
	if _, done := c.Get(bodySpoolStateKey); done {
		return true
	}
	if rt.err != nil {
		writeCapacityRefusal(c, &capacityRefusal{http.StatusInternalServerError, "body_spool_misconfigured", bodySpoolHop + ": " + rt.err.Error()})
		return false
	}
	charge, err := rt.meter.NewCharge() // before any byte of this request exists
	if err != nil {
		writeCapacityRefusal(c, &capacityRefusal{http.StatusInternalServerError, "body_spool_misconfigured", bodySpoolHop + ": " + err.Error()})
		return false
	}
	st := &bodySpoolState{charge: charge}
	c.Set(bodySpoolStateKey, st)
	fail := func(r *capacityRefusal) bool {
		releaseRequestBodySpool(c)
		writeCapacityRefusal(c, r)
		return false
	}

	// The ticket travels in the request context, never as a copied header.
	ticket := strings.TrimSpace(c.GetHeader(bodyspool.TicketHeader))
	c.Request.Header.Del(bodyspool.TicketHeader)
	size := c.Request.ContentLength // -1 when chunked
	var upfront int64
	if ticket != "" || size >= bodyspool.TicketThreshold {
		lease, err := bodyspool.AcquireLease(rt.cfg.LedgerDir, ticket) // before any byte is read
		if err != nil {
			return fail(refusalFor(err, 0))
		}
		st.lease = lease
		n := size
		if n < 0 {
			n = rt.cap // chunked: reserve the cap
		}
		upfront = int64(rt.cfg.KMem*float64(n)) + rt.cfg.PageCacheBound
	}
	covered := isBodySpoolCoveredRequest(c)
	limit, k := rt.cfg.FallbackLimit, rt.cfg.KPath
	if covered {
		limit, k = rt.cfg.InlineLimit, rt.cfg.KSkel
		if st.lease != nil {
			limit = rt.cfg.TicketedInlineLimit
		}
	}
	// prepaid: inline (or fallback) bytes already covered by the up-front
	// charge, rounded up to the step the readers charge in.
	var prepaid int64
	if size >= 0 && (covered || size <= limit) {
		prepaid = roundUpStep(min(size, limit))
		upfront += int64(k * float64(prepaid))
	}
	if err := st.charge.Add(upfront); err != nil {
		return fail(refusalFor(err, 0))
	}

	if !covered {
		if size > limit {
			return fail(tooLarge("%s is a fallback path; its limit is %d bytes and the body has %d", c.Request.URL.Path, limit, size))
		}
		c.Request.Body = &chargingReader{r: http.MaxBytesReader(c.Writer, c.Request.Body, limit), charge: st.charge, k: k, charged: prepaid}
		if _, err := readPrereadBody(c); err != nil {
			if errors.As(err, new(*http.MaxBytesError)) {
				return fail(tooLarge("%s is a fallback path; its limit is %d bytes", c.Request.URL.Path, limit))
			}
			return fail(refusalFor(err, 0))
		}
		return true
	}

	inline := limit
	var inlineSeen int64
	f, err := rt.dir.Create()
	if err != nil {
		return fail(&capacityRefusal{http.StatusInternalServerError, "body_spool_unavailable", bodySpoolHop + ": " + err.Error()})
	}
	sp, skel, err := bodyspool.Ingest(c.Request.Body, f, bodyspool.Options{
		Threshold:   rt.cfg.Threshold,
		KeepPrefix:  bodyspool.DefaultKeepPrefix,
		InlineLimit: inline,
		Charge: func(n int64) error {
			if inlineSeen += n; inlineSeen <= prepaid {
				return nil
			}
			return st.charge.Add(int64(k * float64(n)))
		},
		Observe: func(raw int64) error {
			if raw >= bodyspool.TicketThreshold && st.lease == nil {
				return &bodyspool.TicketError{Code: bodyspool.CodeTicketMissing, Reason: fmt.Sprintf("received %d bytes (>= %d) without a capacity ticket", raw, bodyspool.TicketThreshold)}
			}
			return nil
		},
	})
	if err != nil {
		_ = f.Close()
		return fail(refusalFor(err, inline))
	}
	st.spool = sp
	leased := ""
	if st.lease != nil {
		leased = st.lease.ID
	}
	c.Request.Body = pkghttputil.NewPrereadBody(skel)
	c.Request.ContentLength = int64(len(skel))
	c.Request = c.Request.WithContext(service.WithRequestBodySpool(c.Request.Context(), sp, leased))
	return true
}

func roundUpStep(n int64) int64 {
	const s = bodyspool.DefaultChargeStep
	return (n + s - 1) / s * s
}

// chargingReader charges k x every 1 MiB past charged before handing the
// bytes on.
type chargingReader struct {
	r       interface{ Read([]byte) (int, error) }
	charge  *bodyspool.Charge
	k       float64
	read    int64
	charged int64
}

func (r *chargingReader) Read(p []byte) (int, error) {
	if r.read+int64(len(p)) > r.charged {
		if err := r.charge.Add(int64(r.k * float64(bodyspool.DefaultChargeStep))); err != nil {
			return 0, err
		}
		r.charged += bodyspool.DefaultChargeStep
	}
	if max := r.charged - r.read; int64(len(p)) > max {
		p = p[:max]
	}
	n, err := r.r.Read(p)
	r.read += int64(n)
	return n, err
}

func (r *chargingReader) Close() error { return nil }
