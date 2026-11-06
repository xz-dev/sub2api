//go:build unit

package handler

import (
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// TestBodySpoolOffListDecisionIsLogged (rr6 follow-up): a request on a
// covered route, or a Responses fallback route, that falls to the
// conservative charge is counted and logged once, with its route class and
// the first failing reason (a name, never a value). A request on the list
// is recorded at the account gate of an off-list account; a request on
// another route records nothing.
func TestBodySpoolOffListDecisionIsLogged(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := newLargeBodyTestHandler(t, &largeBodyDrainUpstream{}, "apikey")
	enableBodySpoolForTest(t, h.cfg, true, 320<<20, nil)
	cases := []struct {
		name, path, body, class, stage, reason string
	}{
		{"covered, tool off the list", "/v1/responses", `{"model":"gpt-5.5","input":"hi","tools":[{"type":"web_search","filters":{"allowed_domains":["secret.example"]}}]}`, "responses", "request", "root member tools: tool type web_search"},
		{"covered, item off the list", "/openai/v1/responses", `{"model":"gpt-5.5","input":[{"type":"item_reference","id":"secret_id"}]}`, "responses", "request", "root member input: item type item_reference"},
		{"covered, root member off the list", "/v1/responses", `{"model":"gpt-5.5","input":"hi","conversation":"secret_conv"}`, "responses", "request", "root member conversation"},
		{"compact, off the list", "/v1/responses/compact", `{"model":"gpt-5.5","input":[{"type":"reasoning","content":[{"type":"reasoning_text","text":"secret"}]}]}`, "responses_compact", "request", "root member input: reasoning content"},
		{"covered, on the list", "/v1/responses", `{"model":"gpt-5.5","input":"hi","tools":[{"type":"function","name":"f"}]}`, "responses", "account", "account Type"},
		{"chat route", "/v1/chat/completions", `{"model":"gpt-5.5","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"web_search"}]}`, "", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			core, logs := observer.New(zap.InfoLevel)
			c, rec := newLargeBodyTestContext(tc.path, []byte(tc.body), "")
			c.Request = c.Request.WithContext(logger.IntoContext(c.Request.Context(), zap.New(core)))
			before := service.LargeRequestOffListSnapshot()

			require.True(t, ingestRequestBody(c, h.cfg), rec.Body.String())
			// A forward attempt to an off-list account finds it again:
			// still one record per request.
			_ = service.ChargeRequestOffList(c, "account", "account Type")
			releaseRequestBodySpool(c)

			entries := logs.FilterMessage("large_request.off_list").All()
			after := service.LargeRequestOffListSnapshot()
			if tc.class == "" {
				require.Empty(t, entries)
				require.Equal(t, before, after)
				return
			}
			require.Len(t, entries, 1)
			f := entries[0].ContextMap()
			require.Equal(t, tc.class, f["route_class"])
			require.Equal(t, tc.stage, f["stage"])
			require.Equal(t, tc.reason, f["reason"])
			require.Equal(t, "charged", f["outcome"])
			require.NotContains(t, f["reason"], "secret")
			require.Equal(t, before[tc.class]+1, after[tc.class])
		})
	}
}
