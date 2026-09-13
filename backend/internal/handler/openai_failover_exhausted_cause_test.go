//go:build unit

package handler

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestOpenAIExhaustedCauseBoundary(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := &OpenAIGatewayHandler{}

	t.Run("model_not_found_special_case", func(t *testing.T) {
		recorder := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(recorder)
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)

		h.handleFailoverExhausted(c, &service.UpstreamFailoverError{
			StatusCode:   http.StatusBadRequest,
			ResponseBody: []byte(`{"error":{"code":"model_not_found","message":"Model not found"}}`),
		}, false)

		require.Equal(t, http.StatusBadRequest, recorder.Code)
		require.Equal(t, "model_not_found", gjson.Get(recorder.Body.String(), "error.code").String())
		require.Equal(t, "Model not found", gjson.Get(recorder.Body.String(), "error.message").String())
	})

	t.Run("last_rate_limit_maps_to_native_category", func(t *testing.T) {
		recorder := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(recorder)
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)

		h.handleFailoverExhausted(c, &service.UpstreamFailoverError{
			StatusCode:   http.StatusTooManyRequests,
			ResponseBody: []byte(`{"error":{"code":"rate_limit_exceeded","message":"Per-minute token quota exhausted; retry after the quota reset."}}`),
		}, false)

		require.Equal(t, http.StatusTooManyRequests, recorder.Code)
		t.Logf("wire body: %s", recorder.Body.String())
		require.Equal(t, "rate_limit_error", gjson.Get(recorder.Body.String(), "error.code").String())
		require.Equal(t, "Upstream rate limit exceeded, please retry later", gjson.Get(recorder.Body.String(), "error.message").String())
		require.NotContains(t, recorder.Body.String(), "Per-minute token quota")
	})
}

func TestOpenAIFinalCauseSafetyBoundaries(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := &OpenAIGatewayHandler{}
	tests := []struct {
		name       string
		statusCode int
		body       string
		wantStatus int
		wantCode   string
		wantMsg    string
		forbid     []string
	}{
		{
			name:       "structured_secret_and_internal_url_falls_back",
			statusCode: http.StatusTooManyRequests,
			body:       `{"error":{"code":"rate_limit_exceeded","message":"retry https://10.0.0.7/quota?access_token=secret"}}`,
			wantStatus: http.StatusTooManyRequests,
			wantCode:   "rate_limit_error",
			wantMsg:    "Upstream rate limit exceeded, please retry later",
			forbid:     []string{"secret", "10.0.0.7"},
		},
		{
			name:       "malformed_body_falls_back_without_raw_echo",
			statusCode: http.StatusBadGateway,
			body:       `upstream stack trace token=secret at 10.0.0.8`,
			wantStatus: http.StatusBadGateway,
			wantCode:   "upstream_error",
			wantMsg:    "Upstream service temporarily unavailable",
			forbid:     []string{"stack trace", "secret", "10.0.0.8"},
		},
		{
			name:       "provider_auth_body_keeps_safe_mapping",
			statusCode: http.StatusUnauthorized,
			body:       `{"error":{"code":"invalid_api_key","message":"invalid api_key=provider-secret"}}`,
			wantStatus: http.StatusBadGateway,
			wantCode:   "upstream_error",
			wantMsg:    "Upstream authentication failed, please contact administrator",
			forbid:     []string{"provider-secret", "invalid_api_key"},
		},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			h.handleFailoverExhausted(c, &service.UpstreamFailoverError{StatusCode: testCase.statusCode, ResponseBody: []byte(testCase.body)}, false)
			require.Equal(t, testCase.wantStatus, recorder.Code)
			require.Equal(t, testCase.wantCode, gjson.Get(recorder.Body.String(), "error.code").String())
			require.Equal(t, testCase.wantMsg, gjson.Get(recorder.Body.String(), "error.message").String())
			for _, forbidden := range testCase.forbid {
				require.NotContains(t, recorder.Body.String(), forbidden)
			}
		})
	}
}

func TestOpenAIFinalCauseDoesNotLeakAcrossFreshWriter(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := &OpenAIGatewayHandler{}

	// Actual nil control: a failover error with no body at all must not
	// produce an empty response.
	nilErr := httptest.NewRecorder()
	nilCtx, _ := gin.CreateTestContext(nilErr)
	h.handleFailoverExhausted(nilCtx, nil, false)
	require.Equal(t, http.StatusBadGateway, nilErr.Code)
	require.NotEmpty(t, gjson.Get(nilErr.Body.String(), "error.message").String())
	require.Equal(t, "upstream_error", gjson.Get(nilErr.Body.String(), "error.code").String())

	// Known category first, generic fresh failure second on a clean writer.
	first := httptest.NewRecorder()
	firstCtx, _ := gin.CreateTestContext(first)
	h.handleFailoverExhausted(firstCtx, &service.UpstreamFailoverError{
		StatusCode: http.StatusTooManyRequests, ResponseBody: []byte(`{"error":{"code":"rate_limit_exceeded","message":"first cause"}}`),
	}, false)
	require.Equal(t, "Upstream rate limit exceeded, please retry later", gjson.Get(first.Body.String(), "error.message").String())
	require.Equal(t, "rate_limit_error", gjson.Get(first.Body.String(), "error.code").String())

	fresh := httptest.NewRecorder()
	freshCtx, _ := gin.CreateTestContext(fresh)
	h.handleFailoverExhausted(freshCtx, &service.UpstreamFailoverError{StatusCode: http.StatusBadGateway}, false)
	require.Equal(t, "Upstream service temporarily unavailable", gjson.Get(fresh.Body.String(), "error.message").String())
	require.Equal(t, "upstream_error", gjson.Get(fresh.Body.String(), "error.code").String())
	require.NotContains(t, fresh.Body.String(), "first cause")
}

func TestOpenAIFinalCauseReviewBoundaries(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := &OpenAIGatewayHandler{}
	tests := []struct {
		name      string
		body      string
		forbidden string
	}{
		{
			name:      "credential_in_code",
			body:      `{"error":{"code":"sk-live-synthetic-secret","message":"Temporary request limit"}}`,
			forbidden: "sk-live-synthetic-secret",
		},
		{
			name:      "bare_private_ipv4",
			body:      `{"error":{"code":"rate_limit_exceeded","message":"Backend 10.23.45.67 is throttled"}}`,
			forbidden: "10.23.45.67",
		},
		{
			name:      "bare_private_ipv6",
			body:      `{"error":{"code":"rate_limit_exceeded","message":"Backend fd00:abcd::7 is throttled"}}`,
			forbidden: "fd00:abcd::7",
		},
		{
			name:      "overlay_address",
			body:      `{"error":{"code":"rate_limit_exceeded","message":"Backend 100.64.12.34 is throttled"}}`,
			forbidden: "100.64.12.34",
		},
		{
			name:      "prompt_echo",
			body:      `{"error":{"code":"rate_limit_exceeded","message":"Prompt: synthetic confidential customer instruction"}}`,
			forbidden: "synthetic confidential customer instruction",
		},
		{
			name:      "json_string_is_not_a_structured_error",
			body:      `"synthetic confidential customer instruction"`,
			forbidden: "synthetic confidential customer instruction",
		},
		{
			name: "safe_message_without_code_clears_to_native_category",
			body: `{"error":{"message":"Per-minute token quota exhausted; retry after the quota reset."}}`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
			h.handleFailoverExhausted(c, &service.UpstreamFailoverError{
				StatusCode: http.StatusTooManyRequests, ResponseBody: []byte(tc.body),
			}, false)
			require.Equal(t, http.StatusTooManyRequests, recorder.Code)
			t.Logf("synthetic wire: %s", recorder.Body.String())
			if tc.forbidden != "" {
				require.NotContains(t, recorder.Body.String(), tc.forbidden)
			}
			// Unknown upstream text never becomes client text: the fixed
			// native 429 classification is always the final body.
			require.Equal(t, "rate_limit_error", gjson.Get(recorder.Body.String(), "error.code").String(), "fallback must retain a usable safe classification")
			require.Equal(t, "Upstream rate limit exceeded, please retry later", gjson.Get(recorder.Body.String(), "error.message").String())
		})
	}
}
