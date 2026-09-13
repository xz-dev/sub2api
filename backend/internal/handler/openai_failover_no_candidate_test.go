//go:build unit

package handler

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

type noCandidateCauseRepo struct {
	service.AccountRepository
	mu       sync.Mutex
	accounts []service.Account
	excluded map[int64]bool
	selects  int
}

func (r *noCandidateCauseRepo) candidates(platform string) []service.Account {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.selects++
	out := make([]service.Account, 0, len(r.accounts))
	for _, account := range r.accounts {
		if account.Platform == platform && account.IsSchedulable() && !r.excluded[account.ID] {
			out = append(out, account)
		}
	}
	return out
}

func (r *noCandidateCauseRepo) ListSchedulableByPlatform(_ context.Context, platform string) ([]service.Account, error) {
	return r.candidates(platform), nil
}
func (r *noCandidateCauseRepo) ListSchedulableByGroupIDAndPlatform(_ context.Context, _ int64, platform string) ([]service.Account, error) {
	return r.candidates(platform), nil
}
func (r *noCandidateCauseRepo) ListSchedulableUngroupedByPlatform(_ context.Context, platform string) ([]service.Account, error) {
	return r.candidates(platform), nil
}
func (r *noCandidateCauseRepo) GetByID(_ context.Context, id int64) (*service.Account, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, account := range r.accounts {
		if account.ID == id {
			copy := account
			return &copy, nil
		}
	}
	return nil, nil
}
func (r *noCandidateCauseRepo) exclude(id int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.excluded == nil {
		r.excluded = make(map[int64]bool)
	}
	r.excluded[id] = true
}

func (r *noCandidateCauseRepo) SetRateLimited(_ context.Context, id int64, resetAt time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := range r.accounts {
		if r.accounts[i].ID == id {
			r.accounts[i].RateLimitResetAt = &resetAt
		}
	}
	return nil
}

// noCandidateCauseUpstream emits one synthetic 429 per attempted account.
type noCandidateCauseUpstream struct {
	service.HTTPUpstream
	mu    sync.Mutex
	calls []int64
	repo  *noCandidateCauseRepo
}

func (u *noCandidateCauseUpstream) Do(_ *http.Request, _ string, accountID int64, _ int) (*http.Response, error) {
	u.mu.Lock()
	u.calls = append(u.calls, accountID)
	attempts := 0
	for _, id := range u.calls {
		if id == accountID {
			attempts++
		}
	}
	u.mu.Unlock()
	// Explicit synthetic policy: pool account gets exactly its declared retry
	// once; each account is then excluded so next selection reaches no-candidate.
	if attempts >= 2 || accountID == 73011 {
		u.repo.exclude(accountID)
	}
	return &http.Response{
		StatusCode: http.StatusTooManyRequests,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(`{"error":{"code":"rate_limit_exceeded","message":"synthetic final account limit"}}`)),
	}, nil
}

func TestOpenAIResponsesFailoverCauseSurvivesNoCandidate(t *testing.T) {
	gin.SetMode(gin.TestMode)
	groupID := int64(7301)
	accounts := []service.Account{
		{ID: 73010, Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey, Status: service.StatusActive, Schedulable: true, Priority: 1,
			Credentials: map[string]any{"api_key": "sk-a", "base_url": "https://synthetic.invalid", "pool_mode": true, "pool_mode_retry_count": float64(1)}, Extra: map[string]any{"openai_passthrough": true}},
		{ID: 73011, Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey, Status: service.StatusActive, Schedulable: true, Priority: 2,
			Credentials: map[string]any{"api_key": "sk-b", "base_url": "https://synthetic.invalid"}, Extra: map[string]any{"openai_passthrough": true}},
	}
	cfg := &config.Config{RunMode: config.RunModeSimple}
	cfg.Default.RateMultiplier = 1
	cfg.Security.URLAllowlist.Enabled = false
	cfg.Gateway.MaxAccountSwitches = 5 // bounded policy: enough to re-select after both candidates are excluded.
	repo := &noCandidateCauseRepo{accounts: accounts}
	upstream := &noCandidateCauseUpstream{}
	upstream.repo = repo
	billing := service.NewBillingCacheService(nil, nil, nil, nil, nil, nil, cfg, nil)
	t.Cleanup(billing.Stop)
	gateway := service.NewOpenAIGatewayService(repo, nil, nil, nil, nil, nil, nil, cfg, nil, nil,
		service.NewBillingService(cfg, nil), service.NewRateLimitService(repo, nil, cfg, nil, nil), billing, upstream,
		&service.DeferredService{}, nil, nil, nil, nil, nil, nil, nil)
	h := NewOpenAIGatewayHandler(gateway, service.NewConcurrencyService(nil), billing,
		service.NewAPIKeyService(nil, nil, nil, nil, nil, nil, cfg), nil, nil, nil, nil, cfg)

	req := func() *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		c.Request = httptest.NewRequest(http.MethodPost, "/openai/v1/responses", strings.NewReader(`{"model":"gpt-5.2","input":"hello","stream":false}`))
		c.Request.Header.Set("Content-Type", "application/json")
		c.Set(string(middleware.ContextKeyAPIKey), &service.APIKey{ID: 7302, GroupID: &groupID, User: &service.User{ID: 7303, Status: service.StatusActive}, Group: &service.Group{ID: groupID, Platform: service.PlatformOpenAI, Status: service.StatusActive}})
		c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: 7303, Concurrency: 0})
		h.Responses(c)
		return rec
	}

	first := req()
	require.Equal(t, []int64{73010, 73010, 73011}, upstream.calls)
	t.Logf("first wire: %s", first.Body.String())
	require.Equal(t, http.StatusTooManyRequests, first.Code)
	require.Equal(t, "rate_limit_error", gjson.Get(first.Body.String(), "error.type").String())
	require.Equal(t, "Upstream rate limit exceeded, please retry later", gjson.Get(first.Body.String(), "error.message").String())
	require.NotContains(t, first.Body.String(), "no available")
	firstSelects := repo.selects
	t.Logf("first request upstream calls=%v selector calls=%d", upstream.calls, firstSelects)
	require.Equal(t, 4, firstSelects, "two initial candidate passes, switch selection, then no-candidate selection")

	second := req()
	t.Logf("second wire: %s", second.Body.String())
	require.Equal(t, []int64{73010, 73010, 73011}, upstream.calls)
	require.Equal(t, http.StatusBadGateway, second.Code)
	require.Equal(t, "upstream_error", gjson.Get(second.Body.String(), "error.type").String())
	require.Equal(t, "upstream_error", gjson.Get(second.Body.String(), "error.code").String())
	require.Equal(t, "Upstream request failed", gjson.Get(second.Body.String(), "error.message").String())
	require.NotContains(t, second.Body.String(), "rate_limit_error")
	require.NotContains(t, second.Body.String(), "synthetic final account limit")
	require.Greater(t, repo.selects, firstSelects, "fresh request must perform its own no-candidate selection")
}
