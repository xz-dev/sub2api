//go:build unit

package service

import (
	"runtime"
	"strings"
	"testing"

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
