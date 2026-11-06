//go:build unit

package service

import (
	"encoding/json"
	"runtime"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/bodyspool"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func contentSeedJSONForTest(raw string) string {
	var b strings.Builder
	writeContentSeedJSON(&b, raw)
	return b.String()
}

// TestContentSeedJSONBounded (rr6 follow-up): a content-seed field of at
// most contentSeedDecodeMaxValues values keeps its canonical form; a larger
// one is written as sent, without a decode.
func TestContentSeedJSONBounded(t *testing.T) {
	small := `[{"type":"function", "name":"f","parameters":{"type":"object"}}]`
	require.Equal(t, normalizeCompatSeedJSON(rawJSONView(small)), contentSeedJSONForTest(small))
	require.False(t, rawJSONValuesAbove(`["a,b{[","c\"],d"]`, 3))
	require.True(t, rawJSONValuesAbove(`["a","b","c"]`, 3))

	tools := `[` + strings.Repeat(`{"type":"function","name":"f","parameters":{"type":"object"}},`, 1<<14)
	tools = tools[:len(tools)-1] + `]`
	require.True(t, rawJSONValuesAbove(tools, contentSeedDecodeMaxValues))
	require.Equal(t, "raw:"+tools, contentSeedJSONForTest(tools))

	var b strings.Builder
	b.Grow(len(tools) + 8)
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	writeContentSeedJSON(&b, tools)
	runtime.ReadMemStats(&after)
	require.Less(t, after.TotalAlloc-before.TotalAlloc, uint64(4<<10), "a large field is not decoded")
}

// TestContentSeedRawFieldOverSpliceStream: a field above the bound that
// holds a spooled string hashes the same from the skeleton as from the
// whole body.
func TestContentSeedRawFieldOverSpliceStream(t *testing.T) {
	gin.SetMode(gin.TestMode)
	img := strings.Repeat("QUJD", 40000)
	parts := strings.Repeat(`{"type":"input_text","text":"a"},`, 3000)
	for name, body := range map[string]string{
		"tools":     `{"model":"gpt-5.1","tools":[` + strings.Repeat(`{"type":"function","name":"f"},`, 3000) + `{"type":"function","name":"g","image_url":"data:image/png;base64,` + img + `"}],"input":[{"role":"user","content":"hi"}]}`,
		"developer": `{"model":"gpt-5.1","input":[{"role":"developer","content":[` + parts + `{"type":"input_image","image_url":"data:image/png;base64,` + img + `"}]},{"role":"user","content":"hi"}]}`,
		// rr9: few values; above the byte cap on the whole body, a marker
		// on the skeleton.
		"first_user_image": `{"model":"gpt-5.1","input":[{"role":"user","content":[{"type":"input_text","text":"a"},{"type":"input_image","image_url":"data:image/png;base64,` + img + `"}]}]}`,
	} {
		sp, skel := spoolForTest(t, []byte(body))
		svc := &OpenAIGatewayService{}
		legacyCtx, spoolCtx := ginWithSpool(nil, ""), ginWithSpool(sp, "")
		require.Contains(t, deriveOpenAIContentSessionSeed([]byte(body)), "raw:", name)
		want := svc.GenerateSessionHash(legacyCtx, []byte(body))
		require.NotEmpty(t, want, name)
		require.Equal(t, want, svc.GenerateSessionHash(spoolCtx, skel), name)
	}
}

// TestContentSeedJSONBoundedInBytes (rr9): a field of few values but more
// than contentSeedDecodeMaxBytes bytes (one large escaped string) is
// written as sent, without a decode; so is a field holding a spool marker.
// The byte cap must not exceed the spool threshold T.
func TestContentSeedJSONBoundedInBytes(t *testing.T) {
	require.LessOrEqual(t, contentSeedDecodeMaxBytes, bodyspool.DefaultThreshold)
	big := `"` + strings.Repeat(`line \"quoted\" caf\u00e9 text\n`, 1<<15) + `"`
	require.Greater(t, len(big), contentSeedDecodeMaxBytes)
	require.False(t, rawJSONValuesAbove(big, contentSeedDecodeMaxValues))
	require.Equal(t, "raw:"+big, contentSeedJSONForTest(big))
	require.Equal(t, big, cyberTranscriptCanonical(big))

	var b strings.Builder
	b.Grow(len(big) + 8)
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	writeContentSeedJSON(&b, big)
	_ = cyberTranscriptCanonical(big)
	runtime.ReadMemStats(&after)
	require.Less(t, after.TotalAlloc-before.TotalAlloc, uint64(4<<10), "a large string is not decoded")

	atCap := `"` + strings.Repeat("a", contentSeedDecodeMaxBytes-2) + `"`
	require.Equal(t, normalizeCompatSeedJSON(rawJSONView(atCap)), contentSeedJSONForTest(atCap))
	marker := `["x\u0000lrc0123:0\u0000"]`
	require.Equal(t, "raw:"+marker, contentSeedJSONForTest(marker))
	require.Equal(t, marker, cyberTranscriptCanonical(marker))
}

// TestBoundedDecodesSkipLargeValues (rr9): the tool-schema union check and
// the turn metadata are not decoded above contentSeedDecodeMaxBytes (or,
// for the schema, contentSeedDecodeMaxValues).
func TestBoundedDecodesSkipLargeValues(t *testing.T) {
	small := []byte(`{"oneOf":[{"type":"object"}]}`)
	_, ok := openAIMissingRootObjectUnionTypeEdit(small, 0)
	require.True(t, ok)
	pad := `,"description":"` + strings.Repeat("a", contentSeedDecodeMaxBytes) + `"}`
	_, ok = openAIMissingRootObjectUnionTypeEdit([]byte(`{"oneOf":[{"type":"object"}]`+pad), 0)
	require.False(t, ok, "above the byte cap")
	many := `{"oneOf":[{"type":"object"}],"enum":[` + strings.Repeat(`0,`, contentSeedDecodeMaxValues) + `0]}`
	require.Less(t, len(many), contentSeedDecodeMaxBytes)
	_, ok = openAIMissingRootObjectUnionTypeEdit([]byte(many), 0)
	require.False(t, ok, "above the value cap")

	meta := `{"thread_id":"t1","request_kind":"turn"}`
	got, ok := parseCodexTurnMetadata(meta)
	require.True(t, ok)
	require.Equal(t, "t1", got.ThreadID)
	big := `{"thread_id":"t1","x":"` + strings.Repeat("a", contentSeedDecodeMaxBytes) + `"}`
	_, ok = parseCodexTurnMetadata(big)
	require.False(t, ok)
	quoted := func(s string) string { b, _ := json.Marshal(s); return string(b) }
	require.Equal(t, "t1", openAIWSExecutionTurnMetadata(nil, []byte(`{"client_metadata":{"x-codex-turn-metadata":`+quoted(meta)+`}}`)).ThreadID)
	require.Empty(t, openAIWSExecutionTurnMetadata(nil, []byte(`{"client_metadata":{"x-codex-turn-metadata":`+quoted(big)+`}}`)).ThreadID)
}
