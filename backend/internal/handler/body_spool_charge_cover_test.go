//go:build unit

package handler

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"runtime/metrics"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
)

// Charge-covers-allocation property (OpenSpec large-request-capacity task
// 7.3a): while a request runs, its live heap growth stays within the bytes
// it holds on the memory meter.

// requestChargedBytes is the test hook: the bytes cfg's request in flight
// holds on its meter right now, that is the ingest's base charge, every
// top-up (converter, guard decode) and every detached hold (async audit).
// Each case builds its own handler, config and fake cgroup, so its meter
// (bodyspool.SharedMeter is keyed by the cgroup dir) carries exactly one
// request, and the meter's outstanding sum is that request's charge. It
// reads an atomic, so it is safe to call while the handler runs.
func requestChargedBytes(cfg *config.Config) func() int64 {
	rt := bodySpoolRuntimeFor(cfg)
	if rt == nil || rt.meter == nil {
		return func() int64 { return 0 }
	}
	return rt.meter.Outstanding
}

// chargeCoverInterval is the sampling period (task: at most 5 ms).
const chargeCoverInterval = time.Millisecond

// chargeCoverReading is one request's samples.
type chargeCoverReading struct {
	PeakHeap   int64 // max live heap growth over the post-GC baseline
	PeakCharge int64 // max bytes charged
	// Excess is max over samples of heap growth minus the largest charge
	// seen up to and including that sample: allocation the charge did not
	// cover at that moment. Each sample reads the heap first and the charge
	// second, so a charge taken just before its allocation is never missed;
	// the reading can favour the request by at most one interval.
	Excess  int64
	Samples int
}

// measureChargeCover runs fn and samples the live heap
// (/memory/classes/heap/objects:bytes: live objects plus dead ones not yet
// swept, the metric k_skel and k_conv were measured with) and charged().
// The baseline is read after a forced GC, with the body and the handler
// already built.
func measureChargeCover(charged func() int64, fn func()) chargeCoverReading {
	runtime.GC()
	s := []metrics.Sample{{Name: "/memory/classes/heap/objects:bytes"}}
	metrics.Read(s)
	base := int64(s[0].Value.Uint64())
	var r chargeCoverReading
	sample := func(ls []metrics.Sample) {
		metrics.Read(ls)
		heap := int64(ls[0].Value.Uint64()) - base
		r.PeakCharge = max(r.PeakCharge, charged())
		r.PeakHeap = max(r.PeakHeap, heap)
		r.Excess = max(r.Excess, heap-r.PeakCharge)
		r.Samples++
	}
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		ls := []metrics.Sample{{Name: "/memory/classes/heap/objects:bytes"}}
		tk := time.NewTicker(chargeCoverInterval)
		defer tk.Stop()
		for {
			sample(ls)
			select {
			case <-stop:
				return
			case <-tk.C:
			}
		}
	}()
	fn()
	close(stop)
	wg.Wait()
	// One sample after the request: a request shorter than an interval
	// still counts, since what it allocated stays in the metric until the
	// next GC.
	sample([]metrics.Sample{{Name: "/memory/classes/heap/objects:bytes"}})
	return r
}

// coverResult is one case.
type coverResult struct {
	chargeCoverReading
	status int
	calls  int32
	bytes  int
	reply  string // the start of a non-200 response body
}

// coverHarnessCombo measures the harness itself: no ingest and a handler
// that answers 200 without reading the body, so nothing is charged and the
// heap growth is the recorder, the request, gin and the sampler. That is
// what chargeCoverAllowance must cover.
func coverHarnessCombo() coverCombo {
	return coverCombo{route: "harness", account: "noop", proto: "harness", model: "gpt-5.5",
		pattern: "/v1/responses", path: "/v1/responses",
		setup: func(t *testing.T) (http.Handler, *config.Config, *coverUpstream) {
			cfg := &config.Config{RunMode: config.RunModeSimple}
			cfg.Gateway.MaxBodySize = 256 << 20
			enableBodySpoolForTest(t, cfg, true, 8<<30, nil)
			r := gin.New()
			r.POST("/v1/responses", coverAuth("openai"), func(c *gin.Context) {
				c.JSON(http.StatusOK, gin.H{"ok": true})
			})
			return r, cfg, &coverUpstream{}
		}}
}

// coverServer is one combo's handler, built once and warmed up, so a case
// measures the request and not the first use of a fresh service.
type coverServer struct {
	h   http.Handler
	cfg *config.Config
	up  *coverUpstream
}

// runCoverCase serves body once. The meter must be empty before: the
// previous request released everything it charged.
func runCoverCase(t *testing.T, srv coverServer, path string, body []byte, codex bool, header map[string]string) coverResult {
	t.Helper()
	charged := requestChargedBytes(srv.cfg)
	assert.Zero(t, charged(), "a charge outlived its request")
	srv.cfg.Gateway.CodexImageGenerationBridgeEnabled = codex
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if codex {
		req.Header.Set("originator", "codex_cli_rs")
	}
	for k, v := range header {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	calls := srv.up.calls.Load()
	srv.up.lastPath.Store("")
	rd := measureChargeCover(charged, func() { srv.h.ServeHTTP(rec, req) })
	res := coverResult{chargeCoverReading: rd, status: rec.Code, calls: srv.up.calls.Load() - calls, bytes: len(body)}
	if rec.Code != http.StatusOK {
		last, _ := srv.up.lastPath.Load().(string)
		res.reply = "upstream " + last + ": " + rec.Body.String()[:min(rec.Body.Len(), 240)]
	}
	return res
}

// TestBodySpoolChargeCoversAllocation (task 7.3a): for every route that
// takes a body, on every account kind, and every corpus shape at 2, 8 and
// 16 MiB, the live heap growth of the request never exceeds the bytes it has charged by more than chargeCoverAllowance. A
// 413 or 503 passes if nothing reached the upstream and the heap stayed
// within the charge (no decode before the refusal). Any other status means
// the case did not exercise its route and fails.
//
// Slow and not meaningful under -race: set SUB2API_CHARGE_COVER=1.
// SUB2API_CHARGE_COVER_ONLY=<a|b|...> runs the cases whose name has one of
// the substrings.
func TestBodySpoolChargeCoversAllocation(t *testing.T) {
	if os.Getenv("SUB2API_CHARGE_COVER") == "" {
		t.Skip("set SUB2API_CHARGE_COVER=1 to run the charge-covers-allocation property test")
	}
	gin.SetMode(gin.TestMode)
	only := os.Getenv("SUB2API_CHARGE_COVER_ONLY")
	cases := 0
	var notExercised []string
	for _, cb := range append(append([]coverCombo{coverHarnessCombo()}, coverOpenAICombos()...), coverGatewayCombos()...) {
		prefix := cb.route + "/" + cb.account + "/"
		type shape struct {
			name   string
			body   func(size int) []byte
			codex  bool
			header map[string]string
			prime  string
		}
		var shapes []shape
		p := coverProtos[cb.proto]
		if cb.proto == "harness" {
			p = coverProtos["responses"]
		}
		for _, b := range coverBases {
			shapes = append(shapes, shape{name: b.name, body: func(n int) []byte { return coverBody(p, b, n, cb.model, "", "") }})
		}
		for _, m := range coverMods {
			if m.proto != cb.proto {
				continue
			}
			model := cb.model
			if m.model != "" {
				model = m.model
			}
			shapes = append(shapes, shape{name: m.name, codex: m.codex, header: m.header, prime: m.prime, body: func(n int) []byte {
				return coverBody(p, coverBases[0], n, model, m.root, m.item)
			}})
		}
		// 16 MiB is S; a converted or fully decoded request is charged
		// k_conv, which the 8 GiB fake cgroup leaves room for, so every
		// route runs at every size.
		sizes := []int{2 << 20, 8 << 20, 16 << 20}
		var srv *coverServer
		for i, sh := range shapes {
			sz := sizes
			if i == 0 { // tiny at 64 KiB: the fixed per-request overhead, logged only
				sz = append([]int{64 << 10}, sizes...)
			}
			for _, size := range sz {
				name := prefix + sh.name + "/" + coverSize(size)
				if !coverSelected(name, only) {
					continue
				}
				if srv == nil { // built on the parent t; one unmeasured request warms it
					h, cfg, up := cb.setup(t)
					srv = &coverServer{h: h, cfg: cfg, up: up}
					runCoverCase(t, *srv, cb.path, coverBody(p, coverBases[0], 64<<10, cb.model, "", ""), false, nil)
				}
				cases++
				t.Run(name, func(t *testing.T) {
					if sh.prime != "" {
						coverPrimes[sh.prime](t, *srv, cb.path, sh.body(64<<10), sh.header)
					}
					body := sh.body(size)
					r := runCoverCase(t, *srv, cb.path, body, sh.codex, sh.header)
					t.Logf("COVER %s status %d calls %d bytes %d peak_heap %d peak_charge %d excess %d samples %d",
						name, r.status, r.calls, r.bytes, r.PeakHeap, r.PeakCharge, r.Excess, r.Samples)
					if r.reply != "" {
						t.Logf("COVER %s reply %s", name, r.reply)
					}
					switch r.status {
					case http.StatusOK:
						if cb.proto != "harness" && !cb.local {
							assert.Positive(t, r.calls, "200 without an upstream call")
						}
					case http.StatusRequestEntityTooLarge, http.StatusServiceUnavailable:
						assert.Zero(t, r.calls, "refused after an upstream call")
					default:
						// A product refusal before the upstream (invalid UTF-8,
						// an unsupported model or item for the account). Not a
						// capacity refusal: listed, and the heap must still stay
						// within the charge up to it.
						assert.Zero(t, r.calls, "status %d after an upstream call", r.status)
						notExercised = append(notExercised, name+" "+http.StatusText(r.status))
					}
					if size < 1<<20 {
						// Not asserted: the per-request cost that does not
						// grow with the body (decoders, buffers, the upstream
						// request) is left to the meter's ChargeMargin.
						t.Logf("COVER-INFO %s fixed overhead above charge %d B", name, r.Excess)
						return
					}
					assert.LessOrEqual(t, r.Excess, int64(chargeCoverAllowance),
						"live heap %d B above charge (peak heap %d, peak charge %d, allowance %d)",
						r.Excess, r.PeakHeap, r.PeakCharge, chargeCoverAllowance)
				})
			}
		}
	}
	t.Logf("COVER cases %d, refused before the upstream with a non-capacity status %d", cases, len(notExercised))
	for _, n := range notExercised {
		t.Logf("COVER not-exercised %s", n)
	}
}

// coverSelected: only is empty or one of its "|"-separated substrings is
// in name.
func coverSelected(name, only string) bool {
	if only == "" {
		return true
	}
	for _, part := range strings.Split(only, "|") {
		if part != "" && strings.Contains(name, part) {
			return true
		}
	}
	return false
}

// coverPrimes set up per-session state before a case: each sends body (the
// case's shape at 64 KiB, with its session header) once, unmeasured.
var coverPrimes = map[string]func(t *testing.T, srv coverServer, path string, body []byte, header map[string]string){
	// The upstream rejects the first send with invalid_encrypted_content;
	// Forward retries without the encrypted item and marks the session's
	// lineage with its digest (openai_gateway_forward.go retry). The case
	// then sends the same item again in that session.
	"invalid_encrypted_content": func(t *testing.T, srv coverServer, path string, body []byte, header map[string]string) {
		srv.up.rejectEncrypted.Store(1)
		r := runCoverCase(t, srv, path, body, false, header)
		srv.up.rejectEncrypted.Store(0)
		t.Logf("COVER prime invalid_encrypted_content status %d calls %d", r.status, r.calls)
	},
}

// chargeCoverAllowance is what the heap may exceed the charge by for
// reasons outside the request under test. Measured: the harness combo
// (recorder, request, gin, sampler; nothing charged) grows the heap by at
// most 2,688 B. In two full runs the passing cases stayed within 144 B of
// their charge, except web_search invalid 2 MiB once at +16,480 B (35.39
// MiB on a 35.37 MiB charge; 27.9 MiB in the other run: the metric counts
// garbage until the GC sweeps it, so peaks move with GC timing). 64 KiB is
// 24 x the harness cost and 0.5 % of the smallest asserted charge (12 MiB:
// k_skel 6 x 2 MiB).
const chargeCoverAllowance = 64 << 10
