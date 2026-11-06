//go:build unit

package service

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

func offListContext(t *testing.T, class string, charge func() error) (*gin.Context, *observer.ObservedLogs) {
	t.Helper()
	core, logs := observer.New(zap.InfoLevel)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	c.Request = c.Request.WithContext(logger.IntoContext(c.Request.Context(), zap.New(core)))
	if class != "" {
		c.Set(LargeRequestRouteClassKey, class)
	}
	if charge != nil {
		c.Set(RequestConversionChargeKey, charge)
	}
	return c, logs
}

// TestChargeRequestOffListRecordsOnce: an off-list decision on a covered
// route takes the charge, counts once per request and logs one line with
// the route class and the reason, names only.
func TestChargeRequestOffListRecordsOnce(t *testing.T) {
	charged := 0
	c, logs := offListContext(t, "responses_test_once", func() error { charged++; return nil })
	before := LargeRequestOffListSnapshot()["responses_test_once"]

	require.NoError(t, ChargeRequestOffList(c, "request", "tool type web_search"))
	require.NoError(t, ChargeRequestOffList(c, "account", "account IsOpenAIPassthroughEnabled"))

	require.Equal(t, 2, charged, "every call reaches the once-per-request charge")
	require.Equal(t, before+1, LargeRequestOffListSnapshot()["responses_test_once"])
	entries := logs.FilterMessage("large_request.off_list").All()
	require.Len(t, entries, 1)
	f := entries[0].ContextMap()
	require.Equal(t, "responses_test_once", f["route_class"])
	require.Equal(t, "request", f["stage"])
	require.Equal(t, "tool type web_search", f["reason"])
	require.Equal(t, "charged", f["outcome"])
	require.Equal(t, before+1, f["off_list_total"])
}

func TestChargeRequestOffListRefusedOutcome(t *testing.T) {
	c, logs := offListContext(t, "responses_test_refused", func() error { return ErrRequestConversionRefused })
	err := ChargeRequestOffList(c, "account", "account Type")
	require.True(t, errors.Is(err, ErrRequestConversionRefused))
	entries := logs.FilterMessage("large_request.off_list").All()
	require.Len(t, entries, 1)
	require.Equal(t, "refused", entries[0].ContextMap()["outcome"])
	require.Equal(t, "account Type", entries[0].ContextMap()["reason"])
}

// TestChargeRequestOffListOutsideCoveredRoute: no route class (another
// route, or the spool off): the charge still runs, nothing is recorded.
func TestChargeRequestOffListOutsideCoveredRoute(t *testing.T) {
	charged := false
	c, logs := offListContext(t, "", func() error { charged = true; return nil })
	before := LargeRequestOffListSnapshot()
	require.NoError(t, ChargeRequestOffList(c, "account", "account Type"))
	require.True(t, charged)
	require.Zero(t, logs.Len())
	require.Equal(t, before, LargeRequestOffListSnapshot())
}

// TestVerifiedRawReasonNamesOnly: a reason names the failing part and
// never carries a value, also when the client sends a long, odd or
// non-string name or type.
func TestVerifiedRawReasonNamesOnly(t *testing.T) {
	long := strings.Repeat("x", 1<<20)
	cases := []struct{ body, want string }{
		{`{"prompt":"secret-value"}`, "root member prompt"},
		{`{"` + long + `":1}`, "root member " + strings.Repeat("x", 64) + "..."},
		{`{"we ird\"":1}`, "root member _"},
		{`{"input":[{"type":"image_generation_call","result":"secret"}]}`, "root member input: item type image_generation_call"},
		{`{"input":[{"type":{"secret":"v"}}]}`, "root member input: item type not a plain string"},
		{`{"input":[{"type":"a b<c>"}]}`, "root member input: item type a_b_c_"},
		{`{"input":[{"role":"user","content":[{"type":"input_audio","data":"secret"}]}]}`, "root member input: content part type input_audio"},
		{`{"tools":[{"type":"image_generation","secret":"v"}]}`, "root member tools: tool type image_generation"},
		{`{"input":[{"type":"reasoning","id":"rs_1","id":"secret"}]}`, "root member input: item duplicate member id"},
		{`{"tools":[{"type":["secret"]}]}`, "root member tools: tool type not a plain string"},
		{`{"input":[{"type":"reasoning","content":[{"type":"reasoning_text","text":"secret"}]}]}`, "root member input: reasoning content"},
	}
	for _, tc := range cases {
		ok, reason := VerifiedRawResponsesRequest([]byte(tc.body), http.Header{})
		require.False(t, ok)
		require.Equal(t, tc.want, reason)
		require.NotContains(t, reason, "secret")
	}
}
