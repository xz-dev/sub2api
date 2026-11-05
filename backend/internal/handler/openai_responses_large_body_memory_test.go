//go:build unit

package handler

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"runtime/metrics"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/spf13/viper"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// Large-request-capacity red tests (task 7.1, design D2/D4). The spool is
// enabled through viper keys so these tests compile on ab6b00e, where the
// unknown gateway.body_spool.* keys are ignored and the assertions fail.

const capacityTicketHeader = "X-AI-Gateway-Capacity-Ticket"

// largeBodyDrainUpstream drains the forwarded request body and answers with a
// minimal non-stream Responses payload carrying usage. block, when set, holds
// the first call until it is closed.
type largeBodyDrainUpstream struct {
	service.HTTPUpstream
	forwarded atomic.Int64
	calls     atomic.Int32
	headers   chan http.Header
	block     chan struct{}
	entered   chan struct{}
}

func (u *largeBodyDrainUpstream) Do(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	if u.calls.Add(1) == 1 && u.block != nil {
		close(u.entered)
		<-u.block
	}
	if u.headers != nil {
		select {
		case u.headers <- req.Header.Clone():
		default:
		}
	}
	if req.Body != nil {
		n, _ := io.Copy(io.Discard, req.Body)
		u.forwarded.Add(n)
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body: io.NopCloser(bytes.NewBufferString(`{"id":"resp_1","object":"response","status":"completed","model":"gpt-5.1",` +
			`"output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}],` +
			`"usage":{"input_tokens":10,"output_tokens":1,"total_tokens":11}}`)),
	}, nil
}

// largeResponsesTextBody is one huge input_text: inline bytes, never spooled.
func largeResponsesTextBody(size int) []byte {
	prefix := []byte(`{"model":"gpt-5.1","stream":false,"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"`)
	suffix := []byte(`"}]}]}`)
	b := make([]byte, 0, size)
	b = append(b, prefix...)
	b = append(b, bytes.Repeat([]byte("a"), size-len(prefix)-len(suffix))...)
	return append(b, suffix...)
}

// largeResponsesImagesBody builds data-URL images of up to 1 MiB (the common
// shape of real large Codex/Responses bodies), exactly size bytes long; the
// inline (non-media) part stays a few KiB at every size.
func largeResponsesImagesBody(size int) []byte {
	b := bytes.NewBufferString(`{"model":"gpt-5.1","stream":false,"input":[`)
	const tail = `]}`
	for i := 0; ; i++ {
		head := `{"type":"message","role":"user","content":[{"type":"input_text","text":"look ` + strconv.Itoa(i) + `"},{"type":"input_image","image_url":"data:image/png;base64,`
		if i > 0 {
			head = "," + head
		}
		const end = `"}]}`
		room := size - b.Len() - len(head) - len(end) - len(tail)
		if room <= 0 {
			// Too little room for another image: pad the last image instead.
			out := b.Bytes()
			pad := size - len(out) - len(tail)
			cut := len(out) - len(end)
			out = append(out[:cut:cut], append(bytes.Repeat([]byte("A"), pad), []byte(end+tail)...)...)
			return out
		}
		b.WriteString(head)
		b.Write(bytes.Repeat([]byte("A"), min(room, 1<<20)))
		b.WriteString(end)
		if room <= 1<<20 {
			b.WriteString(tail)
			return b.Bytes()
		}
	}
}

// spoolTestEnv enables gateway.body_spool for cfg: spool on a temp disk dir,
// a fake cgroup (memory.max/memory.current) and optionally a ledger dir.
type spoolTestEnv struct {
	ledger string
	cgroup string
}

func enableBodySpoolForTest(t *testing.T, cfg *config.Config, withLedger bool, cgroupMax int64, extra map[string]any) spoolTestEnv {
	t.Helper()
	env := spoolTestEnv{cgroup: t.TempDir()}
	require.NoError(t, os.WriteFile(filepath.Join(env.cgroup, "memory.max"), []byte(strconv.FormatInt(cgroupMax, 10)+"\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(env.cgroup, "memory.current"), []byte("0\n"), 0o600))
	spoolDir := filepath.Join(t.TempDir(), "spool")
	require.NoError(t, os.Mkdir(spoolDir, 0o700))
	v := viper.New()
	v.Set("gateway.body_spool.enabled", true)
	v.Set("gateway.body_spool.dir", spoolDir)
	v.Set("gateway.body_spool.cgroup_dir", env.cgroup)
	if withLedger {
		env.ledger = t.TempDir()
		v.Set("gateway.body_spool.ledger_dir", env.ledger)
	}
	for k, val := range extra {
		v.Set("gateway.body_spool."+k, val)
	}
	require.NoError(t, v.Unmarshal(cfg))
	return env
}

// writeTestTicket links a ticket the way keeper does: <128-bit hex>.ticket.
func writeTestTicket(t *testing.T, ledger string, n int) string {
	t.Helper()
	var raw [16]byte
	_, err := rand.Read(raw[:])
	require.NoError(t, err)
	id := hex.EncodeToString(raw[:])
	body := `{"id":"` + id + `","n":` + strconv.Itoa(n) + `}` + "\n"
	require.NoError(t, os.WriteFile(filepath.Join(ledger, id+".ticket"), []byte(body), 0o640))
	return id
}

func newLargeBodyTestHandler(t *testing.T, upstream service.HTTPUpstream, accountType string) *OpenAIGatewayHandler {
	t.Helper()
	accounts := make([]service.Account, 2)
	for i := range accounts {
		accounts[i] = service.Account{
			ID:          int64(i + 1),
			Name:        "responses-account-" + strconv.Itoa(i+1),
			Platform:    service.PlatformOpenAI,
			Type:        service.AccountTypeOAuth,
			Status:      service.StatusActive,
			Schedulable: true,
			Priority:    i,
			Credentials: map[string]any{"access_token": "token-" + strconv.Itoa(i+1)},
		}
		if accountType != "oauth" {
			accounts[i].Type = service.AccountTypeAPIKey
			accounts[i].Credentials = map[string]any{"api_key": "sk-test", "base_url": "https://api.openai.com"}
			accounts[i].Extra = map[string]any{"openai_passthrough": accountType == "apikey_passthrough"}
		}
	}
	cfg := &config.Config{RunMode: config.RunModeSimple}
	cfg.Gateway.MaxBodySize = 256 << 20
	gatewayService := service.NewOpenAIGatewayService(
		openAIImagesFailoverAccountRepo{accounts: accounts},
		nil, nil, nil, nil, nil, nil, cfg, nil, nil, nil, nil, nil,
		upstream,
		nil, nil, nil, nil, nil, nil, nil, nil,
	)
	billingService := service.NewBillingCacheService(nil, nil, nil, nil, nil, nil, cfg, nil)
	t.Cleanup(billingService.Stop)
	handler := NewOpenAIGatewayHandler(
		gatewayService,
		service.NewConcurrencyService(nil),
		billingService,
		service.NewAPIKeyService(nil, nil, nil, nil, nil, nil, cfg),
		nil, nil, nil, nil,
		cfg,
	)
	handler.maxAccountSwitches = 10
	return handler
}

// newLargeBodyTestContext builds an authenticated gin context for path.
func newLargeBodyTestContext(path string, body []byte, ticket string) (*gin.Context, *httptest.ResponseRecorder) {
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if ticket != "" {
		req.Header.Set(capacityTicketHeader, ticket)
	}
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = req.WithContext(context.Background())
	groupID := int64(3131)
	c.Set(string(middleware2.ContextKeyAPIKey), &service.APIKey{
		ID: 99, GroupID: &groupID,
		Group: &service.Group{ID: groupID, Platform: service.PlatformOpenAI},
		User:  &service.User{ID: 100},
	})
	c.Set(string(middleware2.ContextKeyUser), middleware2.AuthSubject{UserID: 100, Concurrency: 0})
	return c, rec
}

func heapObjectsBytes(s []metrics.Sample) uint64 {
	metrics.Read(s)
	return s[0].Value.Uint64()
}

// measurePeakHeap runs fn and returns the peak live-heap growth over the
// pre-call baseline, sampled every millisecond.
func measurePeakHeap(fn func()) uint64 {
	runtime.GC()
	s := []metrics.Sample{{Name: "/memory/classes/heap/objects:bytes"}}
	base := heapObjectsBytes(s)
	var peak atomic.Uint64
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		ls := []metrics.Sample{{Name: "/memory/classes/heap/objects:bytes"}}
		tk := time.NewTicker(time.Millisecond)
		defer tk.Stop()
		for {
			if v := heapObjectsBytes(ls); v > peak.Load() {
				peak.Store(v)
			}
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
	if p := peak.Load(); p > base {
		return p - base
	}
	return 0
}

func requireCapacityError(t *testing.T, rec *httptest.ResponseRecorder, status int, code string, contains ...string) {
	t.Helper()
	require.Equal(t, status, rec.Code, rec.Body.String())
	require.Equal(t, code, gjson.Get(rec.Body.String(), "error.code").String(), rec.Body.String())
	require.Equal(t, "sub2api", gjson.Get(rec.Body.String(), "error.hop").String(), rec.Body.String())
	for _, s := range contains {
		require.Contains(t, gjson.Get(rec.Body.String(), "error.message").String(), s)
	}
}

// TestOpenAIResponsesLargeBodyPeakHeap: a ticketed media-dominated body on the
// OAuth and API-key paths keeps live heap flat (< 32 MiB over baseline).
// SUB2API_LARGE_BODY_MIB overrides the body size (default 64).
func TestOpenAIResponsesLargeBodyPeakHeap(t *testing.T) {
	gin.SetMode(gin.TestMode)
	mib := 64
	if v, err := strconv.Atoi(os.Getenv("SUB2API_LARGE_BODY_MIB")); err == nil && v > 0 {
		mib = v
	}
	size := mib << 20
	body := largeResponsesImagesBody(size)
	require.Len(t, body, size)
	for _, accountType := range []string{"oauth", "apikey"} {
		t.Run(accountType, func(t *testing.T) {
			upstream := &largeBodyDrainUpstream{}
			h := newLargeBodyTestHandler(t, upstream, accountType)
			env := enableBodySpoolForTest(t, h.cfg, true, 8<<30, nil)
			c, rec := newLargeBodyTestContext("/v1/responses", body, writeTestTicket(t, env.ledger, size))
			delta := measurePeakHeap(func() { h.Responses(c) })
			t.Logf("body=%dMiB status=%d forwarded=%dMiB peakHeapDelta=%.1fMiB (%.3fx)",
				mib, rec.Code, upstream.forwarded.Load()>>20, float64(delta)/(1<<20), float64(delta)/float64(size))
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			require.GreaterOrEqual(t, upstream.forwarded.Load(), int64(size-(3<<20)))
			require.LessOrEqual(t, delta, uint64(32<<20), "peak heap delta must stay flat")
		})
	}
}

// TestOpenAIResponsesTextHeavyInlineLimit: 32 MiB of inline text is refused
// by name (413 request_too_large, 16 MiB inline-bytes limit), never carried.
func TestOpenAIResponsesTextHeavyInlineLimit(t *testing.T) {
	gin.SetMode(gin.TestMode)
	upstream := &largeBodyDrainUpstream{}
	h := newLargeBodyTestHandler(t, upstream, "oauth")
	enableBodySpoolForTest(t, h.cfg, true, 8<<30, nil)
	c, rec := newLargeBodyTestContext("/v1/responses", largeResponsesTextBody(32<<20), "")
	h.Responses(c)
	requireCapacityError(t, rec, http.StatusRequestEntityTooLarge, "request_too_large", "16777216", "inline")
	require.Zero(t, upstream.calls.Load())
}

// TestOpenAIResponsesTicketMissing: >= 64 MiB without a ticket, or with no
// ledger configured, gets 503 capacity_ticket_missing and nothing upstream.
func TestOpenAIResponsesTicketMissing(t *testing.T) {
	gin.SetMode(gin.TestMode)
	body := largeResponsesImagesBody(64 << 20)
	for _, tc := range []struct {
		name       string
		withLedger bool
		ticket     string
	}{
		{"no_ticket", true, ""},
		{"no_ledger", false, "00112233445566778899aabbccddeeff"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			upstream := &largeBodyDrainUpstream{}
			h := newLargeBodyTestHandler(t, upstream, "oauth")
			enableBodySpoolForTest(t, h.cfg, tc.withLedger, 8<<30, nil)
			c, rec := newLargeBodyTestContext("/v1/responses", body, tc.ticket)
			h.Responses(c)
			requireCapacityError(t, rec, http.StatusServiceUnavailable, "capacity_ticket_missing")
			require.Zero(t, upstream.calls.Load())
		})
	}
}

// TestOpenAIResponsesCompactFallbackLimit: /responses/compact is a fallback
// path; a body above L = 16 MiB gets a 413 naming the path and the limit.
func TestOpenAIResponsesCompactFallbackLimit(t *testing.T) {
	gin.SetMode(gin.TestMode)
	upstream := &largeBodyDrainUpstream{}
	h := newLargeBodyTestHandler(t, upstream, "oauth")
	enableBodySpoolForTest(t, h.cfg, true, 8<<30, nil)
	c, rec := newLargeBodyTestContext("/v1/responses/compact", largeResponsesImagesBody(17<<20), "")
	h.Responses(c)
	requireCapacityError(t, rec, http.StatusRequestEntityTooLarge, "request_too_large", "16777216", "/v1/responses/compact")
	require.Zero(t, upstream.calls.Load())
}

// TestOpenAIResponsesConcurrentMemoryCharge: while one text body holds its
// charge, a second one that would cross memory.max - 32 MiB gets 503
// insufficient_memory instead of being admitted.
func TestOpenAIResponsesConcurrentMemoryCharge(t *testing.T) {
	gin.SetMode(gin.TestMode)
	upstream := &largeBodyDrainUpstream{block: make(chan struct{}), entered: make(chan struct{})}
	h := newLargeBodyTestHandler(t, upstream, "oauth")
	// k_skel 4 x 6 MiB = 24 MiB per request; 64 MiB - 32 MiB margin fits one.
	enableBodySpoolForTest(t, h.cfg, true, 64<<20, map[string]any{"k_skel": 4})
	body := largeResponsesTextBody(6 << 20)

	c1, rec1 := newLargeBodyTestContext("/v1/responses", body, "")
	done := make(chan struct{})
	go func() { defer close(done); h.Responses(c1) }()
	select {
	case <-upstream.entered:
	case <-done:
		t.Fatalf("first request finished early: %d %s", rec1.Code, rec1.Body.String())
	}

	c2, rec2 := newLargeBodyTestContext("/v1/responses", body, "")
	h.Responses(c2)
	close(upstream.block)
	<-done
	require.Equal(t, http.StatusOK, rec1.Code, rec1.Body.String())
	requireCapacityError(t, rec2, http.StatusServiceUnavailable, "insufficient_memory", "sub2api")
	require.Equal(t, int32(1), upstream.calls.Load())
}
