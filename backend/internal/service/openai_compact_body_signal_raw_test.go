//go:build unit

package service

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestNormalizeCompactionTriggerInputOrderRawMatchesDecoded (review rr4
// blocker a): the raw rewrite gives the same changed flag as the decoding
// form and a body that decodes to the same value, including duplicate root
// and item keys (the decoder keeps the last), escaped names, and a trigger
// with extra members.
func TestNormalizeCompactionTriggerInputOrderRawMatchesDecoded(t *testing.T) {
	for name, body := range map[string]string{
		"mid":             `{"model":"m","input":[{"type":"message","role":"user","content":"a"},{"type":"compaction_trigger"},{"role":"user","content":"b"}]}`,
		"last":            `{"model":"m","input":[{"role":"user","content":"a"},{"type":"compaction_trigger"}]}`,
		"twice":           `{"input":[{"type":"compaction_trigger"},{"role":"user","content":"a"},{"type":"compaction_trigger","x":1}]}`,
		"only-two":        `{"input":[{"type":"compaction_trigger"},{"type":"compaction_trigger"}]}`,
		"extra-members":   ` { "input" : [ {"type":"compaction_trigger","note":"n"} , {"role":"user","content":"a"} ] , "n" : 12345678901234567890 } `,
		"dup-root-store":  `{"store":false,"input":[{"type":"compaction_trigger"},{"role":"user","content":"a"}],"store":false}`,
		"dup-input-last":  `{"input":[{"role":"user","content":"x"}],"input":[{"type":"compaction_trigger"},{"role":"user","content":"a"}]}`,
		"dup-input-first": `{"input":[{"type":"compaction_trigger"},{"role":"user","content":"a"}],"input":[{"role":"user","content":"x"}]}`,
		"dup-item-type":   `{"input":[{"type":"compaction_trigger","type":"message","content":"a"},{"role":"user","content":"b"}]}`,
		"dup-item-type2":  `{"input":[{"type":"message","type":"compaction_trigger"},{"role":"user","content":"b"}]}`,
		"escaped-name":    `{"in\u0070ut":[{"t\u0079pe":"compaction\u005ftrigger"},{"role":"user","content":"b\u00e9"}]}`,
		"non-string":      `{"input":[{"type":1},{"type":"compaction_trigger"},"s",null,[1]]}`,
		"no-trigger":      `{"input":[{"type":"message","content":"compaction_trigger"}]}`,
	} {
		raw, rawChanged, rawErr := NormalizeCompactionTriggerInputOrder([]byte(body))
		dec, decChanged, decErr := normalizeCompactionTriggerInputOrderDecoded([]byte(body))
		require.NoError(t, rawErr, name)
		require.NoError(t, decErr, name)
		require.Equal(t, decChanged, rawChanged, name)
		if !rawChanged {
			require.Equal(t, body, string(raw), name)
			continue
		}
		var got, want any
		require.NoError(t, decodeOpenAIJSONUseNumber(raw, &got), name)
		require.NoError(t, decodeOpenAIJSONUseNumber(dec, &want), name)
		require.Equal(t, want, got, "%s: %s", name, raw)
	}
	_, _, err := NormalizeCompactionTriggerInputOrder([]byte(`{"input":[`))
	require.Error(t, err, "an invalid body keeps the decoder's error")
}
