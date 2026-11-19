package service

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

// OpenAIUpstreamPolicyMessage is the fixed client explanation for a
// recognized upstream content-policy rejection. Upstream text is never copied.
const OpenAIUpstreamPolicyMessage = "Request blocked by upstream content policy."

// openAIUpstreamPolicyFallbackType replaces a missing or unsafe error.type.
const openAIUpstreamPolicyFallbackType = "content_policy"

// openAIUpstreamPolicyMarkers is the finite set of structured code/type values
// that identify a request-scoped content-policy rejection. cyber_policy keeps
// its dedicated handling and is deliberately absent.
var openAIUpstreamPolicyMarkers = map[string]struct{}{
	"content_filter":           {},
	"content_policy":           {},
	"content_policy_violation": {},
	"content_moderation":       {},
}

// OpenAIUpstreamPolicyError reports a recognized content-policy rejection that
// has already been written to the client. It is request-scoped: no retry, no
// failover and no account-state or health attribution.
type OpenAIUpstreamPolicyError struct {
	StatusCode int
	Code       string
	Type       string
}

func (e *OpenAIUpstreamPolicyError) Error() string {
	return fmt.Sprintf("upstream content policy rejection: %d code=%s", e.StatusCode, e.Code)
}

// IsOpenAIUpstreamPolicyError reports whether err is a recognized upstream
// content-policy rejection.
func IsOpenAIUpstreamPolicyError(err error) bool {
	var policyErr *OpenAIUpstreamPolicyError
	return errors.As(err, &policyErr)
}

func isOpenAIUpstreamPolicyMarker(value string) bool {
	_, ok := openAIUpstreamPolicyMarkers[value]
	return ok
}

func isOpenAIUpstreamPolicySafeType(value string) bool {
	return isOpenAIUpstreamPolicyMarker(value) || value == "invalid_request_error" || value == "permission_error"
}

// openAIUpstreamPolicyField normalizes a structured classification field.
// ok=false means a present non-string value, which is unknown and therefore
// never treated as policy.
func openAIUpstreamPolicyField(r gjson.Result) (value string, ok bool) {
	if !r.Exists() || r.Type == gjson.Null {
		return "", true
	}
	if r.Type != gjson.String {
		return "", false
	}
	return strings.ToLower(strings.TrimSpace(r.Str)), true
}

// classifyOpenAIUpstreamPolicyError recognizes an upstream HTTP 400/403 whose
// OpenAI error envelope carries a known content-policy code, or a known
// content-policy type when no code is present. It reads only error.code,
// error.type and the top-level code; it never searches message text or other
// nested strings. A non-empty unknown code, an account/workspace access-state
// code, cyber_policy and Grok accounts keep their existing handling.
func classifyOpenAIUpstreamPolicyError(account *Account, statusCode int, body []byte) (*OpenAIUpstreamPolicyError, bool) {
	if statusCode != http.StatusBadRequest && statusCode != http.StatusForbidden {
		return nil, false
	}
	if account != nil && account.Platform == PlatformGrok {
		return nil, false
	}
	if len(body) == 0 || !gjson.ValidBytes(body) {
		return nil, false
	}
	envelope := gjson.GetBytes(body, "error")
	if !envelope.IsObject() {
		return nil, false
	}
	if hit, _, _ := detectOpenAICyberPolicy(body); hit {
		return nil, false
	}
	if isOpenAIHTTPUpstreamAccessStateError(statusCode, "", body) {
		return nil, false
	}
	topCode, ok := openAIUpstreamPolicyField(gjson.GetBytes(body, "code"))
	if !ok || (topCode != "" && !isOpenAIUpstreamPolicyMarker(topCode)) {
		return nil, false
	}
	code, ok := openAIUpstreamPolicyField(envelope.Get("code"))
	if !ok || (code != "" && !isOpenAIUpstreamPolicyMarker(code)) {
		return nil, false
	}
	errType, ok := openAIUpstreamPolicyField(envelope.Get("type"))
	if !ok {
		errType = ""
	}
	if code == "" {
		code = topCode
	}
	if code == "" {
		if !isOpenAIUpstreamPolicyMarker(errType) {
			return nil, false
		}
		code = errType
	}
	if !isOpenAIUpstreamPolicySafeType(errType) {
		errType = openAIUpstreamPolicyFallbackType
	}
	return &OpenAIUpstreamPolicyError{StatusCode: statusCode, Code: code, Type: errType}, true
}

// writeOpenAIUpstreamPolicyRejectionIfRecognized terminates the request when
// the upstream error is a recognized content-policy rejection. It must run
// before any same-account retry, failover or account side effect. It returns
// nil when the error is not a policy rejection; the caller then continues with
// its existing handling.
func (s *OpenAIGatewayService) writeOpenAIUpstreamPolicyRejectionIfRecognized(
	c *gin.Context,
	account *Account,
	statusCode int,
	body []byte,
	chatCompletions bool,
) error {
	policyErr, ok := classifyOpenAIUpstreamPolicyError(account, statusCode, body)
	if !ok {
		return nil
	}
	setOpsUpstreamError(c, statusCode, OpenAIUpstreamPolicyMessage, truncateString(string(body), 2048))
	writeOpenAIUpstreamPolicyRejection(c, policyErr, chatCompletions)
	return policyErr
}

// writeOpenAIUpstreamPolicyRejection writes the safe policy error with the
// existing protocol writers. Before commitment it keeps the original HTTP
// 400/403; after a committed stream it emits the protocol terminal error event
// and leaves the committed status unchanged.
func writeOpenAIUpstreamPolicyRejection(c *gin.Context, policyErr *OpenAIUpstreamPolicyError, chatCompletions bool) {
	if c == nil || policyErr == nil {
		return
	}
	keepaliveCommitted := StopOpenAICompactSSEKeepaliveCommitted(c)
	streamCommitted := c.Writer.Written() && strings.HasPrefix(c.Writer.Header().Get("Content-Type"), "text/event-stream")
	if keepaliveCommitted || streamCommitted {
		if chatCompletions {
			MarkOpsStreamError(c, policyErr.Type, OpenAIUpstreamPolicyMessage, policyErr.StatusCode)
			if _, err := fmt.Fprint(c.Writer, buildChatStreamErrorSSEWithType(policyErr.Type, policyErr.Code, OpenAIUpstreamPolicyMessage)); err == nil {
				c.Writer.Flush()
			}
			return
		}
		writeOpenAICompactSSEFailureMessageWithType(c, policyErr.StatusCode, policyErr.Code, policyErr.Type, OpenAIUpstreamPolicyMessage)
		return
	}
	MarkResponseCommitted(c)
	c.JSON(policyErr.StatusCode, gin.H{
		"error": gin.H{
			"type":    policyErr.Type,
			"code":    policyErr.Code,
			"message": OpenAIUpstreamPolicyMessage,
		},
	})
}
