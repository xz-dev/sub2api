package service

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestClassifyOpenAIUpstreamPolicyError(t *testing.T) {
	openAI := &Account{ID: 1, Platform: PlatformOpenAI}
	for _, tc := range []struct {
		name     string
		account  *Account
		status   int
		body     string
		want     bool
		wantCode string
		wantType string
	}{
		{name: "code_and_safe_type", status: 400, body: `{"error":{"type":"permission_error","code":"content_policy_violation","message":"x"}}`, want: true, wantCode: "content_policy_violation", wantType: "permission_error"},
		{name: "code_and_invalid_request_type", status: 403, body: `{"error":{"type":"invalid_request_error","code":"content_filter"}}`, want: true, wantCode: "content_filter", wantType: "invalid_request_error"},
		{name: "code_with_marker_type", status: 400, body: `{"error":{"type":"content_moderation","code":"content_policy"}}`, want: true, wantCode: "content_policy", wantType: "content_moderation"},
		{name: "missing_type_falls_back", status: 400, body: `{"error":{"code":"content_filter","message":"x"}}`, want: true, wantCode: "content_filter", wantType: "content_policy"},
		{name: "unsafe_type_not_copied", status: 403, body: `{"error":{"type":"secret_internal_type","code":"content_moderation"}}`, want: true, wantCode: "content_moderation", wantType: "content_policy"},
		{name: "non_string_type_not_copied", status: 403, body: `{"error":{"type":7,"code":"content_policy"}}`, want: true, wantCode: "content_policy", wantType: "content_policy"},
		{name: "type_only_supplies_code", status: 403, body: `{"error":{"type":"content_policy_violation","message":"x"}}`, want: true, wantCode: "content_policy_violation", wantType: "content_policy_violation"},
		{name: "empty_code_type_only", status: 400, body: `{"error":{"type":"content_filter","code":""}}`, want: true, wantCode: "content_filter", wantType: "content_filter"},
		{name: "null_code_type_only", status: 400, body: `{"error":{"type":"content_filter","code":null}}`, want: true, wantCode: "content_filter", wantType: "content_filter"},
		{name: "whitespace_and_case_normalized", status: 400, body: `{"error":{"type":" Permission_Error ","code":" CONTENT_POLICY "}}`, want: true, wantCode: "content_policy", wantType: "permission_error"},
		{name: "top_level_marker_code", status: 400, body: `{"code":"content_policy","error":{"type":"invalid_request_error"}}`, want: true, wantCode: "content_policy", wantType: "invalid_request_error"},

		{name: "unknown_code_policy_type", status: 403, body: `{"error":{"type":"content_policy","code":"unsupported_country_region_territory"}}`},
		{name: "credential_code_policy_type", status: 403, body: `{"error":{"type":"content_filter","code":"invalid_api_key"}}`},
		{name: "conflicting_top_level_code", status: 400, body: `{"code":"model_not_found","error":{"code":"content_policy"}}`},
		{name: "access_state_code_wins", status: 403, body: `{"code":"account_deactivated","error":{"code":"content_policy"}}`},
		{name: "non_string_code", status: 400, body: `{"error":{"type":"content_policy","code":451}}`},
		{name: "safe_type_without_code", status: 403, body: `{"error":{"type":"permission_error","message":"content_policy"}}`},
		{name: "message_only", status: 400, body: `{"error":{"message":"blocked by content_policy_violation"}}`},
		{name: "nested_message_json_not_searched", status: 400, body: `{"error":{"message":"{\"error\":{\"code\":\"content_policy\"}}"}}`},
		{name: "nested_descendant_not_searched", status: 400, body: `{"error":{"details":{"code":"content_policy"}}}`},
		{name: "error_not_object", status: 400, body: `{"error":"content_policy"}`},
		{name: "malformed_json", status: 400, body: `{"error":{"code":"content_policy"`},
		{name: "plain_text", status: 403, body: `content_policy`},
		{name: "empty_body", status: 403, body: ``},
		{name: "bare_403", status: 403, body: `{"error":{"message":"Forbidden"}}`},
		{name: "cyber_policy_kept_dedicated", status: 400, body: `{"error":{"type":"invalid_request_error","code":"cyber_policy"}}`},
		{name: "status_401", status: 401, body: `{"error":{"code":"content_policy"}}`},
		{name: "status_422", status: 422, body: `{"error":{"code":"content_policy"}}`},
		{name: "status_429", status: 429, body: `{"error":{"code":"content_policy"}}`},
		{name: "status_500", status: 500, body: `{"error":{"code":"content_policy"}}`},
		{name: "grok_kept_provider_specific", account: &Account{ID: 2, Platform: PlatformGrok}, status: 403, body: `{"error":{"code":"content_policy"}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			account := tc.account
			if account == nil {
				account = openAI
			}
			got, ok := classifyOpenAIUpstreamPolicyError(account, tc.status, []byte(tc.body))
			require.Equal(t, tc.want, ok)
			if !tc.want {
				require.Nil(t, got)
				return
			}
			require.Equal(t, tc.status, got.StatusCode)
			require.Equal(t, tc.wantCode, got.Code)
			require.Equal(t, tc.wantType, got.Type)
		})
	}
}

func TestShouldFailoverOpenAIPassthroughResponseSkipsPolicyRejection(t *testing.T) {
	account := &Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Credentials: map[string]any{
		"pool_mode": true, "pool_mode_retry_status_codes": []any{float64(403)},
	}}
	require.False(t, shouldFailoverOpenAIPassthroughResponse(account, 403, []byte(`{"error":{"code":"content_policy"}}`)))
	require.True(t, shouldFailoverOpenAIPassthroughResponse(account, 403, []byte(`{"error":{"code":"invalid_api_key"}}`)))
}

func requirePolicyErrorObject(t *testing.T, raw, wantCode string) {
	t.Helper()
	require.Equal(t, wantCode, gjson.Get(raw, "code").String())
	require.Equal(t, OpenAIUpstreamPolicyMessage, gjson.Get(raw, "message").String())
}

func TestWriteOpenAIUpstreamPolicyRejectionBeforeCommitKeepsStatus(t *testing.T) {
	for _, chat := range []bool{false, true} {
		for _, status := range []int{http.StatusBadRequest, http.StatusForbidden} {
			t.Run(fmt.Sprintf("chat_%t/%d", chat, status), func(t *testing.T) {
				c, rec := newCompactBridgeTestContext(t, false)
				svc := &OpenAIGatewayService{}
				body := []byte(`{"error":{"type":"permission_error","code":"content_policy_violation","message":"synthetic detail 10.0.0.7"}}`)
				err := svc.writeOpenAIUpstreamPolicyRejectionIfRecognized(c, &Account{ID: 1, Platform: PlatformOpenAI}, status, body, chat)
				require.True(t, IsOpenAIUpstreamPolicyError(err))
				require.True(t, IsResponseCommitted(c))
				require.Equal(t, status, rec.Code)
				require.Equal(t, "permission_error", gjson.Get(rec.Body.String(), "error.type").String())
				requirePolicyErrorObject(t, gjson.Get(rec.Body.String(), "error").Raw, "content_policy_violation")
				require.NotContains(t, rec.Body.String(), "10.0.0.7")
			})
		}
	}
}

func TestWriteOpenAIUpstreamPolicyRejectionIgnoresNonPolicy(t *testing.T) {
	c, rec := newCompactBridgeTestContext(t, false)
	svc := &OpenAIGatewayService{}
	err := svc.writeOpenAIUpstreamPolicyRejectionIfRecognized(c, &Account{ID: 1, Platform: PlatformOpenAI}, 403, []byte(`{"error":{"code":"invalid_api_key"}}`), false)
	require.NoError(t, err)
	require.False(t, c.Writer.Written())
	require.Equal(t, 0, rec.Body.Len())
}

// After the compact keepalive committed HTTP 200, the Responses terminal event
// carries the policy code; the committed status is not rewritten.
func TestWriteOpenAIUpstreamPolicyRejectionAfterResponsesKeepaliveCommit(t *testing.T) {
	c, rec := newCompactBridgeTestContext(t, true)
	stop := StartOpenAICompactSSEKeepalive(c, keepaliveTestInterval)
	defer stop()
	waitForKeepaliveBeats()

	writeOpenAIUpstreamPolicyRejection(c, &OpenAIUpstreamPolicyError{StatusCode: 403, Code: "content_filter", Type: "permission_error"}, false)

	require.Equal(t, http.StatusOK, rec.Code)
	events := parseCompactBridgeSSE(t, stripKeepaliveComments(rec.Body.String()))
	require.Len(t, events, 1)
	require.Equal(t, "response.failed", events[0][0])
	require.Equal(t, "failed", gjson.Get(events[0][1], "response.status").String())
	requirePolicyErrorObject(t, gjson.Get(events[0][1], "response.error").Raw, "content_filter")
}

// A committed Chat Completions stream terminates with the Chat SSE error frame.
func TestWriteOpenAIUpstreamPolicyRejectionAfterChatStreamCommit(t *testing.T) {
	c, rec := newCompactBridgeTestContext(t, false)
	c.Writer.Header().Set("Content-Type", "text/event-stream")
	c.Writer.WriteHeader(http.StatusOK)
	_, _ = c.Writer.Write([]byte("data: {\"choices\":[]}\n\n"))

	writeOpenAIUpstreamPolicyRejection(c, &OpenAIUpstreamPolicyError{StatusCode: 400, Code: "content_moderation", Type: "content_moderation"}, true)

	require.Equal(t, http.StatusOK, rec.Code)
	frames := strings.Split(strings.TrimSpace(rec.Body.String()), "\n\n")
	require.Len(t, frames, 2)
	last := strings.TrimPrefix(frames[1], "data: ")
	requirePolicyErrorObject(t, gjson.Get(last, "error").Raw, "content_moderation")
	require.Equal(t, "invalid_request_error", gjson.Get(last, "error.type").String())
}

func TestReportOpenAIAccountScheduleResultIgnoresPolicyRejection(t *testing.T) {
	newSvc := func() *OpenAIGatewayService {
		return &OpenAIGatewayService{rateLimitService: newOpenAIAdvancedSchedulerRateLimitService("true")}
	}
	account := &Account{ID: 77, Platform: PlatformOpenAI, Type: AccountTypeAPIKey}

	svc := newSvc()
	policyErr := fmt.Errorf("wrapped: %w", &OpenAIUpstreamPolicyError{StatusCode: 403, Code: "content_policy", Type: "content_policy"})
	require.False(t, svc.ReportOpenAIAccountScheduleResult(account, "", false, nil, policyErr))
	require.Zero(t, svc.SnapshotOpenAIAccountSchedulerMetrics().RuntimeStatsAccountCount, "policy rejection records no scheduler error sample")

	control := newSvc()
	control.ReportOpenAIAccountScheduleResult(account, "", false, nil, errors.New("upstream error: 403"))
	require.Equal(t, 1, control.SnapshotOpenAIAccountSchedulerMetrics().RuntimeStatsAccountCount, "non-policy failure is still recorded")
}
