//go:build unit

package handler

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// TestReleaseLargeRequestStateDropsPooledReferences: gin keeps a finished
// request's Context in a pool, so its Keys and Request kept the skeleton and
// the parsed copies reachable after the request. After a large request (the
// charge was at least bodyspool.ReclaimAfterRelease) the outermost middleware
// drops them and returns the freed heap once; a small request is untouched
// and does not reclaim.
func TestReleaseLargeRequestStateDropsPooledReferences(t *testing.T) {
	gin.SetMode(gin.TestMode)
	upstream := &largeBodyDrainUpstream{}
	h := newLargeBodyTestHandler(t, upstream, "oauth")
	enableBodySpoolForTest(t, h.cfg, true, 320<<20, nil)
	rt := bodySpoolRuntimeFor(h.cfg)
	require.NoError(t, rt.err)
	runs := make(chan struct{}, 4)
	rt.meter.ReleaseReclaim = func() { runs <- struct{}{} }

	groupID := int64(3131)
	var seen *gin.Context
	r := gin.New()
	r.Use(ReleaseLargeRequestState(h.cfg))
	r.POST("/v1/responses", func(c *gin.Context) {
		seen = c
		c.Set(string(middleware2.ContextKeyAPIKey), &service.APIKey{
			ID: 99, GroupID: &groupID,
			Group: &service.Group{ID: groupID, Platform: service.PlatformOpenAI},
			User:  &service.User{ID: 100},
		})
		c.Set(string(middleware2.ContextKeyUser), middleware2.AuthSubject{UserID: 100})
	}, BodySpoolIngest(h.cfg), h.Responses)

	send := func(body []byte) {
		t.Helper()
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		r.ServeHTTP(rec, req)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	}

	send(largeResponsesTextBody(64 << 10)) // charge 14 × 64 KiB < 16 MiB
	require.NotNil(t, seen.Keys, "a small request keeps its Keys")
	_, ok := seen.Get(bodySpoolStateKey)
	require.True(t, ok)
	select {
	case <-runs:
		t.Fatal("a small request reclaimed")
	case <-time.After(100 * time.Millisecond):
	}

	send(largeResponsesTextBody(4 << 20)) // charge 14 × 4 MiB
	require.Nil(t, seen.Keys, "the pooled Context still holds the large request's Keys")
	require.Equal(t, http.NoBody, seen.Request.Body, "the pooled Context still holds the skeleton as Body")
	require.Equal(t, context.Background(), seen.Request.Context(), "the pooled Context still holds the request context (spool, ticket)")
	select {
	case <-runs:
	case <-time.After(5 * time.Second):
		t.Fatal("a large request did not reclaim")
	}
	require.Zero(t, rt.meter.Outstanding())
	require.EqualValues(t, 2, upstream.calls.Load())
}
