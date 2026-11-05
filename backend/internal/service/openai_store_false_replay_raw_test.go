//go:build unit

package service

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestStoreFalseReplayRawMatchesDecoded (review rr4 blocker a): bodies with
// duplicate root or item names take the raw path, with the decoded form's
// changed flag and a body that decodes to the same value.
func TestStoreFalseReplayRawMatchesDecoded(t *testing.T) {
	rs := `{"type":"reasoning","id":"rs_1","summary":null,"encrypted_content":"e"}`
	for name, body := range map[string]string{
		"dup-root-unchanged": `{"store":false,"model":"m","store":false,"input":[{"role":"user","content":"a"}]}`,
		"dup-root-reasoning": `{"store":false,"model":"m","store":false,"input":[{"role":"user","content":"a"},` + rs + `]}`,
		"dup-root-drop":      `{"store":false,"input":[{"type":"reasoning","id":"rs_2"},{"type":"item_reference","id":"rs_3"},{"role":"user","content":"a"}],"store":false}`,
		"dup-input-last":     `{"store":false,"input":[` + rs + `],"input":[{"role":"user","content":"a"}]}`,
		"dup-input-first":    `{"store":false,"input":[{"role":"user","content":"a"}],"input":[` + rs + `]}`,
		"dup-input-nonarray": `{"store":false,"input":[` + rs + `],"input":"text"}`,
		"dup-item-unchanged": `{"store":false,"input":[{"role":"user","role":"user","content":"a"},{"role":"user","content":"b"}]}`,
		"dup-item-reasoning": `{"store":false,"input":[{"type":"message","type":"reasoning","id":"rs_1","encrypted_content":"e"},{"role":"user","content":"b"}]}`,
		"dup-item-drop":      `{"store":false,"input":[{"type":"reasoning","encrypted_content":"e","encrypted_content":""},{"role":"user","content":"b"}]}`,
		"dup-item-not-rs":    `{"store":false,"input":[{"type":"reasoning","type":"message","id":"rs_1"},` + rs + `]}`,
		"dup-both":           `{"store":false,"input":[{"type":"function_call","call_id":"c","call_id":"d","name":"f"}],"store":false}`,
		"no-dup-reasoning":   `{"store":false,"input":[{"role":"user","content":"a"},` + rs + `]}`,
	} {
		raw, rawChanged, rawErr := normalizeOpenAIAPIKeyStoreFalseReasoningReplay([]byte(body), false)
		dec, decChanged, decErr := normalizeOpenAIAPIKeyStoreFalseReasoningReplayDecoded([]byte(body), false)
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
}
