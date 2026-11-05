package service

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// Every provider 413 is request-scoped (large-request-capacity D5, user
// decision): even with pool-mode retry configured for 413 it is not failed
// over, and status, headers and body reach the client unchanged.
func TestOpenAIRequestBodyLimit_HTTP413IsRequestScopedPassthrough(t *testing.T) {
	gin.SetMode(gin.TestMode)
	requestBody := []byte(`{"model":"gpt-5.2","stream":false,"input":"hello"}`)

	for _, passthrough := range []bool{false, true} {
		name := "native_responses"
		if passthrough {
			name = "api_key_passthrough"
		}
		t.Run(name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(nil))

			const upstreamBody = `{"error":{"message":"request body exceeds this account's 16MB proxy limit; secret=must-not-leak","type":"invalid_request_error"}}`
			body := &passthroughCloseTrackingReadCloser{Reader: strings.NewReader(upstreamBody)}
			upstream := &httpUpstreamRecorder{resp: &http.Response{
				StatusCode: http.StatusRequestEntityTooLarge,
				Header: http.Header{
					"Content-Type": []string{"application/json"},
					"X-Request-Id": []string{"rid-body-limit"},
				},
				Body: body,
			}}
			cfg := &config.Config{Gateway: config.GatewayConfig{ForceCodexCLI: false}}
			svc := &OpenAIGatewayService{
				cfg:                  cfg,
				httpUpstream:         upstream,
				responseHeaderFilter: compileResponseHeaderFilter(cfg),
			}
			account := &Account{
				ID:          161,
				Name:        name,
				Platform:    PlatformOpenAI,
				Type:        AccountTypeAPIKey,
				Concurrency: 1,
				Credentials: map[string]any{
					"api_key":   "sk-test",
					"base_url":  "https://api.example.test",
					"pool_mode": true,
					"pool_mode_retry_status_codes": []any{
						float64(http.StatusRequestEntityTooLarge),
					},
				},
				Extra: map[string]any{
					"openai_passthrough":         passthrough,
					"openai_responses_supported": true,
				},
				Status:      StatusActive,
				Schedulable: true,
			}

			result, err := svc.Forward(context.Background(), c, account, requestBody)

			require.Nil(t, result)
			require.Error(t, err)
			var failoverErr *UpstreamFailoverError
			require.False(t, errors.As(err, &failoverErr), "a provider 413 is never failed over")
			require.True(t, c.Writer.Written())
			require.Equal(t, http.StatusRequestEntityTooLarge, rec.Code)
			require.Equal(t, upstreamBody, rec.Body.String())
			require.Equal(t, "rid-body-limit", rec.Header().Get("X-Request-Id"))
			require.True(t, body.closed)
			if passthrough {
				require.Equal(t, requestBody, upstream.lastBody)
			} else {
				require.Equal(t, "gpt-5.2", gjson.GetBytes(upstream.lastBody, "model").String())
				require.Equal(t, "hello", gjson.GetBytes(upstream.lastBody, "input").String())
			}
		})
	}
}

func TestOpenAIRequestBodyLimitFailover_ContextWindow413DoesNotSwitchAccounts(t *testing.T) {
	gin.SetMode(gin.TestMode)
	requestBody := []byte(`{"model":"gpt-5.2","stream":false,"input":"hello"}`)

	for _, passthrough := range []bool{false, true} {
		t.Run(fmt.Sprintf("passthrough_%t", passthrough), func(t *testing.T) {
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(nil))

			const upstreamBody = `{"error":{"message":"Your input exceeds the context window of this model. Please adjust your input and try again.","type":"invalid_request_error"}}`
			body := &passthroughCloseTrackingReadCloser{Reader: strings.NewReader(upstreamBody)}
			svc := &OpenAIGatewayService{
				cfg: &config.Config{Gateway: config.GatewayConfig{ForceCodexCLI: false}},
				httpUpstream: &httpUpstreamRecorder{resp: &http.Response{
					StatusCode: http.StatusRequestEntityTooLarge,
					Header:     http.Header{"Content-Type": []string{"application/json"}},
					Body:       body,
				}},
			}
			account := &Account{
				ID: 162, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Concurrency: 1,
				Credentials: map[string]any{"api_key": "sk-test", "base_url": "https://api.example.test"},
				Extra: map[string]any{
					"openai_passthrough":         passthrough,
					"openai_responses_supported": true,
				},
				Status: StatusActive, Schedulable: true,
			}

			result, err := svc.Forward(context.Background(), c, account, requestBody)

			require.Nil(t, result)
			require.Error(t, err)
			var failoverErr *UpstreamFailoverError
			require.False(t, errors.As(err, &failoverErr), "context-window failures are deterministic request errors")
			require.True(t, c.Writer.Written())
			require.Contains(t, rec.Body.String(), "exceeds the context window")
			require.True(t, body.closed)
		})
	}
}
