//go:build unit

package service

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestRewriteOpenAIRequestSkeleton (review rr4): a root-member rewrite keeps
// the input array as sent, sees its additional_tools items, and decodes to
// what the same rewrite of the fully decoded body gives.
func TestRewriteOpenAIRequestSkeleton(t *testing.T) {
	body := `{"model":"m","input":[{"role":"user","content":"a"},{"type":"additional_tools","tools":[{"type":"image_generation"}]},` +
		`{"role":"user","content":"b\u00e9"}],"text":{"format":{"type":"json_schema","schema":{"type":"object","uniqueItems":true}}},"n":12345678901234567890}`
	seen := false
	rewrite := func(m map[string]any) bool {
		seen = hasOpenAIImageGenerationTool(m)
		return normalizeOpenAIResponseFormatSchemas(m)
	}
	out, changed, ok, err := rewriteOpenAIRequestSkeleton([]byte(body), rewrite, NoConversionCharge)
	require.NoError(t, err)
	require.True(t, ok)
	require.True(t, changed)
	require.True(t, seen, "the additional_tools item is visible")
	require.Contains(t, string(out), `"input":[{"role":"user","content":"a"},{"type":"additional_tools","tools":[{"type":"image_generation"}]},{"role":"user","content":"b\u00e9"}]`)

	var full map[string]any
	require.NoError(t, decodeOpenAIJSONUseNumber([]byte(body), &full))
	require.True(t, rewrite(full))
	var got any
	require.NoError(t, decodeOpenAIJSONUseNumber(out, &got))
	require.Equal(t, any(full), got)

	same, changed, ok, err := rewriteOpenAIRequestSkeleton([]byte(body), func(map[string]any) bool { return false }, NoConversionCharge)
	require.NoError(t, err)
	require.True(t, ok)
	require.False(t, changed)
	require.Equal(t, body, string(same))

	for _, b := range []string{`{"input":[],"input":[]}`, `[1]`, `{"input":`} {
		_, _, ok, err := rewriteOpenAIRequestSkeleton([]byte(b), rewrite, NoConversionCharge)
		require.NoError(t, err)
		require.False(t, ok, b)
	}
	out, changed, ok, err = rewriteOpenAIRequestSkeleton([]byte(`{"input":"text","text":{"format":{"type":"json_schema","schema":{"minProperties":1}}}}`), rewrite, NoConversionCharge)
	require.NoError(t, err)
	require.True(t, ok && changed)
	require.Contains(t, string(out), `"input":"text"`)
	require.NotContains(t, string(out), "minProperties")
}

// TestRewriteOpenAIRequestSkeletonDuplicateItemType (review rr5 blocker b):
// an input item's type is its LAST type member, as the decoder reads it. An
// additional_tools item written "type":"message","type":"additional_tools"
// is seen (so the Codex image bridge does not add a second
// image_generation tool); one written the other way round is not.
func TestRewriteOpenAIRequestSkeletonDuplicateItemType(t *testing.T) {
	for _, tc := range []struct {
		item string
		seen bool
	}{
		{`{"type":"message","type":"additional_tools","tools":[{"type":"image_generation"}]}`, true},
		{`{"type":"additional_tools","type":"message","tools":[{"type":"image_generation"}]}`, false},
		{`{"typ\u0065":"message","type":"additional_tools","tools":[{"type":"image_generation"}]}`, true},
	} {
		body := `{"model":"m","input":[{"role":"user","content":"a"},` + tc.item + `]}`
		var seen bool
		_, _, ok, err := rewriteOpenAIRequestSkeleton([]byte(body), func(m map[string]any) bool {
			seen = hasOpenAIImageGenerationTool(m)
			return false
		}, NoConversionCharge)
		require.NoError(t, err)
		require.True(t, ok)
		require.Equal(t, tc.seen, seen, tc.item)

		var full map[string]any
		require.NoError(t, decodeOpenAIJSONUseNumber([]byte(body), &full))
		require.Equal(t, hasOpenAIImageGenerationTool(full), seen, "same as the decoded body: %s", tc.item)
	}
}

// TestRewriteOpenAIRequestSkeletonChargesLargeSkeleton (rr6 follow-up): a
// skeleton above contentSeedDecodeMaxValues values takes the conversion
// charge before its decode; a refused charge decodes nothing.
func TestRewriteOpenAIRequestSkeletonChargesLargeSkeleton(t *testing.T) {
	tools := strings.Repeat(`{"type":"function","name":"f"},`, 3000)
	body := `{"model":"m","tools":[` + tools[:len(tools)-1] + `],"input":[{"role":"user","content":"a"}]}`
	charged := 0
	charge := func() error { charged++; return nil }
	_, _, ok, err := rewriteOpenAIRequestSkeleton([]byte(body), func(map[string]any) bool { return false }, charge)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, 1, charged)

	called := false
	_, _, ok, err = rewriteOpenAIRequestSkeleton([]byte(body), func(map[string]any) bool { called = true; return false },
		func() error { return ErrRequestConversionRefused })
	require.ErrorIs(t, err, ErrRequestConversionRefused)
	require.True(t, ok)
	require.False(t, called)

	_, _, _, err = rewriteOpenAIRequestSkeleton([]byte(`{"model":"m","tools":[{"type":"function","name":"f"}],"input":[]}`),
		func(map[string]any) bool { return false }, func() error { t.Fatal("small skeleton charged"); return nil })
	require.NoError(t, err)
}

// TestRewriteOpenAIRequestSkeletonChargesLargeString (rr9): a skeleton of
// few values above openAIRequestSkeletonDecodeMaxBytes (one large escaped
// instructions string) takes the conversion charge once, before its decode;
// input items other than additional_tools do not count.
func TestRewriteOpenAIRequestSkeletonChargesLargeString(t *testing.T) {
	instr := strings.Repeat(`line \"quoted\" caf\u00e9 text\n`, 4<<10)
	body := `{"model":"m","instructions":"` + instr + `","input":[{"role":"user","content":"a"}]}`
	require.Greater(t, len(body), openAIRequestSkeletonDecodeMaxBytes)
	require.False(t, rawJSONValuesAbove(body, contentSeedDecodeMaxValues))
	charged := 0
	out, changed, ok, err := rewriteOpenAIRequestSkeleton([]byte(body), func(m map[string]any) bool { m["x"] = 1; return true },
		func() error { charged++; return nil })
	require.NoError(t, err)
	require.True(t, ok)
	require.True(t, changed)
	require.Equal(t, 1, charged)
	require.Contains(t, string(out), `"x":1`)

	called := false
	_, _, ok, err = rewriteOpenAIRequestSkeleton([]byte(body), func(map[string]any) bool { called = true; return false },
		func() error { return ErrRequestConversionRefused })
	require.ErrorIs(t, err, ErrRequestConversionRefused)
	require.True(t, ok)
	require.False(t, called)

	large := `{"model":"m","input":[{"role":"user","content":"` + instr + `"}]}`
	_, _, ok, err = rewriteOpenAIRequestSkeleton([]byte(large), func(map[string]any) bool { return false },
		func() error { t.Fatal("input items charged"); return nil })
	require.NoError(t, err)
	require.True(t, ok)
}
