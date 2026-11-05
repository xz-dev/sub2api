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
	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
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
	codex            bool // a Codex client, image-generation bridge on, group allows images
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
	{name: "json_schema", root: `"text":{"format":{"type":"json_schema","name":"r","schema":{"type":"object","uniqueItems":true,"properties":{}}}},`},
	{name: "image-tool-legacy", root: `"tools":[{"type":"image_generation","format":"png"}],`, codex: true},
	{name: "codex-image-bridge", codex: true},
	{name: "codex-image-bridge+tool", root: `"tools":[{"type":"image_generation","output_format":"png"}],`, codex: true},
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
// bytes, and requires it to be at most k_skel (k_conv with
// SUB2API_KSKEL_ACCOUNT=oauth). It runs the shapes above at 16 MiB
// (SUB2API_KSKEL_MIB), plus the corpus files named in SUB2API_KSKEL_BODIES
// (comma-separated paths); SUB2API_KSKEL_SHAPES picks a subset of the
// shapes by name. Set SUB2API_KSKEL_LIVE=1 to run it; it skips
// otherwise (the heap reading is not meaningful under -race).
func TestBodySpoolPassthroughLiveK(t *testing.T) {
	if os.Getenv("SUB2API_KSKEL_LIVE") == "" {
		t.Skip("set SUB2API_KSKEL_LIVE=1 (and optionally SUB2API_KSKEL_BODIES) to measure passthrough k")
	}
	gin.SetMode(gin.TestMode)
	// Bodies are built one at a time: keeping all of them live would raise
	// the GC goal and let garbage count in the peak.
	type sample struct {
		name  string
		body  func() []byte
		codex bool
	}
	var samples []sample
	accountType := "apikey" // SUB2API_KSKEL_ACCOUNT=oauth measures an OAuth account
	if v := os.Getenv("SUB2API_KSKEL_ACCOUNT"); v != "" {
		accountType = v
	}
	// An OAuth account runs the Codex OAuth transform, a full decode that
	// takes the converter top-up (review rr4): its bound is k_conv.
	bound, boundName := config.DefaultBodySpoolKSkel, "k_skel"
	if accountType == "oauth" {
		bound, boundName = config.DefaultBodySpoolKConv, "k_conv"
	}
	size := 16 << 20
	if v, err := strconv.Atoi(os.Getenv("SUB2API_KSKEL_MIB")); err == nil && v > 0 {
		size = v << 20
	}
	only := os.Getenv("SUB2API_KSKEL_SHAPES") // comma-separated subset
	for _, sh := range passthroughKShapes {
		if only != "" && !strings.Contains(","+only+",", ","+sh.name+",") {
			continue
		}
		samples = append(samples, sample{sh.name, func() []byte {
			return passthroughKBody(size, sh.root, sh.item, sh.beforeLast, sh.stream)
		}, sh.codex})
	}
	for _, path := range strings.Split(os.Getenv("SUB2API_KSKEL_BODIES"), ",") {
		if path == "" {
			continue
		}
		samples = append(samples, sample{filepath.Base(path), func() []byte {
			body, err := os.ReadFile(path)
			require.NoError(t, err)
			return body
		}, false})
	}
	for _, s := range samples {
		body := s.body()
		worst := 0.0
		for run := 1; run <= 3; run++ {
			upstream := &passthroughKUpstream{sse: gjson.GetBytes(body, "stream").Bool()}
			accounts := largeBodyTestAccounts(accountType)
			for i := range accounts {
				if accounts[i].Extra != nil {
					accounts[i].Extra["openai_responses_mode"] = "force_responses"
				}
			}
			h := newLargeBodyTestHandlerWithAccounts(t, upstream, accounts)
			enableBodySpoolForTest(t, h.cfg, true, 8<<30, nil)
			c, rec := newLargeBodyTestContext("/v1/responses", body, "")
			if s.codex {
				h.cfg.Gateway.CodexImageGenerationBridgeEnabled = true
				c.Request.Header.Set("originator", "codex_cli_rs")
				c.MustGet(string(middleware2.ContextKeyAPIKey)).(*service.APIKey).Group.AllowImageGeneration = true
			}
			delta := measurePeakHeap(func() { h.Responses(c); releaseRequestBodySpool(c) })
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			require.EqualValues(t, 1, upstream.calls.Load())
			k := float64(delta) / float64(len(body))
			worst = max(worst, k)
			t.Logf("KSKEL %s run %d bytes %d forwarded %+d peak_live %.1f MiB k %.2f", s.name, run, len(body), upstream.forwarded.Load()-int64(len(body)), float64(delta)/(1<<20), k)
		}
		t.Logf("KSKEL %s %s worst %.2f (%s %.1f)", accountType, s.name, worst, boundName, bound)
		assert.LessOrEqual(t, worst, bound, "%s %s: k above %s", accountType, s.name, boundName)
	}
}
