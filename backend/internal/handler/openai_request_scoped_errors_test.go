//go:build unit

package handler

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"sync/atomic"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// Request-scoped upstream errors (task 7.3, design D5/D9): an inner gateway
// error (JSON code plus hop) and every provider 413 reach the client
// unchanged after exactly one account attempt, with no failover.

type scriptedErrorUpstream struct {
	service.HTTPUpstream
	status int
	header http.Header
	body   string
	calls  atomic.Int32
}

func (u *scriptedErrorUpstream) Do(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	u.calls.Add(1)
	if req.Body != nil {
		_, _ = io.Copy(io.Discard, req.Body)
	}
	return &http.Response{
		StatusCode: u.status,
		Header:     u.header.Clone(),
		Body:       io.NopCloser(bytes.NewBufferString(u.body)),
	}, nil
}

var requestScopedErrorCases = []struct {
	name   string
	status int
	header http.Header
	body   string
}{
	{
		name:   "provider_413",
		status: http.StatusRequestEntityTooLarge,
		body:   `{"error":{"type":"invalid_request_error","code":"request_too_large","message":"request exceeds the provider limit of 32000000 bytes"}}`,
	},
	{
		name:   "inner_413_route_ceiling",
		status: http.StatusRequestEntityTooLarge,
		body:   `{"error":{"type":"invalid_request_error","code":"request_too_large","hop":"aisix","message":"body of 40000000 bytes exceeds the 20000000-byte ceiling of route bedrock-claude"}}`,
	},
	{
		name:   "inner_507",
		status: http.StatusInsufficientStorage,
		body:   `{"error":{"type":"server_error","code":"insufficient_storage","hop":"cpa","message":"spool write refused: free space would drop below the 1 GiB reserve"}}`,
	},
	{
		name:   "inner_503_memory",
		status: http.StatusServiceUnavailable,
		body:   `{"error":{"type":"server_error","code":"insufficient_memory","hop":"aisix","message":"aisix: 25165824 bytes needed, 1048576 free"}}`,
	},
	{
		name:   "inner_503_busy",
		status: http.StatusServiceUnavailable,
		header: http.Header{"Retry-After": []string{"5"}},
		body:   `{"error":{"type":"server_error","code":"capacity_busy","hop":"keeper","message":"capacity busy"}}`,
	},
	{
		name:   "inner_503_ticket",
		status: http.StatusServiceUnavailable,
		body:   `{"error":{"type":"server_error","code":"capacity_ticket_invalid","hop":"cpa","message":"cpa capacity ticket invalid: ticket already released"}}`,
	},
}

func TestOpenAIResponsesRequestScopedErrorsPassThrough(t *testing.T) {
	gin.SetMode(gin.TestMode)
	sizes := []int{1 << 20}
	if !raceEnabled {
		sizes = append(sizes, 64<<20) // legacy path holds ~8x the body
	}
	for _, size := range sizes {
		body := largeResponsesImagesBody(size)
		for _, accountType := range []string{"oauth", "apikey", "apikey_passthrough"} {
			for _, tc := range requestScopedErrorCases {
				if size > 1<<20 && tc.name != "provider_413" {
					continue
				}
				name := accountType + "/" + tc.name + "/" + byteSizeName(size)
				t.Run(name, func(t *testing.T) {
					header := http.Header{"Content-Type": []string{"application/json"}, "X-Request-Id": []string{"rid-scoped"}}
					for k, v := range tc.header {
						header[k] = v
					}
					upstream := &scriptedErrorUpstream{status: tc.status, header: header, body: tc.body}
					h := newLargeBodyTestHandler(t, upstream, accountType)
					c, rec := newLargeBodyTestContext("/v1/responses", body, "")
					h.Responses(c)
					require.Equal(t, int32(1), upstream.calls.Load(), "exactly one account attempt, no failover")
					require.Equal(t, tc.status, rec.Code)
					require.Equal(t, tc.body, rec.Body.String(), "body must pass through unchanged")
					require.Equal(t, "application/json", rec.Header().Get("Content-Type"))
					require.Equal(t, "rid-scoped", rec.Header().Get("X-Request-Id"))
					if ra := tc.header.Get("Retry-After"); ra != "" {
						require.Equal(t, ra, rec.Header().Get("Retry-After"))
					}
				})
			}
		}
	}
}

// TestOpenAIResponsesOrdinaryProvider5xxStillFailsOver guards the unchanged
// classification: a 503 without a gateway code and hop still fails over.
func TestOpenAIResponsesOrdinaryProvider5xxStillFailsOver(t *testing.T) {
	gin.SetMode(gin.TestMode)
	upstream := &scriptedErrorUpstream{
		status: http.StatusServiceUnavailable,
		header: http.Header{"Content-Type": []string{"application/json"}},
		body:   `{"error":{"type":"server_error","code":"insufficient_memory","message":"no hop field: provider error"}}`,
	}
	h := newLargeBodyTestHandler(t, upstream, "apikey")
	c, _ := newLargeBodyTestContext("/v1/responses", largeResponsesTextBody(1<<10), "")
	h.Responses(c)
	require.Equal(t, int32(2), upstream.calls.Load(), "ordinary 5xx keeps failing over")
}

func TestOpenAIChatConvertedRequestScopedErrorsPassThrough(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, stream := range []bool{false, true} {
		for _, tc := range requestScopedErrorCases {
			// A provider 413 without a named hop retains the Chat compatibility writer.
			if tc.name == "provider_413" {
				continue
			}
			t.Run(fmt.Sprintf("%s/stream_%t", tc.name, stream), func(t *testing.T) {
				upstream := &scriptedErrorUpstream{
					status: tc.status,
					header: http.Header{"Content-Type": []string{"application/json"}, "X-Request-Id": []string{"rid-chat-scoped"}},
					body:   tc.body,
				}
				accounts := largeBodyTestAccounts("apikey")
				for i := range accounts {
					accounts[i].Credentials["pool_mode"] = true
					accounts[i].Credentials["pool_mode_retry_status_codes"] = []any{float64(tc.status)}
				}
				h := newLargeBodyTestHandlerWithAccounts(t, upstream, accounts)
				body := []byte(fmt.Sprintf(`{"model":"gpt-5.2","messages":[{"role":"user","content":"synthetic"}],"stream":%t}`, stream))
				c, rec := newLargeBodyTestContext("/v1/chat/completions", body, "")
				h.ChatCompletions(c)
				require.Equal(t, int32(1), upstream.calls.Load(), "no pool retry or account switch")
				require.Equal(t, tc.status, rec.Code)
				require.Equal(t, tc.body, rec.Body.String())
				require.Equal(t, "application/json", rec.Header().Get("Content-Type"))
				require.Equal(t, "rid-chat-scoped", rec.Header().Get("X-Request-Id"))
			})
		}
	}
}

func TestOpenAIChatConvertedOrdinaryProvider5xxStillFailsOver(t *testing.T) {
	gin.SetMode(gin.TestMode)
	upstream := &scriptedErrorUpstream{
		status: http.StatusServiceUnavailable,
		header: http.Header{"Content-Type": []string{"application/json"}},
		body:   `{"error":{"type":"server_error","code":"insufficient_memory","message":"provider error without hop"}}`,
	}
	h := newLargeBodyTestHandler(t, upstream, "apikey")
	c, _ := newLargeBodyTestContext("/v1/chat/completions", []byte(`{"model":"gpt-5.2","messages":[{"role":"user","content":"synthetic"}]}`), "")
	h.ChatCompletions(c)
	require.Equal(t, int32(2), upstream.calls.Load(), "ordinary 5xx retains account failover")
}

func byteSizeName(n int) string {
	if n >= 1<<20 {
		return strconv.Itoa(n>>20) + "MiB"
	}
	return strconv.Itoa(n) + "B"
}
