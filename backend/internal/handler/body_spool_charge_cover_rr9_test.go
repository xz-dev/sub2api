//go:build unit

package handler

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/Wei-Shaw/sub2api/internal/testutil"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// rr9 reviewer probe: a content-seed or transcript decode on the raw path
// must be bounded in bytes, not only in JSON values. The probe is one user
// message whose content is a single string of about the body size with
// \n, \" and \u00e9 escapes, with no session header and no
// prompt_cache_key (so the content session seed decodes the first user
// message). It runs on every Responses combo; the coverSessionBlockPrefix
// combos run it again with the cyber session block on (transcript keys).
const (
	coverProbeEscapedUser   = "probe_escaped_user_string"
	coverSessionBlockPrefix = "sblock-"
)

func coverEscapedUserStringBody(model string, size int) []byte {
	const unit = `line \"quoted\" caf\u00e9 text\n`
	tail := `"}]}`
	var b bytes.Buffer
	b.Grow(size + len(unit))
	b.WriteString(`{"model":"` + model + `","stream":false,"store":false,"input":[{"role":"user","content":"`)
	for b.Len()+len(unit)+len(tail) <= size {
		b.WriteString(unit)
	}
	b.WriteString(tail)
	return b.Bytes()
}

// newCoverHandler builds the combo's handler; sessionBlock wires the cyber
// session block on (coverSessionBlockDeps).
func newCoverHandler(t *testing.T, up *coverUpstream, accounts []service.Account, sessionBlock bool) *OpenAIGatewayHandler {
	t.Helper()
	if !sessionBlock {
		return newLargeBodyTestHandlerWithAccounts(t, up, accounts)
	}
	cache, settings := coverSessionBlockDeps(t, up)
	return newLargeBodyTestHandlerWithDeps(t, up, accounts, cache, settings)
}

// coverSessionBlockDeps: a miniredis gateway cache in which the scope of
// the test client (API key 99, httptest's remote address, no User-Agent)
// is active, and a setting service with the session block on, so every
// request derives its transcript keys. up.blockScopeHits counts the
// requests that did.
func coverSessionBlockDeps(t *testing.T, up *coverUpstream) (service.GatewayCache, *service.SettingService) {
	t.Helper()
	inner := testutil.NewRedisGatewayCache(t)
	store, ok := inner.(service.CyberSessionBlockStore)
	require.True(t, ok)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	scope := cyberSessionScopeKey(99, c)
	require.NotEmpty(t, scope)
	require.NoError(t, store.SetCyberSessionBlocked(context.Background(), scope, []string{"cover-unrelated-block"}, time.Hour))
	settings := service.NewSettingService(&contentModerationHandlerSettingRepo{values: map[string]string{
		service.SettingKeyCyberSessionBlockEnabled:    "true",
		service.SettingKeyCyberSessionBlockTTLSeconds: "3600",
	}}, nil)
	return coverBlockCache{GatewayCache: inner, store: store, hits: &up.blockScopeHits}, settings
}

// coverBlockCache is the gateway cache with its session-block store; an
// active-scope answer is counted.
type coverBlockCache struct {
	service.GatewayCache
	store service.CyberSessionBlockStore
	hits  *atomic.Int32
}

func (c coverBlockCache) SetCyberSessionBlocked(ctx context.Context, scopeKey string, keys []string, ttl time.Duration) error {
	return c.store.SetCyberSessionBlocked(ctx, scopeKey, keys, ttl)
}

func (c coverBlockCache) IsCyberSessionScopeActive(ctx context.Context, scopeKey string) (bool, error) {
	active, err := c.store.IsCyberSessionScopeActive(ctx, scopeKey)
	if active {
		c.hits.Add(1)
	}
	return active, err
}

func (c coverBlockCache) FindCyberSessionBlocked(ctx context.Context, keys []string) (string, error) {
	return c.store.FindCyberSessionBlocked(ctx, keys)
}
