package service

import (
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
