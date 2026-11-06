//go:build unit

package service

import (
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
	out, changed, ok, err := rewriteOpenAIRequestSkeleton([]byte(body), rewrite)
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

	same, changed, ok, err := rewriteOpenAIRequestSkeleton([]byte(body), func(map[string]any) bool { return false })
	require.NoError(t, err)
	require.True(t, ok)
	require.False(t, changed)
	require.Equal(t, body, string(same))

	for _, b := range []string{`{"input":[],"input":[]}`, `[1]`, `{"input":`} {
		_, _, ok, err := rewriteOpenAIRequestSkeleton([]byte(b), rewrite)
		require.NoError(t, err)
		require.False(t, ok, b)
	}
	out, changed, ok, err = rewriteOpenAIRequestSkeleton([]byte(`{"input":"text","text":{"format":{"type":"json_schema","schema":{"minProperties":1}}}}`), rewrite)
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
		})
		require.NoError(t, err)
		require.True(t, ok)
		require.Equal(t, tc.seen, seen, tc.item)

		var full map[string]any
		require.NoError(t, decodeOpenAIJSONUseNumber([]byte(body), &full))
		require.Equal(t, hasOpenAIImageGenerationTool(full), seen, "same as the decoded body: %s", tc.item)
	}
}
