//go:build unit

package handler

import (
	"context"
	"net/http"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/securityaudit"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

// rr6 reviewer probe: the prompt guard on (production), blocking mode,
// with the production snapshot extraction and the real GuardEvaluator; no
// test copies of the body or the snapshot are kept.
type coverGuardEngine struct {
	evaluator *securityaudit.GuardEvaluator
}

func (e *coverGuardEngine) EffectiveMode() securityaudit.Mode { return securityaudit.ModeBlocking }
func (e *coverGuardEngine) Enqueue(context.Context, securityaudit.Request) error {
	return nil
}
func (e *coverGuardEngine) Evaluate(ctx context.Context, req securityaudit.Request) (*securityaudit.PromptDecision, error) {
	snapshot, err := securityaudit.ExtractBlockingPromptSnapshot(req, false)
	if err != nil {
		return nil, err
	}
	cfg := securityaudit.ActiveConfig{RiskControlEnabled: true, Enabled: true, BlockingEnabled: true, ConfigVersion: 1,
		Scanners: securityaudit.AllScannerIDs, Endpoints: []securityaudit.ActiveEndpoint{{ID: "guard", Enabled: true, TimeoutMS: 5000, InputLimit: 4096}}}
	return e.evaluator.Evaluate(ctx, cfg, snapshot)
}

func rr6GuardCombos() []coverCombo {
	var out []coverCombo
	for _, rt := range []struct{ route, proto, pattern string }{
		{"v1/responses", "responses", "/v1/responses"},
		{"v1/responses/compact", "responses", "/v1/responses/compact"},
	} {
		out = append(out, coverCombo{
			route: rt.route, account: "guard-apikey-force_responses", proto: rt.proto, model: "gpt-5.5",
			pattern: rt.pattern, path: rt.pattern,
			setup: func(t *testing.T) (http.Handler, *config.Config, *coverUpstream) {
				up := &coverUpstream{}
				accounts := largeBodyTestAccounts("apikey")
				for i := range accounts {
					accounts[i].Extra["openai_responses_mode"] = "force_responses"
				}
				h := newLargeBodyTestHandlerWithAccounts(t, up, accounts)
				sc := &blockWordScanner{}
				h.securityAuditCoordinator = securityaudit.NewCoordinator(nil,
					&coverGuardEngine{evaluator: securityaudit.NewGuardEvaluator(sc, nil, securityaudit.NewAtomicMetrics())})
				enableBodySpoolForTest(t, h.cfg, true, 8<<30, nil)
				r := gin.New()
				r.POST(rt.pattern, coverAuth(service.PlatformOpenAI), BodySpoolIngest(h.cfg), h.Responses)
				return r, h.cfg, up
			},
		})
	}
	return out
}
