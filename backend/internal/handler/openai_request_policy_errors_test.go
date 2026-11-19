//go:build unit

package handler

import (
	"context"
	"fmt"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// A recognized upstream content-policy rejection is request-scoped: one
// upstream attempt even with pool retries and an alternate account available,
// no account-state mutation, and the safe policy code/type reach the client on
// both Responses and Chat Completions.

const policyFixedMessage = "Request blocked by upstream content policy."

// policyRecordingAccountRepo counts every account-state mutation the error
// paths can reach (disable, schedulability, rate limits, cooldowns, extra).
type policyRecordingAccountRepo struct {
	openAIImagesFailoverAccountRepo
	mutations atomic.Int32
}

func (r *policyRecordingAccountRepo) SetError(context.Context, int64, string) error {
	r.mutations.Add(1)
	return nil
}

func (r *policyRecordingAccountRepo) SetSchedulable(context.Context, int64, bool) error {
	r.mutations.Add(1)
	return nil
}

func (r *policyRecordingAccountRepo) SetRateLimited(context.Context, int64, time.Time) error {
	r.mutations.Add(1)
	return nil
}

func (r *policyRecordingAccountRepo) SetModelRateLimit(context.Context, int64, string, time.Time, ...string) error {
	r.mutations.Add(1)
	return nil
}

func (r *policyRecordingAccountRepo) SetOverloaded(context.Context, int64, time.Time) error {
	r.mutations.Add(1)
	return nil
}

func (r *policyRecordingAccountRepo) SetTempUnschedulable(context.Context, int64, time.Time, string) error {
	r.mutations.Add(1)
	return nil
}

func (r *policyRecordingAccountRepo) UpdateExtra(context.Context, int64, map[string]any) error {
	r.mutations.Add(1)
	return nil
}

func newPolicyTestHandler(t *testing.T, upstream service.HTTPUpstream, accounts []service.Account) (*OpenAIGatewayHandler, *policyRecordingAccountRepo) {
	t.Helper()
	cfg := &config.Config{RunMode: config.RunModeSimple}
	cfg.Gateway.MaxBodySize = 256 << 20
	repo := &policyRecordingAccountRepo{openAIImagesFailoverAccountRepo: openAIImagesFailoverAccountRepo{accounts: accounts}}
	rateLimitService := service.NewRateLimitService(repo, nil, cfg, nil, nil)
	gatewayService := service.NewOpenAIGatewayService(
		repo,
		nil, nil, nil, nil, nil, nil, cfg, nil, nil, nil, rateLimitService, nil,
		upstream,
		nil, nil, nil, nil, nil, nil, nil, nil,
	)
	billingService := service.NewBillingCacheService(nil, nil, nil, nil, nil, nil, cfg, nil)
	t.Cleanup(billingService.Stop)
	handler := NewOpenAIGatewayHandler(
		gatewayService,
		service.NewConcurrencyService(nil),
		billingService,
		service.NewAPIKeyService(nil, nil, nil, nil, nil, nil, cfg),
		nil, nil, nil, nil,
		cfg,
	)
	handler.maxAccountSwitches = 10
	return handler, repo
}

// policyTestAccounts returns two accounts with pool-mode same-account retries
// enabled for status, so an accidental retry or switch is observable.
func policyTestAccounts(accountType string, status int) []service.Account {
	accounts := largeBodyTestAccounts(accountType)
	for i := range accounts {
		accounts[i].Credentials["pool_mode"] = true
		accounts[i].Credentials["pool_mode_retry_status_codes"] = []any{float64(status)}
	}
	return accounts
}

var policyErrorCases = []struct {
	name     string
	body     string
	wantCode string
	wantType string
}{
	{
		name:     "code_and_safe_type",
		body:     `{"error":{"type":"permission_error","code":"content_policy_violation","message":"synthetic upstream detail from 10.0.0.7"}}`,
		wantCode: "content_policy_violation",
		wantType: "permission_error",
	},
	{
		name:     "type_only",
		body:     `{"error":{"type":"content_moderation","message":"synthetic upstream detail from 10.0.0.7"}}`,
		wantCode: "content_moderation",
		wantType: "content_moderation",
	},
}

func requirePolicyClientError(t *testing.T, status int, wantCode, wantType string, code int, body string) {
	t.Helper()
	require.Equal(t, status, code, "original policy status must survive")
	require.Equal(t, wantCode, gjson.Get(body, "error.code").String())
	require.Equal(t, wantType, gjson.Get(body, "error.type").String())
	require.Equal(t, policyFixedMessage, gjson.Get(body, "error.message").String())
	require.NotContains(t, body, "synthetic upstream detail", "upstream text must not reach the client")
	require.NotContains(t, body, "10.0.0.7")
}

func TestOpenAIResponsesPolicyRejectionIsRequestScoped(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, status := range []int{http.StatusBadRequest, http.StatusForbidden} {
		for _, accountType := range []string{"oauth", "apikey", "apikey_passthrough", "apikey_chat"} {
			for _, tc := range policyErrorCases {
				t.Run(fmt.Sprintf("%d/%s/%s", status, accountType, tc.name), func(t *testing.T) {
					upstream := &scriptedErrorUpstream{
						status: status,
						header: http.Header{"Content-Type": []string{"application/json"}},
						body:   tc.body,
					}
					h, repo := newPolicyTestHandler(t, upstream, policyTestAccounts(accountType, status))
					c, rec := newLargeBodyTestContext("/v1/responses", largeResponsesTextBody(1<<10), "")
					h.Responses(c)
					require.Equal(t, int32(1), upstream.calls.Load(), "no same-account retry or account switch")
					require.Zero(t, repo.mutations.Load(), "no account health, cooldown or disable side effect")
					require.Contains(t, rec.Header().Get("Content-Type"), "application/json")
					requirePolicyClientError(t, status, tc.wantCode, tc.wantType, rec.Code, rec.Body.String())
				})
			}
		}
	}
}

func TestOpenAIChatPolicyRejectionIsRequestScoped(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, status := range []int{http.StatusBadRequest, http.StatusForbidden} {
		for _, accountType := range []string{"apikey", "apikey_chat"} {
			for _, stream := range []bool{false, true} {
				for _, tc := range policyErrorCases {
					t.Run(fmt.Sprintf("%d/%s/stream_%t/%s", status, accountType, stream, tc.name), func(t *testing.T) {
						upstream := &scriptedErrorUpstream{
							status: status,
							header: http.Header{"Content-Type": []string{"application/json"}},
							body:   tc.body,
						}
						h, repo := newPolicyTestHandler(t, upstream, policyTestAccounts(accountType, status))
						body := []byte(fmt.Sprintf(`{"model":"gpt-5.2","messages":[{"role":"user","content":"synthetic"}],"stream":%t}`, stream))
						c, rec := newLargeBodyTestContext("/v1/chat/completions", body, "")
						h.ChatCompletions(c)
						require.Equal(t, int32(1), upstream.calls.Load(), "no same-account retry or account switch")
						require.Zero(t, repo.mutations.Load(), "no account health, cooldown or disable side effect")
						require.Contains(t, rec.Header().Get("Content-Type"), "application/json")
						requirePolicyClientError(t, status, tc.wantCode, tc.wantType, rec.Code, rec.Body.String())
					})
				}
			}
		}
	}
}

// Negative controls: credential/endpoint 403s, unknown codes and opaque bodies
// keep their existing failover semantics even when a field looks policy-like.
func TestOpenAIPolicyClassifierLeavesNonPolicy403Unchanged(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tc := range []struct {
		name string
		body string
	}{
		{name: "credential_code", body: `{"error":{"type":"permission_error","code":"invalid_api_key","message":"synthetic"}}`},
		{name: "unknown_code_policy_type", body: `{"error":{"type":"content_policy","code":"unsupported_country_region_territory","message":"synthetic"}}`},
		{name: "access_state_code_wins", body: `{"code":"account_deactivated","error":{"code":"content_policy","message":"synthetic"}}`},
		{name: "plain_text", body: `forbidden: content_policy`},
		{name: "message_only", body: `{"error":{"message":"blocked by content_policy"}}`},
	} {
		for _, path := range []string{"/v1/responses", "/v1/chat/completions"} {
			t.Run(tc.name+path, func(t *testing.T) {
				upstream := &scriptedErrorUpstream{
					status: http.StatusForbidden,
					header: http.Header{"Content-Type": []string{"application/json"}},
					body:   tc.body,
				}
				h, _ := newPolicyTestHandler(t, upstream, largeBodyTestAccounts("apikey"))
				if path == "/v1/responses" {
					c, rec := newLargeBodyTestContext(path, largeResponsesTextBody(1<<10), "")
					h.Responses(c)
					require.NotEqual(t, policyFixedMessage, gjson.Get(rec.Body.String(), "error.message").String())
				} else {
					c, rec := newLargeBodyTestContext(path, []byte(`{"model":"gpt-5.2","messages":[{"role":"user","content":"synthetic"}]}`), "")
					h.ChatCompletions(c)
					require.NotEqual(t, policyFixedMessage, gjson.Get(rec.Body.String(), "error.message").String())
				}
				require.Equal(t, int32(2), upstream.calls.Load(), "non-policy 403 keeps account failover")
			})
		}
	}
}

// A structured policy code wins over a message that also matches the
// rejected-field compatibility retry: one attempt, no field-stripping replay.
func TestOpenAIResponsesPolicyRejectionPrecedesRejectedFieldRetry(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const conflictBody = `{"error":{"type":"permission_error","code":"content_policy_violation","param":"max_output_tokens","message":"Unsupported parameter: 'max_output_tokens'"}}`
	requestBody := []byte(`{"model":"gpt-5.2","input":"synthetic","max_output_tokens":16,"stream":false}`)
	for _, accountType := range []string{"oauth", "apikey", "apikey_passthrough", "apikey_chat"} {
		t.Run(accountType, func(t *testing.T) {
			upstream := &scriptedErrorUpstream{
				status: http.StatusBadRequest,
				header: http.Header{"Content-Type": []string{"application/json"}},
				body:   conflictBody,
			}
			h, repo := newPolicyTestHandler(t, upstream, policyTestAccounts(accountType, http.StatusBadRequest))
			c, rec := newLargeBodyTestContext("/v1/responses", requestBody, "")
			h.Responses(c)
			require.Equal(t, int32(1), upstream.calls.Load(), "structured policy must win before message-based field retry")
			require.Zero(t, repo.mutations.Load(), "no account health, cooldown or disable side effect")
			require.Equal(t, http.StatusBadRequest, rec.Code)
			require.Equal(t, "content_policy_violation", gjson.Get(rec.Body.String(), "error.code").String())
			require.Equal(t, "permission_error", gjson.Get(rec.Body.String(), "error.type").String())
			require.Equal(t, policyFixedMessage, gjson.Get(rec.Body.String(), "error.message").String())
			require.NotContains(t, rec.Body.String(), "Unsupported parameter")
		})
	}
}

// Control: without a policy code the same message keeps the existing
// rejected-field retry on the passthrough path.
func TestOpenAIResponsesNonPolicyRejectedFieldRetryUnchanged(t *testing.T) {
	gin.SetMode(gin.TestMode)
	upstream := &scriptedErrorUpstream{
		status: http.StatusBadRequest,
		header: http.Header{"Content-Type": []string{"application/json"}},
		body:   `{"error":{"type":"invalid_request_error","code":"unsupported_parameter","param":"max_output_tokens","message":"Unsupported parameter: 'max_output_tokens'"}}`,
	}
	h, _ := newPolicyTestHandler(t, upstream, largeBodyTestAccounts("apikey_passthrough"))
	c, rec := newLargeBodyTestContext("/v1/responses", []byte(`{"model":"gpt-5.2","input":"synthetic","max_output_tokens":16,"stream":false}`), "")
	h.Responses(c)
	require.GreaterOrEqual(t, upstream.calls.Load(), int32(2), "non-policy rejected field keeps its compatibility retry")
	require.NotEqual(t, policyFixedMessage, gjson.Get(rec.Body.String(), "error.message").String())
}
