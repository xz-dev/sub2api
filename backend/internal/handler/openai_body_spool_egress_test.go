//go:build unit

package handler

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// Body spool egress (review major 2): only some request builders splice.
// A body without blobs carries no spool, so every builder works; a body with
// blobs on a builder that cannot splice gets a named, request-scoped refusal
// on its one attempt: nothing is sent, no failover.

// protocolUpstream answers Anthropic /v1/messages and OpenAI Responses.
type protocolUpstream struct {
	service.HTTPUpstream
	mu       sync.Mutex
	urls     []string
	accounts []int64
	bodies   [][]byte
}

func (u *protocolUpstream) Do(req *http.Request, _ string, accountID int64, _ int) (*http.Response, error) {
	b, err := io.ReadAll(req.Body)
	if err != nil {
		return nil, err
	}
	u.mu.Lock()
	u.urls = append(u.urls, req.URL.String())
	u.accounts = append(u.accounts, accountID)
	u.bodies = append(u.bodies, b)
	u.mu.Unlock()
	if strings.HasSuffix(req.URL.Path, "/v1/messages") {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       io.NopCloser(bytes.NewBufferString(anthropicTestSSE)),
		}, nil
	}
	return (&largeBodyDrainUpstream{}).Do(&http.Request{}, "", 0, 0)
}

const anthropicTestSSE = "event: message_start\n" +
	`data: {"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"claude-sonnet-4-6","content":[],"stop_reason":null,"usage":{"input_tokens":3,"output_tokens":0}}}` + "\n\n" +
	"event: content_block_start\n" + `data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}` + "\n\n" +
	"event: content_block_delta\n" + `data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"ok"}}` + "\n\n" +
	"event: content_block_stop\n" + `data: {"type":"content_block_stop","index":0}` + "\n\n" +
	"event: message_delta\n" + `data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":1}}` + "\n\n" +
	"event: message_stop\n" + `data: {"type":"message_stop"}` + "\n\n"

func (u *protocolUpstream) calls() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return len(u.urls)
}

// anthropicProtocolAccount resolves /v1/responses to the native Anthropic
// builder, which does not splice.
func anthropicProtocolAccount(id int64, priority int) service.Account {
	return service.Account{
		ID: id, Name: "anthropic-protocol", Platform: service.PlatformZhipu, Type: service.AccountTypeAPIKey,
		Status: service.StatusActive, Schedulable: true, Priority: priority, Concurrency: 1,
		Credentials: map[string]any{"api_key": "sk-test", "api_protocol": service.APIProtocolAnthropic, "base_url": "https://anthropic.example"},
	}
}

// forwardWithIngest runs the production ingest on body and then forwards the
// request on account, as one attempt of the Responses handler does.
// Covered requests come from OpenAI groups, whose scheduler never picks a
// multi-protocol account today; calling Forward directly reaches the
// native Anthropic builder with the spool state the handler attached.
func forwardWithIngest(t *testing.T, upstream *protocolUpstream, account service.Account, body []byte) (*httptest.ResponseRecorder, error) {
	t.Helper()
	h := newLargeBodyTestHandlerWithAccounts(t, upstream, []service.Account{account})
	enableBodySpoolForTest(t, h.cfg, true, 4<<30, nil)
	c, rec := newLargeBodyTestContext("/v1/responses", body, "")
	require.True(t, ingestRequestBody(c, h.cfg), rec.Body.String())
	defer releaseRequestBodySpool(c)
	reqBody, err := io.ReadAll(c.Request.Body)
	require.NoError(t, err)
	_, err = h.gatewayService.Forward(c.Request.Context(), c, &account, reqBody)
	return rec, err
}

// TestBodySpoolTextRequestReachesNativeAnthropic: a 51-byte text request has
// no blob, so it carries no spool and the native Anthropic builder sends it.
func TestBodySpoolTextRequestReachesNativeAnthropic(t *testing.T) {
	gin.SetMode(gin.TestMode)
	body := []byte(`{"model":"claude-sonnet-4-6","input":"hello world"}`)
	require.Len(t, body, 51)
	upstream := &protocolUpstream{}
	rec, err := forwardWithIngest(t, upstream, anthropicProtocolAccount(1, 0), body)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, 1, upstream.calls())
	require.True(t, strings.HasSuffix(upstream.urls[0], "/v1/messages"), upstream.urls[0])
	require.Contains(t, string(upstream.bodies[0]), "hello world")
}

// TestBodySpoolBlobOnUnsplicedBuilderIsRequestScoped: a body with a spooled
// image on the native Anthropic builder gets the named refusal on its one
// attempt: nothing is sent, the error is not a failover error, and no
// upstream error is recorded against the account.
func TestBodySpoolBlobOnUnsplicedBuilderIsRequestScoped(t *testing.T) {
	gin.SetMode(gin.TestMode)
	body := bytes.Replace(largeResponsesImagesBody(256<<10), []byte(`"gpt-5.1"`), []byte(`"claude-sonnet-4-6"`), 1)
	upstream := &protocolUpstream{}
	rec, err := forwardWithIngest(t, upstream, anthropicProtocolAccount(1, 0), body)
	require.Error(t, err)
	var failover *service.UpstreamFailoverError
	require.False(t, errors.As(err, &failover), "must not fail over: %v", err)
	requireCapacityError(t, rec, http.StatusUnprocessableEntity, "spooled_body_unsupported", "sub2api", "nothing was sent")
	require.Zero(t, upstream.calls())
}
