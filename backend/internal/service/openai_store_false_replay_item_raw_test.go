//go:build unit

package service

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// decodedStoreFalseReplayItem is the old decoded form of the store=false
// item edits, kept for the parity test.
func decodedStoreFalseReplayItem(t *testing.T, raw string, stripID, addSummary, stripCallID bool) string {
	t.Helper()
	var decoded map[string]any
	require.NoError(t, decodeOpenAIJSONUseNumber([]byte(raw), &decoded))
	if stripID {
		delete(decoded, "id")
	}
	if addSummary {
		decoded["summary"] = []any{}
	}
	if stripCallID {
		delete(decoded, "call_id")
	}
	out, err := marshalOpenAIUpstreamJSON(decoded)
	require.NoError(t, err)
	return string(out)
}

// TestEditStoreFalseReplayItemRawMatchesDecoded: the raw edit gives the
// same JSON value as the decode-and-re-encode it replaced.
func TestEditStoreFalseReplayItemRawMatchesDecoded(t *testing.T) {
	cases := []struct {
		raw                           string
		stripID, addSummary, stripCID bool
	}{
		{`{"type":"reasoning","id":"rs_1","summary":[{"type":"summary_text","text":"s"}],"encrypted_content":"gAAA","content":[]}`, true, false, false},
		{`{"type":"reasoning","id":"rs_1","encrypted_content":"gAAA"}`, true, true, false},
		{`{"type":"reasoning","id":"rs_1","summary":null,"encrypted_content":"gAAA"}`, true, true, false},
		{`{ "type" : "reasoning" , "summary" : null , "id" : "rs_\u0031" }`, true, true, false},
		{`{"type":"reasoning","summary":[],"call_id":"c","id":"rs_2","n":1.50,"big":12345678901234567890}`, true, false, true},
		{`{"type":"message","role":"assistant","call_id":"c","content":[{"type":"output_text","text":"<a & b>\u00e9\n"}]}`, false, false, true},
		{`{"call_id":"c","type":"image_generation_call","id":"ig_1","result":"QUJD"}`, false, false, true},
		{`{"id":"rs_3","type":"reasoning"}`, true, true, false},
	}
	for _, tc := range cases {
		got, err := editStoreFalseReplayItemRaw(tc.raw, tc.stripID, tc.addSummary, tc.stripCID)
		require.NoError(t, err)
		want := decodedStoreFalseReplayItem(t, tc.raw, tc.stripID, tc.addSummary, tc.stripCID)
		var g, w any
		require.NoError(t, decodeOpenAIJSONUseNumber([]byte(got), &g), got)
		require.NoError(t, decodeOpenAIJSONUseNumber([]byte(want), &w))
		require.Equal(t, w, g, "raw %s: got %s want %s", tc.raw, got, want)
	}
}

// TestStoreFalseReplayLargeItemStaysRaw: a reasoning item with a large
// summary is edited in place, its other members kept byte for byte.
func TestStoreFalseReplayLargeItemStaysRaw(t *testing.T) {
	parts := strings.Repeat(`{"type":"summary_text","text":"a"},`, 1000)
	item := `{"type":"reasoning","id":"rs_1","summary":[` + parts[:len(parts)-1] + `],"encrypted_content":"gAAA","content":[]}`
	body := []byte(`{"model":"m","store":false,"input":[` + item + `,{"role":"user","content":"hi"}]}`)
	out, changed, err := normalizeOpenAIAPIKeyStoreFalseReasoningReplay(body, false)
	require.NoError(t, err)
	require.True(t, changed)
	require.Contains(t, string(out), `{"type":"reasoning","summary":[`+parts[:len(parts)-1]+`],"encrypted_content":"gAAA","content":[]}`)
	require.NotContains(t, string(out), `"rs_1"`)
}
