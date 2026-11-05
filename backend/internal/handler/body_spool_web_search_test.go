//go:build unit

package handler

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/bodyspool"
	"github.com/Wei-Shaw/sub2api/internal/securityaudit"
	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// TestWebSearchCapacityRefusalIsNamed (review a4a7d843 minor): when the
// guard decode charge does not fit, web_search writes the capacity refusal
// with code, hop and limit_bytes, not a bare security-audit error.
func TestWebSearchCapacityRefusalIsNamed(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cfg := &config.Config{}
	billingCache := service.NewBillingCacheService(newHandlerInflightCache(100), nil, nil, nil, nil, nil, cfg, nil)
	t.Cleanup(billingCache.Stop)
	billing := service.NewBillingService(cfg, nil)
	gw := service.NewGatewayService(
		nil, nil, nil, nil, nil, nil, nil, nil, cfg, nil, nil, billing, nil, nil,
		nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, service.NewModelPricingResolver(nil, billing), nil, nil, nil,
	)
	h := &GatewayHandler{gatewayService: gw, billingCacheService: billingCache,
		securityAuditCoordinator: securityaudit.NewCoordinator(nil, newGuardEngine())}

	// 64 MiB limit, 450 KiB free above the margin: the first granule fits,
	// the guard's 512 KiB does not.
	env := enableBodySpoolForTest(t, cfg, false, 64<<20, map[string]any{"idle_baseline": 1 << 20})
	require.NoError(t, os.WriteFile(filepath.Join(env.cgroup, "memory.current"), []byte(strconv.Itoa(32<<20-450<<10)+"\n"), 0o600))
	groupID := int64(3)
	apiKey := &service.APIKey{ID: 9, User: &service.User{ID: 42, Balance: 100}, GroupID: &groupID,
		Group: &service.Group{ID: groupID, Platform: service.PlatformGrok, RateMultiplier: 1}}
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/web_search", bytes.NewBufferString(`{"query":"sub2api"}`))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Set(string(middleware2.ContextKeyAPIKey), apiKey)
	require.True(t, ingestRequestBody(c, cfg), w.Body.String())
	defer releaseRequestBodySpool(c)
	h.WebSearch(c)

	require.Equal(t, http.StatusServiceUnavailable, w.Code, w.Body.String())
	var resp struct {
		Error struct {
			Code       string `json:"code"`
			Hop        string `json:"hop"`
			LimitBytes int64  `json:"limit_bytes"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.Equal(t, bodyspool.CodeInsufficientMemory, resp.Error.Code)
	require.Equal(t, "sub2api", resp.Error.Hop)
	require.Positive(t, resp.Error.LimitBytes)
}
