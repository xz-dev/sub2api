package service

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestForwardAsChatCompletions_InnerCapacityErrorsPassThrough(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, stream := range []bool{false, true} {
		for _, hop := range []string{"keeper", "aisix", "cpa", "sub2api"} {
			for _, tc := range []struct {
				code   string
				status int
			}{
				{"insufficient_memory", http.StatusServiceUnavailable},
				{"capacity_busy", http.StatusServiceUnavailable},
				{"request_too_large", http.StatusRequestEntityTooLarge},
				{"insufficient_storage", http.StatusInsufficientStorage},
			} {
				t.Run(fmt.Sprintf("%s/%s/stream_%t", tc.code, hop, stream), func(t *testing.T) {
					requestBody := []byte(fmt.Sprintf(`{"model":"gpt-5.4","messages":[{"role":"user","content":"synthetic-prompt-must-not-leak"}],"stream":%t}`, stream))
					errorBody := fmt.Sprintf(`{"error":{"type":"server_error","code":%q,"hop":%q,"limit_bytes":169328640,"message":"capacity refused"}}`, tc.code, hop)
					rec := httptest.NewRecorder()
					c, _ := gin.CreateTestContext(rec)
					c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(requestBody))
					upstream := &httpUpstreamRecorder{responses: []*http.Response{{
						StatusCode: tc.status,
						Header: http.Header{
							"Content-Type": []string{"application/json"},
							"X-Request-Id": []string{"rid-capacity"},
							"Retry-After":  []string{"5"},
						},
						Body: io.NopCloser(strings.NewReader(errorBody)),
					}}}
					cfg := structuredInputRetryTestConfig()
					svc := &OpenAIGatewayService{cfg: cfg, httpUpstream: upstream, responseHeaderFilter: compileResponseHeaderFilter(cfg)}
					account := structuredInputRetryTestAccount()
					account.Extra["openai_responses_mode"] = "force_responses"
					account.Credentials["pool_mode"] = true
					account.Credentials["pool_mode_retry_status_codes"] = []any{float64(tc.status)}

					result, err := svc.ForwardAsChatCompletions(context.Background(), c, account, requestBody, "", "")

					require.Nil(t, result)
					require.Error(t, err)
					var failoverErr *UpstreamFailoverError
					require.False(t, errors.As(err, &failoverErr), "explicit refusal must not trigger account failover or pool retry")
					require.Len(t, upstream.requests, 1)
					require.Equal(t, "/v1/responses", upstream.requests[0].URL.Path)
					require.True(t, IsResponseCommitted(c))
					require.Equal(t, tc.status, rec.Code)
					require.Equal(t, errorBody, rec.Body.String())
					require.Equal(t, "rid-capacity", rec.Header().Get("X-Request-Id"))
					require.Equal(t, "5", rec.Header().Get("Retry-After"))
					require.NotContains(t, rec.Body.String(), "synthetic-prompt-must-not-leak")
					require.NotContains(t, rec.Body.String(), "sk-test")
				})
			}
		}
	}
}

func TestForwardAsChatCompletions_InnerCapacityErrorsRequireCompleteNamedEnvelope(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const named = `{"error":{"code":"insufficient_memory","hop":"keeper","limit_bytes":169328640,"message":"capacity refused"}}`
	limit := int(openAIUpstreamErrorBodyReadLimitForConfig(structuredInputRetryTestConfig()))
	for _, tc := range []struct {
		name    string
		body    string
		status  int
		readErr error
	}{
		{name: "ordinary OpenAI error", body: `{"error":{"type":"invalid_request_error","code":"invalid_value","message":"bad input","private_detail":"not-for-client"}}`},
		{name: "no hop", body: strings.Replace(named, `"hop":"keeper",`, "", 1)},
		{name: "plain provider 413", status: http.StatusRequestEntityTooLarge, body: `{"error":{"type":"invalid_request_error","code":"request_too_large","message":"provider limit","private_detail":"not-for-client"}}`},
		{name: "empty hop", body: strings.Replace(named, `"keeper"`, `"  "`, 1)},
		{name: "nonstring hop", body: strings.Replace(named, `"keeper"`, `42`, 1)},
		{name: "unknown code", body: strings.Replace(named, "insufficient_memory", "provider_failure", 1)},
		{name: "truncated JSON", body: named[:len(named)-1]},
		{name: "trailing data", body: named + " trailing"},
		{name: "valid prefix exceeds cap", body: named + strings.Repeat(" ", limit-len(named)+1)},
		{name: "discarded suffix after valid prefix", body: named + strings.Repeat(" ", limit-len(named)) + " trailing"},
		{name: "read failure after complete JSON", body: named, readErr: io.ErrUnexpectedEOF},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requestBody := []byte(`{"model":"gpt-5.4","messages":[{"role":"user","content":"hello"}]}`)
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(requestBody))
			errorBody := io.NopCloser(strings.NewReader(tc.body))
			if tc.readErr != nil {
				errorBody = &openAIChatStreamReadErrorCloser{payload: []byte(tc.body), err: tc.readErr}
			}
			status := tc.status
			if status == 0 {
				status = http.StatusBadRequest
			}
			upstream := &httpUpstreamRecorder{responses: []*http.Response{{StatusCode: status, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: errorBody}}}
			svc := &OpenAIGatewayService{cfg: structuredInputRetryTestConfig(), httpUpstream: upstream}

			result, err := svc.ForwardAsChatCompletions(context.Background(), c, structuredInputRetryTestAccount(), requestBody, "", "")

			require.Nil(t, result)
			require.Error(t, err)
			require.Len(t, upstream.requests, 1)
			require.Equal(t, status, rec.Code)
			errType := "invalid_request_error"
			if status == http.StatusRequestEntityTooLarge {
				errType = "api_error"
			}
			require.Equal(t, errType, gjson.Get(rec.Body.String(), "error.type").String())
			for _, field := range []string{"code", "hop", "limit_bytes", "private_detail"} {
				require.False(t, gjson.Get(rec.Body.String(), "error."+field).Exists(), "nonmatching envelopes retain the old compatibility writer")
			}
			if tc.name == "ordinary OpenAI error" {
				require.Equal(t, "bad input", gjson.Get(rec.Body.String(), "error.message").String())
				require.NotContains(t, rec.Body.String(), "not-for-client")
			}
		})
	}
}

func TestForwardAsChatCompletions_InnerCapacityErrorKeepsCredentialRedaction(t *testing.T) {
	key, privateKey := newTestAgentIdentityKey(t)
	account := &Account{
		ID: 25, Platform: PlatformOpenAI, Type: AccountTypeOAuth,
		Credentials: map[string]any{
			"auth_mode": OpenAIAuthModeAgentIdentity, "agent_runtime_id": key.runtimeID,
			"agent_private_key": privateKey, "task_id": key.taskID,
		},
	}
	errorBody := fmt.Sprintf(`{"error":{"code":"insufficient_memory","hop":"keeper","limit_bytes":169328640,"message":"%s %s AgentAssertion synthetic-assertion"}}`, key.runtimeID, key.taskID)
	upstream := &httpUpstreamRecorder{responses: []*http.Response{{
		StatusCode: http.StatusServiceUnavailable,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(errorBody)),
	}}}
	svc := &OpenAIGatewayService{cfg: structuredInputRetryTestConfig(), httpUpstream: upstream}
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	requestBody := []byte(`{"model":"gpt-5.4","messages":[{"role":"user","content":"synthetic"}]}`)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(requestBody))

	_, err := svc.ForwardAsChatCompletions(context.Background(), c, account, requestBody, "", "")

	require.Error(t, err)
	require.Len(t, upstream.requests, 1)
	require.Equal(t, http.StatusServiceUnavailable, rec.Code)
	require.Equal(t, "insufficient_memory", gjson.Get(rec.Body.String(), "error.code").String())
	require.Equal(t, "keeper", gjson.Get(rec.Body.String(), "error.hop").String())
	require.Equal(t, int64(169328640), gjson.Get(rec.Body.String(), "error.limit_bytes").Int())
	for _, secret := range []string{key.runtimeID, key.taskID, "synthetic-assertion"} {
		require.NotContains(t, rec.Body.String(), secret)
		require.NotContains(t, err.Error(), secret)
	}
	require.Contains(t, rec.Body.String(), "[redacted]")
}

// Classification scans the existing capped error bytes, not the request JSON.
func BenchmarkChatInnerCapacityErrorClassification(b *testing.B) {
	const named = `{"error":{"code":"insufficient_memory","hop":"keeper","limit_bytes":169328640,"message":"capacity refused"}}`
	for _, size := range []int{len(named), int(openAIUpstreamErrorBodyReadLimitForConfig(structuredInputRetryTestConfig()))} {
		body := []byte(named + strings.Repeat(" ", size-len(named)))
		b.Run(fmt.Sprintf("%d_bytes", size), func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				if !isInnerGatewayCapacityError(body) {
					b.Fatal("expected named capacity error")
				}
			}
		})
	}
}
