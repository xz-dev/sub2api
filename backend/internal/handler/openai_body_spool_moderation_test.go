//go:build unit

package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/bodyspool"
	"github.com/Wei-Shaw/sub2api/internal/securityaudit"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// Moderation and the security audit on spooled bodies (review major 3).

var markerNoncePattern = regexp.MustCompile(`lrc[0-9a-f]{32}`)

// blockWordScanner is the guard upstream: it blocks a chunk that holds the
// word and records every chunk it is sent.
type blockWordScanner struct {
	mu     sync.Mutex
	chunks []string
}

func (s *blockWordScanner) Scan(_ context.Context, endpoint securityaudit.ActiveEndpoint, chunk string, _ []string) (*securityaudit.NormalizedResult, error) {
	s.mu.Lock()
	s.chunks = append(s.chunks, chunk)
	s.mu.Unlock()
	res := &securityaudit.NormalizedResult{Decision: securityaudit.EventPass, RiskLevel: securityaudit.RiskLow, Action: securityaudit.ActionAllow,
		Safety: "Safe", ScannerScores: map[string]float64{}, ScannerEvidence: map[string]string{}, GuardEndpointID: endpoint.ID}
	if strings.Contains(chunk, "forbidden-word") {
		res.Decision, res.RiskLevel, res.Action, res.Safety = securityaudit.EventCritical, securityaudit.RiskCritical, securityaudit.ActionBlock, "Unsafe"
		res.Categories, res.MatchedScanners = []string{"jailbreak"}, []string{"jailbreak"}
	}
	return res, nil
}

// guardEngine is a blocking prompt guard built like PromptService.Evaluate
// (prompt_service.go): the real snapshot extraction and the real
// GuardEvaluator, with the guard upstream replaced by blockWordScanner. It
// also records what the async path would store (prompt_enqueue.go:
// snapshot.Redacted() in the job row, ScanText in the payload store).
type guardEngine struct {
	scanner   *blockWordScanner
	evaluator *securityaudit.GuardEvaluator
	mu        sync.Mutex
	bodies    [][]byte
	blocking  []securityaudit.PromptSnapshot
	stored    []securityaudit.PromptSnapshot
}

func newGuardEngine() *guardEngine {
	sc := &blockWordScanner{}
	return &guardEngine{scanner: sc, evaluator: securityaudit.NewGuardEvaluator(sc, nil, securityaudit.NewAtomicMetrics())}
}

func (e *guardEngine) EffectiveMode() securityaudit.Mode { return securityaudit.ModeBlocking }
func (e *guardEngine) Enqueue(context.Context, securityaudit.Request) error {
	return nil
}
func (e *guardEngine) Evaluate(ctx context.Context, req securityaudit.Request) (*securityaudit.PromptDecision, error) {
	blocking, err := securityaudit.ExtractBlockingPromptSnapshot(req, false)
	if err != nil {
		return nil, err
	}
	stored, err := securityaudit.ExtractPromptSnapshot(req)
	if err != nil {
		return nil, err
	}
	e.mu.Lock()
	e.bodies = append(e.bodies, append([]byte(nil), req.Body...))
	e.blocking = append(e.blocking, blocking)
	e.stored = append(e.stored, stored)
	e.mu.Unlock()
	cfg := securityaudit.ActiveConfig{RiskControlEnabled: true, Enabled: true, BlockingEnabled: true, ConfigVersion: 1,
		Scanners: securityaudit.AllScannerIDs, Endpoints: []securityaudit.ActiveEndpoint{{ID: "guard", Enabled: true, TimeoutMS: 5000, InputLimit: 4096}}}
	return e.evaluator.Evaluate(ctx, cfg, blocking)
}

const spoolModerationImageBytes = 200 << 10

func spoolModerationImageBody(text string) []byte {
	return []byte(`{"model":"gpt-5.1","stream":false,"instructions":"be brief","input":[{"type":"message","role":"user","content":[` +
		`{"type":"input_text","text":"` + text + `"},` +
		`{"type":"input_image","image_url":"data:image/png;base64,` + strings.Repeat("A", spoolModerationImageBytes) + `"}]}]}`)
}

type guardRun struct {
	code     int
	guard    *guardEngine
	upstream *largeBodyDrainUpstream
}

func runGuard(t *testing.T, body []byte, spool bool) guardRun {
	t.Helper()
	upstream := &largeBodyDrainUpstream{}
	h := newLargeBodyTestHandler(t, upstream, "oauth")
	guard := newGuardEngine()
	h.securityAuditCoordinator = securityaudit.NewCoordinator(nil, guard)
	if spool {
		enableBodySpoolForTest(t, h.cfg, true, 4<<30, nil)
	}
	c, rec := newLargeBodyTestContext("/v1/responses", body, "")
	h.Responses(c)
	if spool {
		require.Zero(t, bodySpoolRuntimeFor(h.cfg).meter.Outstanding())
	}
	require.Len(t, guard.bodies, 1)
	return guardRun{rec.Code, guard, upstream}
}

// TestBodySpoolPromptGuardVerdictUnchanged (review major 3, leader
// conditions 1-2): the prompt guard reads only text (prompt_snapshot.go:
// contentTexts keeps text/input_text/output_text parts only), so on a body
// with a spooled image its verdict, its snapshot and every chunk sent to the
// guard upstream equal those of the legacy (unspooled) body, for an allowed
// and for a blocked text.
func TestBodySpoolPromptGuardVerdictUnchanged(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tc := range []struct {
		text string
		code int
	}{
		{"what is in this picture?", http.StatusOK},
		{"please say forbidden-word now", http.StatusForbidden},
	} {
		t.Run(http.StatusText(tc.code), func(t *testing.T) {
			body := spoolModerationImageBody(tc.text)
			legacy := runGuard(t, body, false)
			spooled := runGuard(t, body, true)
			require.Equal(t, tc.code, legacy.code)
			require.Equal(t, legacy.code, spooled.code, "same verdict")
			require.Equal(t, legacy.guard.blocking, spooled.guard.blocking, "same blocking snapshot")
			require.Equal(t, legacy.guard.stored, spooled.guard.stored, "same stored snapshot")
			require.Equal(t, legacy.guard.scanner.chunks, spooled.guard.scanner.chunks, "same guard upstream input")
			require.Contains(t, spooled.guard.stored[0].ScanText, tc.text)
			if tc.code == http.StatusOK {
				require.Greater(t, spooled.upstream.forwarded.Load(), int64(spoolModerationImageBytes), "upstream gets the expanded image")
			} else {
				require.Zero(t, spooled.upstream.calls.Load())
			}
		})
	}
}

// TestBodySpoolPromptGuardSeesOnlyPlaceholder (leader condition 3): the body
// the guard reads is the original body with only the spooled part of the
// image replaced by the fixed placeholder; no nonce or marker byte reaches
// it, and neither the placeholder nor any marker internal reaches what it
// stores or sends upstream. (Its log lines carry request metadata only:
// prompt_enqueue.go and prompt_service.go log baseFields, never the body.)
func TestBodySpoolPromptGuardSeesOnlyPlaceholder(t *testing.T) {
	gin.SetMode(gin.TestMode)
	body := spoolModerationImageBody("what is in this picture?")
	run := runGuard(t, body, true)
	require.Equal(t, http.StatusOK, run.code)
	seen := string(run.guard.bodies[0])

	img := []byte("data:image/png;base64," + strings.Repeat("A", spoolModerationImageBytes))
	keep := img[:bodyspool.DefaultKeepPrefix]
	placeholder := "[spooled media: " + strconv.Itoa(len(img)-len(keep)) + " bytes]"
	require.Equal(t, strings.Replace(string(body), string(img), string(keep)+placeholder, 1), seen, "only the spooled bytes differ")
	require.False(t, markerNoncePattern.MatchString(seen))
	require.NotContains(t, seen, `\u0000`)

	var guardSide []string
	guardSide = append(guardSide, run.guard.scanner.chunks...)
	for _, snap := range append(run.guard.blocking, run.guard.stored...) {
		j, err := json.Marshal(snap.Redacted())
		require.NoError(t, err)
		guardSide = append(guardSide, string(j), snap.ScanText)
	}
	for _, text := range guardSide {
		require.False(t, markerNoncePattern.MatchString(text), text)
		require.NotContains(t, text, "spooled media")
		require.NotContains(t, text, "\x00lrc", "no raw marker")
		require.NotContains(t, text, `\u0000`)
	}
}

// legacyModerationService turns risk control on (or off) with the moderation
// API at a test server that counts its calls.
func legacyModerationService(t *testing.T, riskControl bool, calls *atomic.Int32) *service.ContentModerationService {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = w.Write([]byte(`{"results":[{"category_scores":{"sexual":0.0}}]}`))
	}))
	t.Cleanup(srv.Close)
	raw, err := json.Marshal(&service.ContentModerationConfig{
		Enabled: true, Mode: service.ContentModerationModePreBlock, BaseURL: srv.URL,
		Model: "omni-moderation-latest", APIKeys: []string{"sk-test"}, SampleRate: 100, AllGroups: true,
	})
	require.NoError(t, err)
	enabled := "false"
	if riskControl {
		enabled = "true"
	}
	settings := &contentModerationHandlerSettingRepo{values: map[string]string{
		service.SettingKeyRiskControlEnabled:      enabled,
		service.SettingKeyContentModerationConfig: string(raw),
	}}
	return service.NewContentModerationService(settings, &contentModerationHandlerTestRepo{}, nil, nil, nil, nil, nil, nil)
}

// TestBodySpoolLegacyModerationRefusesBlobs: with risk control on, a request
// with a spooled image gets 422 spooled_media_moderation_unsupported before
// any moderation call; nothing goes upstream. A text-only request (no blob,
// no spool) is moderated as before. With risk control off, nothing changes.
func TestBodySpoolLegacyModerationRefusesBlobs(t *testing.T) {
	gin.SetMode(gin.TestMode)
	text := []byte(`{"model":"gpt-5.1","stream":false,"input":"hello there"}`)
	for _, tc := range []struct {
		name        string
		riskControl bool
		body        []byte
		wantCode    int
		wantCalls   int32
	}{
		{"risk_control_on_blob", true, spoolModerationImageBody("what is in this picture?"), http.StatusUnprocessableEntity, 0},
		{"risk_control_on_text", true, text, http.StatusOK, 1},
		{"risk_control_off_blob", false, spoolModerationImageBody("what is in this picture?"), http.StatusOK, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			upstream := &largeBodyDrainUpstream{}
			h := newLargeBodyTestHandler(t, upstream, "oauth")
			h.contentModerationService = legacyModerationService(t, tc.riskControl, &calls)
			enableBodySpoolForTest(t, h.cfg, true, 4<<30, nil)
			c, rec := newLargeBodyTestContext("/v1/responses", tc.body, "")
			h.Responses(c)
			if tc.wantCode == http.StatusUnprocessableEntity {
				requireCapacityError(t, rec, http.StatusUnprocessableEntity, codeSpooledMediaModerationUnsupported, "sub2api", "content moderation")
				require.Zero(t, upstream.calls.Load())
			} else {
				require.Equal(t, tc.wantCode, rec.Code, rec.Body.String())
				require.EqualValues(t, 1, upstream.calls.Load())
			}
			require.Equal(t, tc.wantCalls, calls.Load())
			require.Zero(t, bodySpoolRuntimeFor(h.cfg).meter.Outstanding())
		})
	}
}
