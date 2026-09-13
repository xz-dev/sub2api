//go:build unit

package handler

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type nativeDefaultRetryUpstream struct {
	service.HTTPUpstream
	mu    sync.Mutex
	calls []int64
	times []time.Time
	seen  map[int64]int
	mode  string
}

func (u *nativeDefaultRetryUpstream) Do(_ *http.Request, _ string, accountID int64, _ int) (*http.Response, error) {
	u.mu.Lock()
	if u.seen == nil {
		u.seen = make(map[int64]int)
	}
	u.seen[accountID]++
	u.calls = append(u.calls, accountID)
	u.times = append(u.times, time.Now())
	attempt := u.seen[accountID]
	u.mu.Unlock()

	status := http.StatusTooManyRequests
	body := `{"error":{"message":"synthetic default 429"}}`
	if u.mode == "same-account-success" && attempt == 2 {
		status = http.StatusOK
		body = `{"id":"resp_same_account","object":"response","model":"gpt-5.2","status":"completed"}`
	}
	if u.mode == "switch-success" && accountID == 73021 {
		status = http.StatusOK
		body = `{"id":"resp_fallback","object":"response","model":"gpt-5.2","status":"completed"}`
	}
	if u.mode == "switch-success" && accountID == 73020 {
		status = http.StatusBadGateway
		body = `{"error":{"message":"synthetic 502"}}`
	}
	return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(body))}, nil
}

func (u *nativeDefaultRetryUpstream) snapshot() ([]int64, []time.Time) {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]int64(nil), u.calls...), append([]time.Time(nil), u.times...)
}

func nativeDefaultRetryHandler(t *testing.T, accounts []service.Account, upstream *nativeDefaultRetryUpstream) *OpenAIGatewayHandler {
	t.Helper()
	cfg := &config.Config{RunMode: config.RunModeSimple}
	cfg.Default.RateMultiplier = 1
	cfg.Security.URLAllowlist.Enabled = false
	cfg.Gateway.MaxAccountSwitches = 1
	repo := &openAIWSFailoverHandlerAccountRepoStub{accounts: accounts}
	billing := service.NewBillingCacheService(nil, nil, nil, nil, nil, nil, cfg, nil)
	t.Cleanup(billing.Stop)
	gateway := service.NewOpenAIGatewayService(repo, nil, nil, nil, nil, nil, nil, cfg, nil, nil,
		service.NewBillingService(cfg, nil), nil, billing, upstream, &service.DeferredService{}, nil, nil, nil, nil, nil, nil, nil)
	return NewOpenAIGatewayHandler(gateway, service.NewConcurrencyService(nil), billing,
		service.NewAPIKeyService(nil, nil, nil, nil, nil, nil, cfg), nil, nil, nil, nil, cfg)
}

func nativeDefaultRetryRequest(t *testing.T) (*gin.Context, *httptest.ResponseRecorder) {
	t.Helper()
	groupID := int64(7302)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/openai/v1/responses", strings.NewReader(`{"model":"gpt-5.2","input":"hello","stream":false}`))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Set(string(middleware.ContextKeyAPIKey), &service.APIKey{ID: 7302, GroupID: &groupID,
		User: &service.User{ID: 7303, Status: service.StatusActive}, Group: &service.Group{ID: groupID, Platform: service.PlatformOpenAI, Status: service.StatusActive}})
	c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: 7303, Concurrency: 0})
	return c, rec
}

func TestOpenAINativeDefaultRetryScope(t *testing.T) {
	gin.SetMode(gin.TestMode)
	t.Run("default429 retries same account once", func(t *testing.T) {
		upstream := &nativeDefaultRetryUpstream{mode: "same-account-success"}
		h := nativeDefaultRetryHandler(t, []service.Account{{ID: 73020, Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey, Status: service.StatusActive, Schedulable: true, Priority: 1,
			Credentials: map[string]any{"api_key": "sk-a", "base_url": "https://synthetic.invalid", "pool_mode": true, "pool_mode_retry_count": float64(1)}, Extra: map[string]any{"openai_passthrough": true}}}, upstream)
		synctest.Test(t, func(t *testing.T) {
			c, rec := nativeDefaultRetryRequest(t)
			h.Responses(c)
			calls, times := upstream.snapshot()
			require.Equal(t, []int64{73020, 73020}, calls)
			require.Equal(t, http.StatusOK, rec.Code)
			require.Equal(t, 500*time.Millisecond, times[1].Sub(times[0]))
		})
	})

	t.Run("default502 switches without same-account retry", func(t *testing.T) {
		upstream := &nativeDefaultRetryUpstream{mode: "switch-success"}
		h := nativeDefaultRetryHandler(t, []service.Account{
			{ID: 73020, Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey, Status: service.StatusActive, Schedulable: true, Priority: 1,
				Credentials: map[string]any{"api_key": "sk-a", "base_url": "https://synthetic.invalid", "pool_mode": true, "pool_mode_retry_count": float64(1)}, Extra: map[string]any{"openai_passthrough": true}},
			{ID: 73021, Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey, Status: service.StatusActive, Schedulable: true, Priority: 2,
				Credentials: map[string]any{"api_key": "sk-b", "base_url": "https://synthetic.invalid"}, Extra: map[string]any{"openai_passthrough": true}},
		}, upstream)
		synctest.Test(t, func(t *testing.T) {
			c, rec := nativeDefaultRetryRequest(t)
			h.Responses(c)
			calls, times := upstream.snapshot()
			require.Equal(t, []int64{73020, 73021}, calls)
			require.Equal(t, http.StatusOK, rec.Code)
			require.Equal(t, time.Duration(0), times[1].Sub(times[0]))
		})
	})
}
