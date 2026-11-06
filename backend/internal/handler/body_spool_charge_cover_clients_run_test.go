//go:build unit

package handler

import (
	"crypto/sha256"
	"fmt"
	"net/http"
	"os"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// coverClientsOffList: client shapes that stay off the verified-raw list,
// each with why (report.md, "client shapes"). Every other client shape
// must be on the list.
var coverClientsOffList = map[string]string{
	"client_sdk_prompt":                "root prompt: k 14.48 on the passthrough path",
	"client_sdk_tool_image_generation": "Codex image-tool policy and image bridge decode it for Codex clients (corpus shape image_generation); k 3.22 for this non-Codex client",
	"client_sdk_tool_local_shell":      "local_shell_call ids are deleted by the input-ID sanitizer (prefix fc_ expected): forwarded changed, k 3.22",
}

// coverClientsCharged: listed stress shapes that the body path charges the
// k_conv top-up on purpose before a bounded-but-large decode.
var coverClientsCharged = map[string]string{
	"stress_tools_json_schema":            "skeleton rewrite above 4096 values (rewriteOpenAIRequestSkeleton)",
	"stress_additional_tools_json_schema": "same",
}

// coverClientsRewritten: listed client shapes whose body the gateway
// rewrites on purpose (on raw JSON); every other listed shape must reach
// the upstream byte for byte.
var coverClientsRewritten = map[string]string{
	"client_pi":                   "store=false replay: reasoning rs_ id dropped (editStoreFalseReplayItemRaw)",
	"client_pi_tool_search":       "same pi items as client_pi: rs_ id dropped",
	"stress_reasoning_rs_summary": "store=false replay: rs_ id dropped",
	"stress_message_call_id":      "store=false replay: message call_id dropped",
	"stress_item_id_sanitize":     "input-ID sanitizer: message ids without msg prefix dropped",
}

// TestClientShapesStayOnListWithIdentifierCap (rr10): the real pi and SDK
// request shapes, with their normal-length ids, keys and modes, stay on
// the verified-raw list under the 4 KiB identifier cap at every size; only
// the shapes recorded in coverClientsOffList are off.
func TestClientShapesStayOnListWithIdentifierCap(t *testing.T) {
	for _, cl := range coverClients() {
		for _, size := range []int{64 << 10, 2 << 20} {
			listed, reason := service.VerifiedRawResponsesRequest(cl.body(size), http.Header{})
			if why, off := coverClientsOffList[cl.name]; off {
				assert.False(t, listed, "%s (%s)", cl.name, why)
				continue
			}
			assert.True(t, listed, "%s at %d: %s", cl.name, size, reason)
		}
	}
}

// TestBodySpoolClientShapesRawPath (rr6 follow-up item 6): each client
// shape at 16 MiB on /v1/responses, API-key force_responses account. A
// shape on the verified-raw list must get 200 at the k_skel charge with
// the live heap within it; the upstream must receive the body unchanged.
// Logs k (live-heap peak / body) for every shape, also those off the list.
// Slow: SUB2API_CHARGE_COVER=1; SUB2API_CHARGE_COVER_ONLY filters by name.
func TestBodySpoolClientShapesRawPath(t *testing.T) {
	if os.Getenv("SUB2API_CHARGE_COVER") == "" {
		t.Skip("set SUB2API_CHARGE_COVER=1")
	}
	gin.SetMode(gin.TestMode)
	only := os.Getenv("SUB2API_CHARGE_COVER_ONLY")
	var cb coverCombo
	for _, c := range coverOpenAICombos() {
		if c.route == "v1/responses" && c.account == "apikey-force_responses" {
			cb = c
		}
	}
	require.NotEmpty(t, cb.path)
	var srv *coverServer
	for _, cl := range append(coverClients(), coverStressClients()...) {
		if !coverSelected(cl.name, only) {
			continue
		}
		if srv == nil {
			h, cfg, up := cb.setup(t)
			srv = &coverServer{h: h, cfg: cfg, up: up}
			runCoverCase(t, *srv, cb.path, coverBody(coverProtos["responses"], coverBases[0], 64<<10, cb.model, "", ""), false, nil)
		}
		t.Run(cl.name, func(t *testing.T) {
			body := cl.body(16 << 20)
			listed, reason := service.VerifiedRawResponsesRequest(body, http.Header{})
			sum := coverExpectedForwardSum(t, body)
			r := runCoverCaseBody(t, *srv, cb.path, body, false, nil, false)
			got, _ := srv.up.lastSum.Load().(string)
			unchanged := r.calls > 0 && got == sum
			k := float64(r.TotalAlloc) / float64(r.bytes)
			chargeK := float64(r.PeakCharge) / float64(r.bytes)
			t.Logf("CLIENT %s listed=%v reason=%q status=%d calls=%d bytes=%d k=%.2f charge_k=%.2f live_k=%.2f excess=%d unchanged=%v",
				cl.name, listed, reason, r.status, r.calls, r.bytes, k, chargeK, float64(r.PeakHeap)/float64(r.bytes), r.Excess, unchanged)
			assertChargeCovers(t, r.chargeCoverReading)
			why, off := coverClientsOffList[cl.name]
			if off {
				assert.False(t, listed, "%s is on the list now; drop it from coverClientsOffList (%s)", cl.name, why)
				return
			}
			if !assert.True(t, listed, "%s off the list: %s", cl.name, reason) {
				return
			}
			assert.Equal(t, http.StatusOK, r.status, r.reply)
			if _, charged := coverClientsCharged[cl.name]; charged {
				assert.GreaterOrEqual(t, chargeK, bodySpoolRuntimeFor(srv.cfg).cfg.KConv-0.5, "the k_conv top-up was taken")
				return
			}
			kSkel := bodySpoolRuntimeFor(srv.cfg).cfg.KSkel
			assert.LessOrEqual(t, chargeK, kSkel+0.5, "a listed shape takes the k_skel charge")
			assert.LessOrEqual(t, k, kSkel, "a listed shape stays within k_skel")
			if _, rewritten := coverClientsRewritten[cl.name]; !rewritten {
				assert.True(t, unchanged, "the upstream got the body changed")
			}
		})
	}
}

// coverExpectedForwardSum: sha256 of body as a non-Codex client's request
// reaches an API-key upstream unchanged: only the raw patch that deletes
// the root members the upstream rejects (openai_gateway_forward.go,
// prompt_cache_retention, safety_identifier, prompt_cache_options), and
// the raw edit that deletes namespace from direct input items when no
// namespace tool is declared (stripOpenAIResponsesInputNamespaces; listed
// bodies declare none). The expected body is dropped before the measured
// request.
func coverExpectedForwardSum(t *testing.T, body []byte) string {
	t.Helper()
	want := body
	for _, key := range []string{"prompt_cache_retention", "safety_identifier", "prompt_cache_options"} {
		if gjson.GetBytes(want, key).Exists() {
			var err error
			want, err = sjson.DeleteBytes(want, key)
			require.NoError(t, err)
		}
	}
	var withNamespace []int
	idx := 0
	gjson.GetBytes(want, "input").ForEach(func(_, item gjson.Result) bool {
		if item.Get("namespace").Exists() {
			withNamespace = append(withNamespace, idx)
		}
		idx++
		return true
	})
	for _, i := range withNamespace {
		var err error
		want, err = sjson.DeleteBytes(want, fmt.Sprintf("input.%d.namespace", i))
		require.NoError(t, err)
	}
	sum := sha256.Sum256(want)
	return string(sum[:])
}
