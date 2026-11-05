package service

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/bodyspool"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func spoolForTest(t *testing.T, body []byte) (*bodyspool.Spool, []byte) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "spool")
	require.NoError(t, os.Mkdir(p, 0o700))
	d, err := bodyspool.OpenDir(p, 1)
	require.NoError(t, err)
	f, err := d.Create()
	require.NoError(t, err)
	sp, skel, err := bodyspool.Ingest(bytes.NewReader(body), f, bodyspool.Options{})
	require.NoError(t, err)
	t.Cleanup(func() { _ = sp.Close() })
	require.Positive(t, sp.Blobs())
	return sp, skel
}

func ginWithSpool(sp *bodyspool.Spool, ticket string) *gin.Context {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	if sp != nil {
		c.Request = c.Request.WithContext(WithRequestBodySpool(context.Background(), sp, ticket))
	}
	return c
}

// TestSessionHashOverSpliceStream: the content seed taken from the skeleton
// hashes exactly like the legacy body (current and legacy hash).
func TestSessionHashOverSpliceStream(t *testing.T) {
	gin.SetMode(gin.TestMode)
	img := strings.Repeat("QUJD", 40000)
	for name, body := range map[string]string{
		"first_user_content": `{"model":"gpt-5.1","input":[{"role":"user","content":[{"type":"input_text","text":"hi"},{"type":"input_image","image_url":"data:image/png;base64,` + img + `"}]}]}`,
		"input_string_url":   `{"model":"gpt-5.1","input":[{"type":"input_text","text":"x"}],"tools":[{"type":"function","name":"f","image_url":"data:image/png;base64,` + img + `"}]}`,
	} {
		sp, skel := spoolForTest(t, []byte(body))
		svc := &OpenAIGatewayService{}
		legacyCtx, spoolCtx := ginWithSpool(nil, ""), ginWithSpool(sp, "")
		want := svc.GenerateSessionHash(legacyCtx, []byte(body))
		require.NotEmpty(t, want, name)
		require.Equal(t, want, svc.GenerateSessionHash(spoolCtx, skel), name)
		require.Equal(t, openAILegacySessionHashFromContext(legacyCtx.Request.Context()), openAILegacySessionHashFromContext(spoolCtx.Request.Context()), name)
		require.Equal(t, len(body), RequestBodyLogicalSize(spoolCtx, skel))
	}
}

// TestCapacityTicketHeaderForwardedOnlyToInternalUpstream: the ticket goes
// to a configured apisix-models host on a direct connection, and is
// stripped for any other host and on a proxied (Squid relay) account.
func TestCapacityTicketHeaderForwardedOnlyToInternalUpstream(t *testing.T) {
	const ticket = "0123456789abcdef0123456789abcdef"
	cfg := &config.Config{}
	cfg.Gateway.BodySpool.TicketForwardHosts = []string{"apisix-models:9080"}
	svc := &OpenAIGatewayService{cfg: cfg}
	sp, skel := spoolForTest(t, []byte(`{"input":[{"image_url":"data:image/png;base64,`+strings.Repeat("A", 70000)+`"}]}`))
	proxyID := int64(7)
	for name, tc := range map[string]struct {
		url     string
		account *Account
		want    string
	}{
		"internal_direct": {"http://apisix-models:9080/v1/responses", &Account{}, ticket},
		"other_host":      {"https://chatgpt.com/backend-api/codex/responses", &Account{}, ""},
		"squid_relay":     {"http://apisix-models:9080/v1/responses", &Account{ProxyID: &proxyID, Proxy: &Proxy{}}, ""},
	} {
		c := ginWithSpool(sp, ticket)
		req, err := newOpenAIUpstreamBodyRequest(c.Request.Context(), c, http.MethodPost, tc.url, skel)
		require.NoError(t, err)
		req.Header.Set(bodyspool.TicketHeader, "client-forged")
		svc.applyCapacityTicketHeader(tc.account, req)
		require.Equal(t, tc.want, req.Header.Get(bodyspool.TicketHeader), name)
	}
}

// TestSpoolEgressFailsClosed: an unresolved marker answers 500 with nothing
// sent, and a spooled request whose body skipped the splicer never leaves.
func TestSpoolEgressFailsClosed(t *testing.T) {
	gin.SetMode(gin.TestMode)
	sp, skel := spoolForTest(t, []byte(`{"input":[{"image_url":"data:image/png;base64,`+strings.Repeat("A", 70000)+`"}]}`))
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil).WithContext(WithRequestBodySpool(context.Background(), sp, ""))
	mangled := bytes.Replace(skel, []byte(`\u0000lrc`), []byte(`\\u0000lrc`), 1)
	_, err := newOpenAIUpstreamBodyRequest(c.Request.Context(), c, http.MethodPost, "http://x/v1/responses", mangled)
	require.ErrorIs(t, err, bodyspool.ErrMarkerUnresolved)
	require.Equal(t, http.StatusInternalServerError, rec.Code)
	require.Contains(t, rec.Body.String(), `"marker_unresolved"`)

	raw, err := http.NewRequestWithContext(c.Request.Context(), http.MethodPost, "http://x/v1/responses", bytes.NewReader(skel))
	require.NoError(t, err)
	require.ErrorIs(t, checkSpoolEgress(raw), errSpoolEgressUnspliced)

	// The egress refusal is request-scoped on every builder: a named 422,
	// no failover error, no upstream error recorded against the account.
	rec = httptest.NewRecorder()
	c, _ = gin.CreateTestContext(rec)
	c.Request = raw
	svc := &OpenAIGatewayService{}
	_, err = svc.doOpenAIUpstream(raw, "", &Account{ID: 7, Platform: PlatformOpenAI})
	require.ErrorIs(t, err, errSpoolEgressUnspliced)
	err = svc.handleOpenAIUpstreamTransportError(raw.Context(), c, &Account{ID: 7, Platform: PlatformOpenAI}, err, false)
	var failover *UpstreamFailoverError
	require.False(t, errors.As(err, &failover), "%v", err)
	require.Equal(t, http.StatusUnprocessableEntity, rec.Code)
	require.Contains(t, rec.Body.String(), `"code":"spooled_body_unsupported"`)
	require.Contains(t, rec.Body.String(), `"hop":"sub2api"`)
	_, recorded := c.Get(OpsUpstreamErrorsKey)
	require.False(t, recorded)
}

// TestCCEgressForwardsTicketAndSplices: the Chat Completions egress (the
// Responses->Chat path) splices the blobs back, forwards the leased ticket
// to a configured internal host only, and answers an unresolved marker with
// the named 500 instead of a bare build error.
func TestCCEgressForwardsTicketAndSplices(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const ticket = "0123456789abcdef0123456789abcdef"
	body := []byte(`{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,` + strings.Repeat("A", 70000) + `"}}]}]}`)
	sp, skel := spoolForTest(t, body)
	cfg := &config.Config{}
	cfg.Gateway.BodySpool.TicketForwardHosts = []string{"apisix-models:9080"}
	for name, tc := range map[string]struct{ url, want string }{
		"internal": {"http://apisix-models:9080/v1/chat/completions", ticket},
		"other":    {"https://api.example.com/v1/chat/completions", ""},
	} {
		upstream := &httpUpstreamRecorder{resp: &http.Response{StatusCode: http.StatusOK, Body: http.NoBody, Header: http.Header{}}}
		svc := &OpenAIGatewayService{cfg: cfg, httpUpstream: upstream}
		c := ginWithSpool(sp, ticket)
		c.Request.Header.Set(bodyspool.TicketHeader, "client-forged")
		_, err := svc.sendCCUpstreamRequest(c.Request.Context(), c, &Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeAPIKey}, tc.url, skel, false, "tok", "", "")
		require.NoError(t, err, name)
		require.Equal(t, tc.want, upstream.lastReq.Header.Get(bodyspool.TicketHeader), name)
		require.Equal(t, body, upstream.lastBody, name)
	}

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil).WithContext(WithRequestBodySpool(context.Background(), sp, ""))
	upstream := &httpUpstreamRecorder{}
	svc := &OpenAIGatewayService{cfg: cfg, httpUpstream: upstream}
	mangled := bytes.Replace(skel, []byte(`\u0000lrc`), []byte(`\\u0000lrc`), 1)
	_, err := svc.sendCCUpstreamRequest(c.Request.Context(), c, &Account{ID: 1}, "http://apisix-models:9080/v1/chat/completions", mangled, false, "tok", "", "")
	require.ErrorIs(t, err, bodyspool.ErrMarkerUnresolved)
	require.Equal(t, http.StatusInternalServerError, rec.Code)
	require.Contains(t, rec.Body.String(), `"marker_unresolved"`)
	require.Nil(t, upstream.lastReq)
}
