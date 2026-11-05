//go:build unit

package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/securityaudit"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// Moderation and the security audit on spooled bodies (review major 3).

var markerNoncePattern = regexp.MustCompile(`lrc[0-9a-f]{32}`)

// capturePromptEngine is a blocking prompt guard that records what it reads.
type capturePromptEngine struct {
	mu        sync.Mutex
	bodies    [][]byte
	scanTexts []string
}

func (e *capturePromptEngine) EffectiveMode() securityaudit.Mode { return securityaudit.ModeBlocking }
func (e *capturePromptEngine) Enqueue(context.Context, securityaudit.Request) error {
	return nil
}
func (e *capturePromptEngine) Evaluate(_ context.Context, req securityaudit.Request) (*securityaudit.PromptDecision, error) {
	snap, err := securityaudit.ExtractPromptSnapshot(req)
	e.mu.Lock()
	defer e.mu.Unlock()
	e.bodies = append(e.bodies, append([]byte(nil), req.Body...))
	if err == nil {
		e.scanTexts = append(e.scanTexts, snap.ScanText)
	}
	return &securityaudit.PromptDecision{Kind: securityaudit.DecisionAllow, AllowNextStage: true}, nil
}

func spoolModerationImageBody() []byte {
	return []byte(`{"model":"gpt-5.1","stream":false,"input":[{"type":"message","role":"user","content":[` +
		`{"type":"input_text","text":"what is in this picture?"},` +
		`{"type":"input_image","image_url":"data:image/png;base64,` + strings.Repeat("A", 200<<10) + `"}]}]}`)
}

// TestBodySpoolPromptGuardSeesPlaceholder: the prompt guard reads a copy of
// the skeleton with a nonce-free placeholder per blob. Its text extraction
// is the same as for the legacy (unspooled) body, and the request passes.
func TestBodySpoolPromptGuardSeesPlaceholder(t *testing.T) {
	gin.SetMode(gin.TestMode)
	body := spoolModerationImageBody()
	var scans []string
	for _, spool := range []bool{false, true} {
		upstream := &largeBodyDrainUpstream{}
		h := newLargeBodyTestHandler(t, upstream, "oauth")
		guard := &capturePromptEngine{}
		h.securityAuditCoordinator = securityaudit.NewCoordinator(nil, guard)
		if spool {
			enableBodySpoolForTest(t, h.cfg, true, 4<<30, nil)
		}
		c, rec := newLargeBodyTestContext("/v1/responses", body, "")
		h.Responses(c)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		require.Greater(t, upstream.forwarded.Load(), int64(200<<10), "upstream gets the expanded image")
		require.Len(t, guard.bodies, 1)
		require.Len(t, guard.scanTexts, 1)
		scans = append(scans, guard.scanTexts[0])
		if spool {
			seen := string(guard.bodies[0])
			require.Contains(t, seen, "[spooled media: ")
			require.NotContains(t, seen, `\u0000`)
			require.False(t, markerNoncePattern.MatchString(seen), "no nonce reaches the guard")
			require.True(t, json.Valid(guard.bodies[0]))
			require.Less(t, len(seen), 4<<10)
			require.Zero(t, bodySpoolRuntimeFor(h.cfg).meter.Outstanding())
		}
	}
	require.Equal(t, scans[0], scans[1], "text is audited exactly as before")
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
		{"risk_control_on_blob", true, spoolModerationImageBody(), http.StatusUnprocessableEntity, 0},
		{"risk_control_on_text", true, text, http.StatusOK, 1},
		{"risk_control_off_blob", false, spoolModerationImageBody(), http.StatusOK, 0},
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
