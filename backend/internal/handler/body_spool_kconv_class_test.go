//go:build unit

package handler

import (
	"math"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// TestKConvClasses (rr10): every row is ceil(measured x 1.15), at least
// config k_conv; each route lands in its class; and at 320m every class's
// S_conv / L_conv stays above its route's production maximum (7 days:
// non-Responses 0.53 MiB, chat 0.53 MiB, other /v1 0.21 MiB).
func TestKConvClasses(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cfg := config.GatewayBodySpoolConfig{}.Effective()
	for name, row := range kConvClasses {
		if row.measured == 0 {
			continue
		}
		require.Equal(t, math.Max(cfg.KConv, math.Ceil(row.measured*1.15)), row.k, name)
	}
	worst := 0.0
	for _, row := range kConvClasses {
		worst = math.Max(worst, row.k)
	}
	require.Equal(t, worst, kConvClasses["unmeasured"].k, "an unmeasured route takes the worst class")

	// The class comes from the matched route, so each path is served through
	// an engine with the registered patterns of routes/gateway.go.
	var covered, responsesFallback bool
	var got string
	engine := gin.New()
	for _, pattern := range []string{
		"/v1/responses", "/v1/responses/*subpath", "/responses", "/responses/*subpath",
		"/backend-api/codex/responses", "/backend-api/codex/responses/*subpath",
		"/v1/chat/completions", "/chat/completions",
		"/v1/messages", "/v1/messages/count_tokens", "/messages/count_tokens",
		"/antigravity/v1/messages", "/antigravity/v1/messages/count_tokens",
		"/v1beta/models/*modelAction", "/v1beta/cachedContents", "/antigravity/v1beta/models/*modelAction",
		"/v1/web_search", "/web_search", "/v1/embeddings", "/embeddings", "/v1/rerank", "/v1/images/generations",
	} {
		engine.POST(pattern, func(c *gin.Context) { got = kConvClassFor(c, covered, responsesFallback) })
	}
	for _, tc := range []struct {
		path                       string
		covered, responsesFallback bool
		class                      string
	}{
		{"/v1/responses", true, false, "responses"},
		{"/v1/responses/compact", false, true, "responses_fallback"},
		{"/v1/responses", false, false, "responses_other_platform"},
		{"/v1/chat/completions", false, false, "chat"},
		{"/chat/completions", false, false, "chat"},
		{"/v1/messages", false, false, "messages"},
		{"/antigravity/v1/messages", false, false, "messages"},
		{"/v1/messages/count_tokens", false, false, "count_tokens"},
		{"/messages/count_tokens", false, false, "count_tokens"},
		{"/antigravity/v1/messages/count_tokens", false, false, "count_tokens"},
		{"/v1beta/models/gemini-2.5-pro:generateContent", false, false, "gemini"},
		{"/antigravity/v1beta/models/g:streamGenerateContent", false, false, "gemini"},
		{"/v1beta/cachedContents", false, false, "unmeasured"},
		{"/v1/web_search", false, false, "web_search"},
		{"/web_search", false, false, "web_search"},
		{"/v1/embeddings", false, false, "embeddings"},
		{"/embeddings", false, false, "embeddings"},
		{"/v1/rerank", false, false, "rerank"},
		{"/v1/images/generations", false, false, "unmeasured"},
		// Responses subpaths are served by the Responses handler, whatever
		// their last segment.
		{"/v1/responses/x/web_search", false, false, "unmeasured"},
		{"/v1/responses/x/messages", false, false, "unmeasured"},
		{"/v1/responses/x/count_tokens", false, false, "unmeasured"},
		{"/v1/responses/x/chat/completions", false, false, "unmeasured"},
		{"/responses/x/web_search", false, false, "unmeasured"},
		{"/backend-api/codex/responses/x/web_search", false, false, "unmeasured"},
		{"/v1/responses/embeddings", false, false, "unmeasured"},
		{"/v1/responses/x/rerank", false, false, "unmeasured"},
		{"/responses/embeddings", false, false, "unmeasured"},
		{"/v1/responses/x/web_search", false, true, "responses_fallback"},
	} {
		covered, responsesFallback, got = tc.covered, tc.responsesFallback, ""
		engine.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", tc.path, nil))
		require.Equal(t, tc.class, got, tc.path)
	}
	// Without a matched route the request path must equal a route.
	for path, class := range map[string]string{"/web_search": "web_search", "/v1/chat/completions/": "chat",
		"/v1/responses/x/web_search": "unmeasured", "/x/v1/messages": "unmeasured"} {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest("POST", path, nil)
		require.Equal(t, class, kConvClassFor(c, false, false), path)
	}

	// /v1beta/cachedContents is unmeasured, so it gets the unmeasured limit,
	// below L, until it is measured. The routes test
	// TestGatewayRoutesUnconvertedGeminiAndAntigravityRefuseAboveL serves it
	// on a 4 GiB hop with this cfg and reads this limit from its 413.
	covered, responsesFallback, got = false, false, ""
	engine.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/v1beta/cachedContents", nil))
	cachedLimit := conversionLimit(cfg, 4<<30, false, false, kConvFor(cfg, got))
	require.Equal(t, conversionLimit(cfg, 4<<30, false, false, kConvFor(cfg, "unmeasured")), cachedLimit)
	require.Less(t, cachedLimit, cfg.FallbackLimit, "the unmeasured limit at 4 GiB is below L")

	const mib = 1 << 20
	production := map[string]float64{"responses": 0.21, "responses_fallback": 0.21, "responses_other_platform": 0.21,
		"chat": 0.53, "messages": 0.53, "count_tokens": 0.53, "gemini": 0.53, "web_search": 0.53, "embeddings": 0.53, "rerank": 0.53, "unmeasured": 0.53}
	for name := range kConvClasses {
		k := kConvFor(cfg, name)
		limit := conversionLimit(cfg, 320<<20, name == "responses", false, k)
		t.Logf("class %-26s k_conv %3.0f  limit at 320m %8d B (%.2f MiB), production max %.2f MiB", name, k, limit, float64(limit)/mib, production[name])
		require.Greater(t, float64(limit)/mib, production[name], name)
	}
}
