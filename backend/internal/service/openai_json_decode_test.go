//go:build unit

package service

import (
	"bytes"
	"encoding/json"
	jsonv2 "encoding/json/v2"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func legacyDecodeOpenAIJSONUseNumber(data []byte, dst any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := decoder.Decode(dst); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("multiple JSON values are not allowed")
		}
		return err
	}
	return nil
}

// TestDecodeOpenAIJSONUseNumberMatchesDecoder: the in-place fast path gives
// the same value and the same error as the legacy json.Decoder + UseNumber.
func TestDecodeOpenAIJSONUseNumberMatchesDecoder(t *testing.T) {
	cases := []string{
		`{"a":1,"b":-0,"c":1.50,"d":1e400,"e":-12.5E-3,"f":[1,2.0,{"g":3}],"h":null,"i":true,"j":"x"}`,
		`{"dup":1,"dup":"two","nested":{"k":1,"k":[2]}}`,
		"{\"bad\":\"\xff\xfe\",\"esc\":\"\\u00e9\\ud83d\\ude00\\n\\/\",\"lone\":\"\\ud800\"}",
		`{"input":[{"type":"message","content":[{"type":"input_text","text":"hi"}]}]}`,
		`  {"ws":1}  ` + "\n",
		`null`, `[]`, `"str"`, `12`, `{}`,
		`{"a":1}{"b":2}`, `{"a":1} x`, `{"a":}`, `{"a":1`, ``, `   `, `{"a":01}`, `{"a":NaN}`,
	}
	corpus, _ := filepath.Glob("../pkg/bodyspool/testdata/corpus/*.json")
	for _, f := range corpus {
		b, err := os.ReadFile(f)
		require.NoError(t, err)
		cases = append(cases, string(b))
	}
	for _, c := range cases[:5] {
		var m map[string]any
		require.NoError(t, jsonv2.Unmarshal([]byte(c), &m, useNumberOptions), "fast path must take valid bodies: %.80q", c)
	}
	for _, c := range cases {
		var got, want map[string]any
		gerr := decodeOpenAIJSONUseNumber([]byte(c), &got)
		werr := legacyDecodeOpenAIJSONUseNumber([]byte(c), &want)
		require.Equal(t, werr, gerr, "%.80q", c)
		require.Equal(t, want, got, "%.80q", c)
		var gotAny, wantAny any
		require.Equal(t, legacyDecodeOpenAIJSONUseNumber([]byte(c), &wantAny), decodeOpenAIJSONUseNumber([]byte(c), &gotAny), "%.80q", c)
		require.Equal(t, wantAny, gotAny, "%.80q", c)
		type typed struct {
			A     any            `json:"a"`
			Input []any          `json:"input"`
			M     map[string]any `json:"nested"`
		}
		var gotT, wantT typed
		require.Equal(t, legacyDecodeOpenAIJSONUseNumber([]byte(c), &wantT), decodeOpenAIJSONUseNumber([]byte(c), &gotT), "%.80q", c)
		require.Equal(t, wantT, gotT, "%.80q", c)
	}
	// A non-nil destination map is merged into by both, as v1 does.
	got := map[string]any{"keep": json.Number("1")}
	want := map[string]any{"keep": json.Number("1")}
	require.NoError(t, decodeOpenAIJSONUseNumber([]byte(`{"a":2}`), &got))
	require.NoError(t, legacyDecodeOpenAIJSONUseNumber([]byte(`{"a":2}`), &want))
	require.Equal(t, want, got)
}
