package apicompat

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestRawArrayItemsMatchesUnmarshal: the zero-copy split gives exactly the
// items json.Unmarshal gives a []json.RawMessage, and refuses (so the caller
// falls back) whatever is not one valid array.
func TestRawArrayItemsMatchesUnmarshal(t *testing.T) {
	valid := []string{
		`[]`, ` [ ] `, `[1]`, `[ 1 , "a\"b" ,{"x": [1, {"y":null}] }, true,null ,-0.5e3 ]`,
		"[\"\xff\",{\"k\":1,\"k\":2},\"\\ud800\",\"\\u00e9\"]",
		"[\n\t{\"type\":\"function_call_output\",\"output\":\"" + string(make([]byte, 0)) + "x\"}\n]\n",
	}
	for _, c := range valid {
		got, ok := rawArrayItems([]byte(c))
		require.True(t, ok, c)
		var want []json.RawMessage
		require.NoError(t, json.Unmarshal([]byte(c), &want), c)
		require.Equal(t, want, got, c)
		for _, it := range got {
			require.Equal(t, len(it), cap(it), "items must not be appendable into raw")
		}
	}
	for _, c := range []string{``, `{}`, `"a"`, `[1,]`, `[1`, `[1] [2]`, `[1] x`, `[01]`, `{"a":[1]}`} {
		_, ok := rawArrayItems([]byte(c))
		require.False(t, ok, c)
	}
}
