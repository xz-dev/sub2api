//go:build unit

package handler

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

// coverGatewayAccount is one non-OpenAI account kind and its group platform.
func coverGatewayAccount(kind string) (service.Account, string) {
	a := service.Account{ID: 7, Name: kind, Status: service.StatusActive, Schedulable: true, Concurrency: 0}
	switch kind {
	case "gemini-apikey":
		a.Platform, a.Type = service.PlatformGemini, service.AccountTypeAPIKey
		a.Credentials = map[string]any{"api_key": "gk-test", "base_url": "https://generativelanguage.googleapis.com"}
	case "antigravity-oauth":
		a.Platform, a.Type = service.PlatformAntigravity, service.AccountTypeOAuth
		a.Credentials = map[string]any{"access_token": "ag-token", "project_id": "p-1",
			"expires_at": time.Now().Add(24 * time.Hour).Format(time.RFC3339)}
	case "grok-apikey":
		a.Platform, a.Type = service.PlatformGrok, service.AccountTypeAPIKey
		a.Credentials = map[string]any{"api_key": "xai-test", "base_url": "https://api.x.ai"}
	}
	return a, a.Platform
}

// coverAccountRepo adds the multi-platform lists mixed scheduling uses.
type coverAccountRepo struct {
	openAIImagesFailoverAccountRepo
}

func (r coverAccountRepo) forPlatforms(platforms []string) []service.Account {
	var out []service.Account
	for _, p := range platforms {
		out = append(out, r.accountsForPlatform(p)...)
	}
	return out
}

func (r coverAccountRepo) ListSchedulableByGroupIDAndPlatforms(_ context.Context, _ int64, platforms []string) ([]service.Account, error) {
	return r.forPlatforms(platforms), nil
}

func (r coverAccountRepo) ListSchedulableByPlatforms(_ context.Context, platforms []string) ([]service.Account, error) {
	return r.forPlatforms(platforms), nil
}

func (r coverAccountRepo) ListSchedulableUngroupedByPlatforms(_ context.Context, platforms []string) ([]service.Account, error) {
	return r.forPlatforms(platforms), nil
}

// coverSettings answers every setting read as unset, so each feature takes
// its default.
type coverSettings struct{ service.SettingRepository }

func (coverSettings) Get(context.Context, string) (*service.Setting, error) {
	return nil, service.ErrSettingNotFound
}
func (coverSettings) GetValue(context.Context, string) (string, error) {
	return "", service.ErrSettingNotFound
}
func (coverSettings) GetMultiple(context.Context, []string) (map[string]string, error) {
	return map[string]string{}, nil
}
func (coverSettings) GetAll(context.Context) (map[string]string, error) {
	return map[string]string{}, nil
}

// newCoverGatewayHandler builds a GatewayHandler over one account.
func newCoverGatewayHandler(t *testing.T, up *coverUpstream, account service.Account) *GatewayHandler {
	t.Helper()
	cfg := &config.Config{RunMode: config.RunModeSimple}
	cfg.Gateway.MaxBodySize = 256 << 20
	repo := coverAccountRepo{openAIImagesFailoverAccountRepo{accounts: []service.Account{account}}}
	billingCache := service.NewBillingCacheService(newHandlerInflightCache(100), nil, nil, nil, nil, nil, cfg, nil)
	t.Cleanup(billingCache.Stop)
	billing := service.NewBillingService(cfg, nil)
	concurrency := service.NewConcurrencyService(nil)
	antigravity := service.NewAntigravityGatewayService(repo, nil, nil,
		service.NewAntigravityTokenProvider(repo, nil, nil), nil, up, service.NewSettingService(coverSettings{}, cfg), nil)
	gemini := service.NewGeminiMessagesCompatService(repo, nil, nil, nil, nil, nil, up, antigravity, cfg)
	gw := service.NewGatewayService(
		repo, nil, nil, nil, nil, nil, nil, nil, cfg, nil, concurrency, billing, nil, billingCache,
		nil, up, nil, nil, nil, nil, nil, nil, nil, nil, service.NewModelPricingResolver(nil, billing), nil, nil, nil,
	)
	return NewGatewayHandler(gw, nil, gemini, antigravity, nil, concurrency, billingCache, nil,
		service.NewAPIKeyService(nil, nil, nil, nil, nil, nil, cfg), nil, nil, nil, nil, cfg, nil)
}

func coverGatewayCombos() []coverCombo {
	type route struct {
		route, account, proto, model, pattern, path string
		force                                       bool // /antigravity/*: ForcePlatform(antigravity)
		serve                                       func(h *GatewayHandler) gin.HandlerFunc
	}
	messages := func(h *GatewayHandler) gin.HandlerFunc { return h.Messages }
	count := func(h *GatewayHandler) gin.HandlerFunc { return h.CountTokens }
	models := func(h *GatewayHandler) gin.HandlerFunc { return h.GeminiV1BetaModels }
	chat := func(h *GatewayHandler) gin.HandlerFunc { return h.ChatCompletions }
	responses := func(h *GatewayHandler) gin.HandlerFunc { return h.Responses }
	const gm, cm = "gemini-2.5-pro", "claude-sonnet-4-5"
	routes := []route{
		{"v1/messages", "gemini-apikey", "claude", gm, "/v1/messages", "/v1/messages", false, messages},
		{"count_tokens", "gemini-apikey", "claude", gm, "/v1/messages/count_tokens", "/v1/messages/count_tokens", false, count},
		{"chat/completions", "gemini-apikey", "chat", gm, "/v1/chat/completions", "/v1/chat/completions", false, chat},
		{"v1/responses", "gemini-apikey", "responses", gm, "/v1/responses", "/v1/responses", false, responses},
		{"v1beta/generateContent", "gemini-apikey", "gemini", gm, "/v1beta/models/*modelAction", "/v1beta/models/" + gm + ":generateContent", false, models},
		{"v1/messages", "antigravity-oauth", "claude", cm, "/v1/messages", "/v1/messages", false, messages},
		{"antigravity/v1/messages", "antigravity-oauth", "claude", cm, "/antigravity/v1/messages", "/antigravity/v1/messages", true, messages},
		{"antigravity/count_tokens", "antigravity-oauth", "claude", cm, "/antigravity/v1/messages/count_tokens", "/antigravity/v1/messages/count_tokens", true, count},
		{"antigravity/v1beta/generateContent", "antigravity-oauth", "gemini", gm, "/antigravity/v1beta/models/*modelAction", "/antigravity/v1beta/models/" + gm + ":generateContent", true, models},
		{"web_search", "grok-apikey", "websearch", "", "/v1/web_search", "/v1/web_search", false, func(h *GatewayHandler) gin.HandlerFunc { return h.WebSearch }},
	}
	var out []coverCombo
	for _, rt := range routes {
		out = append(out, coverCombo{
			route: rt.route, account: rt.account, proto: rt.proto, model: rt.model,
			pattern: rt.pattern, path: rt.path,
			setup: func(t *testing.T) (http.Handler, *config.Config, *coverUpstream) {
				up := &coverUpstream{}
				account, platform := coverGatewayAccount(rt.account)
				h := newCoverGatewayHandler(t, up, account)
				enableBodySpoolForTest(t, h.cfg, true, 8<<30, nil)
				chain := []gin.HandlerFunc{coverAuth(platform)}
				if rt.force {
					chain = append([]gin.HandlerFunc{middleware.ForcePlatform(service.PlatformAntigravity)}, chain...)
				}
				chain = append(chain, BodySpoolIngest(h.cfg), rt.serve(h))
				r := gin.New()
				r.POST(rt.pattern, chain...)
				return r, h.cfg, up
			},
		})
	}
	return out
}
