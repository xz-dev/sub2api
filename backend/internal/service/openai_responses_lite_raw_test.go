//go:build unit

package service

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestResponsesLiteParallelToolCallsRawMatchesDecoded (review rr5): the
// API-key Responses-Lite rewrite on raw JSON gives the same changed flag,
// the same error and the same decoded value as the decoding form, and takes
// the top-up only for a body the raw form does not take.
func TestResponsesLiteParallelToolCallsRawMatchesDecoded(t *testing.T) {
	bodies := []string{
		`{"model":"m","input":[{"role":"user","content":"a"}]}`,
		`{"model":"m","parallel_tool_calls":true,"input":[]}`,
		`{"model":"m","parallel_tool_calls":false,"input":[]}`,
		`{"model":"m","parallel_tool_calls":"yes"}`,
		`{"model":"m","parallel_tool_calls":null}`,
		`{"model":"m","parallel_tool_calls":0}`,
		`{}`,
		`  { }  `,
		"\n{\n \"model\" : \"m\" ,\"parallel_tool_calls\" : true }\n",
		`{"parallel_tool_c\u0061lls":true,"model":"m"}`,
		`{"model":"m","parallel_tool_calls":true,"parallel_tool_calls":false}`,
		`{"model":"m","parallel_tool_calls":false,"parallel_tool_calls":true}`,
		`{"model":"m","model":"n"}`,
		`{"model":"m","input":[{"role":"user","content":"a","content":"b\u00e9 \"q\""}],"n":12345678901234567890}`,
		`{"model":"m","input":[{"role":"user","content":"a` + "\xff" + `"}]}`,
		`{"model":"m","tools":[{"type":"function","name":"f"}],"parallel_tool_calls":true}`,
		`{"model":"m"`,
		`[1]`,
		`"text"`,
		`{"model":"m"} {"x":1}`,
	}
	for _, body := range bodies {
		charged := 0
		charge := func() error { charged++; return nil }
		rawOut, rawChanged, rawErr := normalizeOpenAIResponsesLiteParallelToolCallsPayload([]byte(body), charge)
		decOut, decChanged, decErr := normalizeOpenAIResponsesLiteParallelToolCallsDecoded([]byte(body), NoConversionCharge)
		require.Equal(t, decChanged, rawChanged, body)
		require.Equal(t, decErr == nil, rawErr == nil, "%s: %v / %v", body, decErr, rawErr)
		if decErr != nil {
			require.Equal(t, decErr.Error(), rawErr.Error(), body)
			continue
		}
		if !rawChanged {
			require.Equal(t, body, string(rawOut), body)
			continue
		}
		var want, got any
		require.NoError(t, decodeOpenAIJSONUseNumber(decOut, &want), body)
		require.NoError(t, decodeOpenAIJSONUseNumber(rawOut, &got), "%s -> %s", body, rawOut)
		require.Equal(t, want, got, "%s -> %s", body, rawOut)
		raw := parseRawJSONView([]byte(body))
		var scratch []string
		if raw.IsObject() && !hasDuplicateJSONObjectKeysIn(raw, &scratch) {
			require.Zero(t, charged, "a unique-root object takes the raw form, no top-up: %s", body)
		}
	}

	// A refused top-up on the decoding form is returned as is.
	refused := func() error { return ErrRequestConversionRefused }
	_, _, err := normalizeOpenAIResponsesLiteParallelToolCallsPayload([]byte(`{"a":1,"a":2}`), refused)
	require.True(t, errors.Is(err, ErrRequestConversionRefused))
	out, changed, err := normalizeOpenAIResponsesLiteParallelToolCallsPayload([]byte(`{"a":1}`), refused)
	require.NoError(t, err)
	require.True(t, changed)
	require.JSONEq(t, `{"a":1,"parallel_tool_calls":false}`, string(out))
}
