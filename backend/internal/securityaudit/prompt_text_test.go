package securityaudit

import (
	"encoding/json"
	"math/rand"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func responsesTextBody(parts ...[2]string) []byte {
	type content struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	type item struct {
		Type    string    `json:"type"`
		Role    string    `json:"role"`
		Content []content `json:"content"`
	}
	input := make([]item, 0, len(parts))
	for _, p := range parts {
		typ := "input_text"
		if p[0] == "assistant" {
			typ = "output_text"
		}
		input = append(input, item{Type: "message", Role: p[0], Content: []content{{Type: typ, Text: p[1]}}})
	}
	body, _ := json.Marshal(map[string]any{"model": "m", "instructions": "be brief", "input": input})
	return body
}

// TestPromptSnapshotUnchangedByInPlaceText: the snapshot the guard gets (scan
// text, chunks, hash, length, count, stored prompt and preview) equals the
// one the copying code built, in both modes.
func TestPromptSnapshotUnchangedByInPlaceText(t *testing.T) {
	long := strings.Repeat("prose with a token=abcdefgh12345678 and mail a.b@example.com ", 3000) // > 64 KiB and > 65536 runes
	bodies := map[string][]byte{
		"single":    responsesTextBody([2]string{"user", "hello world"}),
		"long":      responsesTextBody([2]string{"user", long}),
		"spaces":    responsesTextBody([2]string{"user", "\u00a0 \t lead\x00ing \x00 nul\u0085 "}, [2]string{"assistant", " \x00 "}, [2]string{"user", "\x00x"}),
		"turns":     responsesTextBody([2]string{"user", "u1"}, [2]string{"assistant", "a1"}, [2]string{"user", "u2 Bearer abc.def"}, [2]string{"user", "u3 " + long}, [2]string{"assistant", "a2"}, [2]string{"user", "u4"}, [2]string{"user", "u5 +1 (555) 123-4567"}),
		"no-user":   responsesTextBody([2]string{"assistant", "only output"}, [2]string{"assistant", long}),
		"exact-max": responsesTextBody([2]string{"user", strings.Repeat("é", DefaultFullPromptMaxRunes) + "   "}),
		"max+1":     responsesTextBody([2]string{"user", strings.Repeat("é", DefaultFullPromptMaxRunes) + "  x"}),
	}
	for name, body := range bodies {
		for _, latest := range []bool{false, true} {
			req := Request{Protocol: "openai_responses", Body: body, Model: "m"}
			got, err := ExtractBlockingPromptSnapshot(req, latest)
			require.NoError(t, err, name)
			want, err := legacyExtractPromptSnapshot(req, latest)
			require.NoError(t, err, name)
			require.Equal(t, want.ScanText, got.ScanText, "%s latest=%v", name, latest)
			require.Equal(t, want.PromptHash, got.PromptHash, "%s latest=%v", name, latest)
			require.Equal(t, want.PromptLength, got.PromptLength, "%s latest=%v", name, latest)
			require.Equal(t, want.MessageCount, got.MessageCount, "%s latest=%v", name, latest)
			require.Equal(t, want.FullPrompt, got.FullPrompt, "%s latest=%v", name, latest)
			require.Equal(t, want.RedactedPreview, got.RedactedPreview, "%s latest=%v", name, latest)
			require.Equal(t, legacyFullPromptFromScanText(got.ScanText), FullPromptFromScanText(got.ScanText), name)
			for _, limit := range []int{1, 7, 128, DefaultInputLimit} {
				require.Equal(t, legacySplitRunes(got.ScanText, limit), SplitRunes(got.ScanText, limit), "%s limit=%d", name, limit)
			}
		}
	}
}

// TestPromptTextHelpersMatchLegacy: TrimRunes, SplitRunes and
// BuildFullPrompt equal the []rune-copying versions on random text with
// invalid UTF-8, NULs, unicode spaces and the priority separator.
func TestPromptTextHelpersMatchLegacy(t *testing.T) {
	alphabet := []string{"a", "é", "中", "😀", " ", "\t", "\u00a0", "\u0085", "\u2003", "\x00", "\xff", "\xe4", "\xb8\xad", "\n", promptAuditPrioritySeparator}
	rng := rand.New(rand.NewSource(1))
	for i := 0; i < 5000; i++ {
		var b strings.Builder
		for n := rng.Intn(40); n > 0; n-- {
			b.WriteString(alphabet[rng.Intn(len(alphabet))])
		}
		s := b.String()
		for _, limit := range []int{0, 1, 2, 3, 5, 17} {
			require.Equal(t, legacyTrimRunes(s, limit), TrimRunes(s, limit), "%q %d", s, limit)
			require.Equal(t, legacySplitRunes(s, limit), SplitRunes(s, limit), "%q %d", s, limit)
			require.Equal(t, legacyBuildFullPrompt(s, limit), BuildFullPrompt(s, limit), "%q %d", s, limit)
		}
		require.Equal(t, legacyFullPromptFromScanText(s), FullPromptFromScanText(s), "%q", s)
	}
}
