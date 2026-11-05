//go:build unit

package handler

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// TestBodySpoolConvertedRequestTopUp (review rr3 blocker 2): a covered
// /v1/responses request that the account serves through the Responses->Chat
// converter holds k_conv × its inline bytes while upstream runs; the same
// request to a Responses-native account holds k_skel × them. Both are
// released at chain return.
func TestBodySpoolConvertedRequestTopUp(t *testing.T) {
	gin.SetMode(gin.TestMode)
	body := largeResponsesTextBody(2 << 20)
	held := func(accountType string) int64 {
		upstream := &largeBodyDrainUpstream{block: make(chan struct{}), entered: make(chan struct{})}
		h := newLargeBodyTestHandler(t, upstream, accountType)
		enableBodySpoolForTest(t, h.cfg, true, 4<<30, nil)
		meter := bodySpoolRuntimeFor(h.cfg).meter
		c, rec := newLargeBodyTestContext("/v1/responses", body, "")
		done := make(chan struct{})
		go func() { defer close(done); h.Responses(c); releaseRequestBodySpool(c) }()
		select {
		case <-upstream.entered:
		case <-done:
			t.Fatalf("%s: no upstream call: %d %s", accountType, rec.Code, rec.Body.String())
		}
		n := meter.Outstanding()
		close(upstream.block)
		<-done
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		require.Zero(t, meter.Outstanding(), "%s: released at chain return", accountType)
		return n
	}
	h := newLargeBodyTestHandler(t, &largeBodyDrainUpstream{}, "oauth")
	enableBodySpoolForTest(t, h.cfg, true, 4<<30, nil)
	cfg := bodySpoolRuntimeFor(h.cfg).cfg
	want := chargeBytes(cfg.KConv-cfg.KSkel, roundUpGranule(int64(len(body))))
	native, converted := held("oauth"), held("apikey_chat")
	t.Logf("held while upstream runs: native %d B, converted %d B (k_skel %.1f, k_conv %.1f)", native, converted, cfg.KSkel, cfg.KConv)
	require.Equal(t, want, converted-native)
}

// TestBodySpoolConvertedRequestRefusalIsFinal (review rr3 blocker 2): on a
// 320m hop a converted request above S_conv gets 413 request_too_large
// with limit_bytes = S_conv, and one that fits S_conv but not the budget
// gets 503 insufficient_memory. No upstream is called and no other account
// is tried. A Responses-native account takes the same bodies.
func TestBodySpoolConvertedRequestRefusalIsFinal(t *testing.T) {
	gin.SetMode(gin.TestMode)
	run := func(accountType string, body []byte, current int64) (*largeBodyDrainUpstream, int, map[string]any) {
		upstream := &largeBodyDrainUpstream{}
		h := newLargeBodyTestHandler(t, upstream, accountType)
		env := enableBodySpoolForTest(t, h.cfg, true, 320<<20, nil)
		require.NoError(t, os.WriteFile(filepath.Join(env.cgroup, "memory.current"), []byte(strconv.FormatInt(current, 10)+"\n"), 0o600))
		meter := bodySpoolRuntimeFor(h.cfg).meter
		c, rec := newLargeBodyTestContext("/v1/responses", body, "")
		h.Responses(c)
		releaseRequestBodySpool(c)
		require.Zero(t, meter.Outstanding())
		var out map[string]any
		if rec.Code != http.StatusOK {
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out), rec.Body.String())
			out = out["error"].(map[string]any)
		}
		return upstream, rec.Code, out
	}
	h := newLargeBodyTestHandler(t, &largeBodyDrainUpstream{}, "oauth")
	enableBodySpoolForTest(t, h.cfg, true, 320<<20, nil)
	cfg := bodySpoolRuntimeFor(h.cfg).cfg
	sConv := conversionLimit(cfg, 320<<20, true, false)
	t.Logf("S_conv at 320m = %d B (%.2f MiB), k_conv %.1f", sConv, float64(sConv)/(1<<20), cfg.KConv)
	require.Less(t, sConv, cfg.InlineLimit)

	above := largeResponsesTextBody(int(sConv) + 1<<20)
	upstream, code, e := run("apikey_chat", above, 0)
	require.Equal(t, http.StatusRequestEntityTooLarge, code, "%v", e)
	require.Equal(t, "request_too_large", e["code"])
	require.Equal(t, bodySpoolHop, e["hop"])
	require.EqualValues(t, sConv, e["limit_bytes"])
	require.Zero(t, upstream.calls.Load(), "final: no upstream call, no failover")
	_, code, _ = run("oauth", above, 0)
	require.Equal(t, http.StatusOK, code, "a Responses-native account takes it (S = 16 MiB)")

	// Fits S_conv, but beside a busy working set only k_skel fits, not k_conv.
	fits := largeResponsesTextBody(int(sConv) - 64<<10)
	busy := int64(320<<20) - 32<<20 - int64(float64(cfg.KSkel+1)*float64(len(fits)))
	upstream, code, e = run("apikey_chat", fits, busy)
	require.Equal(t, http.StatusServiceUnavailable, code, "%v", e)
	require.Equal(t, "insufficient_memory", e["code"])
	require.Equal(t, bodySpoolHop, e["hop"])
	require.Zero(t, upstream.calls.Load(), "final: no upstream call, no failover")
	_, code, _ = run("oauth", fits, busy)
	require.Equal(t, http.StatusOK, code)
}
