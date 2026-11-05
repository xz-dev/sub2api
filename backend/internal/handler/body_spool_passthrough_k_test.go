//go:build unit

package handler

import (
	"bytes"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// passthroughKShapes (review rr4 blocker a): ~16 MiB bodies of ~35-byte
// messages that make a passthrough normalizer change, or inspect, the body.
// root is spliced in at the start of the root object; item is appended as
// the last input item, or before one last message when beforeLast is set.
var passthroughKShapes = []struct {
	name, root, item string
	beforeLast       bool
	stream           bool // stream:true (remote compaction v2 wire with a trigger)
}{
	{name: "tiny"},
	{name: "trigger-last", item: `{"type":"compaction_trigger"}`},
	{name: "trigger-mid", item: `{"type":"compaction_trigger"}`, beforeLast: true},
	{name: "stream+trigger-last", item: `{"type":"compaction_trigger"}`, stream: true},
	{name: "stream+trigger-mid", item: `{"type":"compaction_trigger"}`, beforeLast: true, stream: true},
	{name: "trigger-twice", item: `{"type":"compaction_trigger"},{"type":"compaction_trigger"}`, beforeLast: true},
	{name: "dupkey", root: `"store":false,`},
	{name: "ptc", root: `"parallel_tool_calls":true,`},
	{name: "trigger-mid+dupkey", root: `"store":false,`, item: `{"type":"compaction_trigger"}`, beforeLast: true},
	{name: "trigger-mid+ptc", root: `"parallel_tool_calls":true,`, item: `{"type":"compaction_trigger"}`, beforeLast: true},
	{name: "reasoning", item: `{"type":"reasoning","id":"rs_1","summary":[],"encrypted_content":"abc"}`},
	{name: "reasoning+dupkey", root: `"store":false,`, item: `{"type":"reasoning","id":"rs_1","summary":[],"encrypted_content":"abc"}`},
}

// passthroughKBody builds a body of about size bytes for one shape.
func passthroughKBody(size int, root, item string, beforeLast, stream bool) []byte {
	var b bytes.Buffer
	b.Grow(size + 1024)
	b.WriteString(`{` + root + `"model":"gpt-5.5","stream":` + strconv.FormatBool(stream) + `,"store":false,"input":[`)
	for i := 0; b.Len() < size-len(root)-len(item)-64; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(`{"role":"user","content":"m` + strconv.Itoa(i) + ` hello"}`)
	}
	if item != "" {
		b.WriteString(`,` + item)
	}
	if beforeLast {
		b.WriteString(`,{"role":"user","content":"last"}`)
	}
	b.WriteString(`]}`)
	return b.Bytes()
}

// passthroughKUpstream drains the request like largeBodyDrainUpstream and
// answers a streaming request with a completed SSE response.
type passthroughKUpstream struct {
	largeBodyDrainUpstream
	sse bool
}

func (u *passthroughKUpstream) Do(req *http.Request, proxy string, accountID int64, concurrency int) (*http.Response, error) {
	resp, err := u.largeBodyDrainUpstream.Do(req, proxy, accountID, concurrency)
	if err != nil || !u.sse {
		return resp, err
	}
	resp.Header.Set("Content-Type", "text/event-stream")
	resp.Body = io.NopCloser(strings.NewReader("data: {\"type\":\"response.completed\",\"response\":" +
		`{"id":"resp_1","object":"response","status":"completed","model":"gpt-5.5","output":[],` +
		`"usage":{"input_tokens":10,"output_tokens":1,"total_tokens":11}}}` + "\n\ndata: [DONE]\n\n"))
	return resp, nil
}

// TestBodySpoolPassthroughLiveK measures k on passthrough: the peak live heap
// (method of TestBodySpoolConversionLiveK) of a covered /v1/responses request
// on a Responses-native API-key account (no conversion), divided by the body
// bytes, and requires it to be at most k_skel. It runs the shapes above at
// 16 MiB, plus the corpus files named in SUB2API_KSKEL_BODIES
// (comma-separated paths); SUB2API_KSKEL_SHAPES picks a subset of the
// shapes by name. Set SUB2API_KSKEL_LIVE=1 to run it; it skips
// otherwise (the heap reading is not meaningful under -race).
func TestBodySpoolPassthroughLiveK(t *testing.T) {
	if os.Getenv("SUB2API_KSKEL_LIVE") == "" {
		t.Skip("set SUB2API_KSKEL_LIVE=1 (and optionally SUB2API_KSKEL_BODIES) to measure passthrough k")
	}
	gin.SetMode(gin.TestMode)
	type sample struct {
		name string
		body []byte
	}
	var samples []sample
	accountType := "apikey" // SUB2API_KSKEL_ACCOUNT=oauth measures an OAuth account
	if v := os.Getenv("SUB2API_KSKEL_ACCOUNT"); v != "" {
		accountType = v
	}
	only := os.Getenv("SUB2API_KSKEL_SHAPES") // comma-separated subset
	for _, sh := range passthroughKShapes {
		if only != "" && !strings.Contains(","+only+",", ","+sh.name+",") {
			continue
		}
		samples = append(samples, sample{sh.name, passthroughKBody(16<<20, sh.root, sh.item, sh.beforeLast, sh.stream)})
	}
	for _, path := range strings.Split(os.Getenv("SUB2API_KSKEL_BODIES"), ",") {
		if path == "" {
			continue
		}
		body, err := os.ReadFile(path)
		require.NoError(t, err)
		samples = append(samples, sample{filepath.Base(path), body})
	}
	for _, s := range samples {
		worst := 0.0
		for run := 1; run <= 3; run++ {
			upstream := &passthroughKUpstream{sse: gjson.GetBytes(s.body, "stream").Bool()}
			accounts := largeBodyTestAccounts(accountType)
			for i := range accounts {
				if accounts[i].Extra != nil {
					accounts[i].Extra["openai_responses_mode"] = "force_responses"
				}
			}
			h := newLargeBodyTestHandlerWithAccounts(t, upstream, accounts)
			enableBodySpoolForTest(t, h.cfg, true, 8<<30, nil)
			c, rec := newLargeBodyTestContext("/v1/responses", s.body, "")
			delta := measurePeakHeap(func() { h.Responses(c); releaseRequestBodySpool(c) })
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			require.EqualValues(t, 1, upstream.calls.Load())
			k := float64(delta) / float64(len(s.body))
			worst = max(worst, k)
			t.Logf("KSKEL %s run %d bytes %d peak_live %.1f MiB k %.2f", s.name, run, len(s.body), float64(delta)/(1<<20), k)
		}
		t.Logf("KSKEL %s worst %.2f (k_skel %.1f)", s.name, worst, config.DefaultBodySpoolKSkel)
		assert.LessOrEqual(t, worst, config.DefaultBodySpoolKSkel, "%s: passthrough k above k_skel", s.name)
	}
}
