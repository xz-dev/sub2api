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

// legacyNormalizeCompactionTriggerInputOrder is the decode-first version
// NormalizeCompactionTriggerInputOrder had before its precheck (review rr3).
func legacyNormalizeCompactionTriggerInputOrder(body []byte) ([]byte, bool, error) {
	var payload map[string]any
	if err := decodeOpenAIJSONUseNumber(body, &payload); err != nil {
		return body, false, err
	}
	input, ok := payload["input"].([]any)
	if !ok || len(input) == 0 {
		return body, false, nil
	}
	count, last := 0, false
	for i, raw := range input {
		if item, ok := raw.(map[string]any); ok && item["type"] == "compaction_trigger" {
			count++
			last = i == len(input)-1
		}
	}
	return body, count > 1 || (count == 1 && !last), nil
}

// TestCompactionTriggerPrecheckMatchesDecode: the precheck that skips the
// decode gives the decoding code's answer, on duplicate and escaped names,
// escaped values, a trigger in a shadowed "input", non-object bodies and
// invalid ones.
func TestCompactionTriggerPrecheckMatchesDecode(t *testing.T) {
	bodies := []string{
		`{"input":[{"type":"message"},{"type":"compaction_trigger"}]}`,
		`{"input":[{"type":"compaction_trigger"},{"type":"message"}]}`,
		`{"input":[{"type":"compaction_trigger"},{"type":"message"}],"input":[{"type":"message"}]}`,
		`{"input":[{"type":"message"}],"input":[{"type":"compaction_trigger"},{"type":"x"}]}`,
		`{"\u0069nput":[{"type":"compaction\u005ftrigger"},{"type":"x"}]}`,
		`{"input":[{"type":"compaction_trigger","type":"message"},{"type":"x"}]}`,
		`{"input":[{"type":"message","type":"compaction_trigger"},{"type":"x"}]}`,
		`{"input":[{"type":["compaction_trigger"]},{"type":"x"}]}`, `{"input":"compaction_trigger"}`,
		`{"input":[{"role":"user","content":"a"},{"role":"assistant","content":"b"}]}`,
		`[{"type":"compaction_trigger"}]`, `null`, `"x"`, `{"input":[}`, `{"input":[]} x`, "{\"input\":[{\"type\":\"m\xff\"}]}",
	}
	for _, body := range bodies {
		_, gotChanged, gotErr := NormalizeCompactionTriggerInputOrder([]byte(body))
		_, wantChanged, wantErr := legacyNormalizeCompactionTriggerInputOrder([]byte(body))
		require.Equal(t, wantErr != nil, gotErr != nil, body)
		require.Equal(t, wantChanged, gotChanged, body)
	}
}

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
