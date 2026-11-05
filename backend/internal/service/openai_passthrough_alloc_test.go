//go:build !race

package service

import (
	"bytes"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestPassthroughNormalizersAllocateLittleWhenUnchanged (review rr3): on a
// 4 MiB body of ~30-byte messages that none of them changes, the
// passthrough request normalizers allocate a small fraction of the body
// (they took about 30 x, 7 x and 5 x).
func TestPassthroughNormalizersAllocateLittleWhenUnchanged(t *testing.T) {
	var b bytes.Buffer
	b.WriteString(`{"model":"m","store":false,"input":[`)
	for i := 0; b.Len() < 4<<20; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(`{"role":"user","content":"m` + strconv.Itoa(i) + `"}`)
	}
	b.WriteString(`]}`)
	body := b.Bytes()
	for name, f := range map[string]func() (bool, error){
		"compaction_trigger": func() (bool, error) { _, c, err := NormalizeCompactionTriggerInputOrder(body); return c, err },
		"input_item_ids":     func() (bool, error) { _, c, err := sanitizeOpenAIResponsesInputItemIDs(body); return c, err },
		"store_false_replay": func() (bool, error) {
			_, c, err := normalizeOpenAIAPIKeyStoreFalseReasoningReplay(body, false)
			return c, err
		},
		"namespace_tools": func() (bool, error) { return hasOpenAIResponsesNamespaceToolDeclaration(body), nil },
		"has_trigger":     func() (bool, error) { return HasCompactionTriggerInInput(body), nil },
	} {
		old := debug.SetGCPercent(-1)
		var a, z runtime.MemStats
		runtime.ReadMemStats(&a)
		changed, err := f()
		runtime.ReadMemStats(&z)
		debug.SetGCPercent(old)
		require.NoError(t, err, name)
		require.False(t, changed, name)
		ratio := float64(z.TotalAlloc-a.TotalAlloc) / float64(len(body))
		t.Logf("%-20s allocated %.3f x the body", name, ratio)
		require.Less(t, ratio, 0.05, name)
	}
	require.True(t, strings.HasPrefix(string(body), `{"model"`))
}
