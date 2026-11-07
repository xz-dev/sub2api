package routes

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/handler"
	servermiddleware "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// Task 7.2 extension: /v1beta/* and /antigravity/* are unconverted routes.
// Above L they return the same named 413 as the other unconverted routes,
// from the body-spool ingest, before the group allowlist, account selection
// or any upstream call. The allowlist (blocked model, 404) marks "the request
// got past the ingest".

const spoolRouteTestKey = "spool-route-test-key"

type spoolRouteAPIKeyRepo struct {
	service.APIKeyRepository
	apiKey *service.APIKey
}

func (r *spoolRouteAPIKeyRepo) GetByKeyForAuth(_ context.Context, key string) (*service.APIKey, error) {
	if key != r.apiKey.Key {
		return nil, service.ErrAPIKeyNotFound
	}
	clone := *r.apiKey
	return &clone, nil
}

func (r *spoolRouteAPIKeyRepo) UpdateLastUsed(context.Context, int64, time.Time) error { return nil }

func newBodySpoolRouteTestRouter(t *testing.T, platform string) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	cg := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(cg, "memory.max"), []byte(strconv.Itoa(4<<30)), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(cg, "memory.current"), []byte("0"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(cg, "memory.stat"), []byte("anon 0\ninactive_file 0\n"), 0o600))
	spool := filepath.Join(t.TempDir(), "spool")
	require.NoError(t, os.Mkdir(spool, 0o700))

	cfg := &config.Config{RunMode: config.RunModeSimple}
	cfg.Gateway.MaxBodySize = 64 << 20
	cfg.Gateway.TextMaxBodySize = 64 << 20
	cfg.Gateway.BodySpool.Enabled = true
	cfg.Gateway.BodySpool.Dir = spool
	cfg.Gateway.BodySpool.CgroupDir = cg
	cfg.Gateway.BodySpool.LedgerDir = t.TempDir()

	group := &service.Group{
		ID: 7, Status: service.StatusActive, Hydrated: true, Platform: platform,
		SubscriptionType: service.SubscriptionTypeStandard,
		ModelAllowlist:   service.GroupModelAllowlist{Enabled: true, Models: []string{"allowed-model"}},
	}
	user := &service.User{ID: 8, Role: service.RoleUser, Status: service.StatusActive, Balance: 10}
	apiKey := &service.APIKey{ID: 9, UserID: user.ID, Key: spoolRouteTestKey, Status: service.StatusActive,
		User: user, GroupID: &group.ID, Group: group}
	apiKeyService := service.NewAPIKeyService(&spoolRouteAPIKeyRepo{apiKey: apiKey}, nil, nil, nil, nil, nil, cfg)

	router := gin.New()
	RegisterGatewayRoutes(
		router,
		&handler.Handlers{Gateway: &handler.GatewayHandler{}, OpenAIGateway: &handler.OpenAIGatewayHandler{}},
		servermiddleware.NewAPIKeyAuthMiddleware(apiKeyService, nil, cfg),
		apiKeyService, nil, nil, nil, nil, cfg,
	)
	return router
}

// spoolRouteBody is valid JSON of exactly n bytes naming model in the body.
func spoolRouteBody(n int, model string) []byte {
	head := `{"model":"` + model + `","contents":[{"parts":[{"text":"`
	tail := `"}]}]}`
	return []byte(head + strings.Repeat("a", n-len(head)-len(tail)) + tail)
}

func serveSpoolRoute(router *gin.Engine, path string, body []byte, chunked bool) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+spoolRouteTestKey)
	req.Header.Set("x-goog-api-key", spoolRouteTestKey)
	if chunked {
		req.ContentLength = -1
		req.Body = io.NopCloser(io.MultiReader(bytes.NewReader(body)))
	}
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

func TestGatewayRoutesUnconvertedGeminiAndAntigravityRefuseAboveL(t *testing.T) {
	L := config.DefaultBodySpoolFallbackLimit
	routes := []struct {
		platform, path string
		unmeasured     bool
	}{
		{service.PlatformGemini, "/v1beta/models/blocked-model:generateContent", false},
		// /v1beta/cachedContents is unmeasured (k_conv class "unmeasured"), so
		// it gets a lower limit than L until it is measured. Its boundary is
		// the limit_bytes of its 413; handler TestKConvClasses proves that
		// value is the unmeasured class limit for this cfg and hop.
		{service.PlatformGemini, "/v1beta/cachedContents", true},
		{service.PlatformAntigravity, "/antigravity/v1/messages", false},
		{service.PlatformAntigravity, "/antigravity/v1/messages/count_tokens", false},
		{service.PlatformAntigravity, "/antigravity/v1beta/models/blocked-model:generateContent", false},
	}
	// The reference: an existing unconverted route (/v1/messages, Anthropic group).
	ref := newBodySpoolRouteTestRouter(t, service.PlatformAnthropic)
	refPath := "/v1/messages"
	for _, rt := range routes {
		for _, chunked := range []bool{false, true} {
			name := rt.path + map[bool]string{false: "/content_length", true: "/chunked"}[chunked]
			t.Run(name, func(t *testing.T) {
				router := newBodySpoolRouteTestRouter(t, rt.platform)

				limit := L
				if rt.unmeasured {
					probe := serveSpoolRoute(router, rt.path, spoolRouteBody(int(L)+1, "blocked-model"), chunked)
					require.Equal(t, http.StatusRequestEntityTooLarge, probe.Code, probe.Body.String())
					var refusal map[string]map[string]any
					require.NoError(t, json.Unmarshal(probe.Body.Bytes(), &refusal))
					limit = int64(refusal["error"]["limit_bytes"].(float64))
					require.Less(t, limit, L, "the unmeasured class limit is below L")
					require.Contains(t, refusal["error"]["message"], "its limit is "+strconv.FormatInt(limit, 10)+" bytes")
				}

				under := serveSpoolRoute(router, rt.path, spoolRouteBody(int(limit), "blocked-model"), chunked)
				require.Equal(t, http.StatusNotFound, under.Code, "at the class limit the body passes the ingest and reaches the allowlist: %s", under.Body.String())

				over := serveSpoolRoute(router, rt.path, spoolRouteBody(int(limit)+1, "blocked-model"), chunked)
				require.Equal(t, http.StatusRequestEntityTooLarge, over.Code, over.Body.String())
				want := serveSpoolRoute(ref, refPath, spoolRouteBody(int(L)+1, "blocked-model"), chunked)
				require.Equal(t, http.StatusRequestEntityTooLarge, want.Code, want.Body.String())

				var got, exp map[string]map[string]any
				require.NoError(t, json.Unmarshal(over.Body.Bytes(), &got))
				require.NoError(t, json.Unmarshal(want.Body.Bytes(), &exp))
				msg := strings.Replace(exp["error"]["message"].(string), refPath, rt.path, 1)
				if limit != L { // only the limit and size numbers differ
					msg = strings.Replace(msg, strconv.FormatInt(L+1, 10), strconv.FormatInt(limit+1, 10), 1)
					msg = strings.Replace(msg, strconv.FormatInt(L, 10), strconv.FormatInt(limit, 10), 1)
					exp["error"]["limit_bytes"] = float64(limit)
				}
				exp["error"]["message"] = msg
				require.Equal(t, exp, got, "same status, type, code, hop and message as the existing unconverted-route 413")
				require.Equal(t, "request_too_large", got["error"]["code"])
				require.Equal(t, "sub2api", got["error"]["hop"])
			})
		}
	}
}
