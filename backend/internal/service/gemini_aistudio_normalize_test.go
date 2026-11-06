//go:build unit

package service

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

// normalizeGeminiRequestForAIStudioDecoded is the function before rr6 4b:
// it always decodes the whole body.
func normalizeGeminiRequestForAIStudioDecoded(body []byte) []byte {
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return body
	}
	tools, ok := payload["tools"].([]any)
	if !ok || len(tools) == 0 {
		return body
	}
	modified := false
	for _, rawTool := range tools {
		tool, ok := rawTool.(map[string]any)
		if !ok {
			continue
		}
		googleSearch, ok := tool["googleSearch"]
		if !ok {
			continue
		}
		if _, exists := tool["google_search"]; exists {
			continue
		}
		tool["google_search"] = googleSearch
		delete(tool, "googleSearch")
		modified = true
	}
	if !modified {
		return body
	}
	normalized, err := json.Marshal(payload)
	if err != nil {
		return body
	}
	return normalized
}

// TestNormalizeGeminiRequestForAIStudioRawPrecheckParity (rr6 4b): the raw
// pre-check gives the same bytes as the always-decoding form.
func TestNormalizeGeminiRequestForAIStudioRawPrecheckParity(t *testing.T) {
	bodies := []string{
		`{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`,
		`{"contents":[],"tools":[]}`,
		`{"contents":[],"tools":[{"functionDeclarations":[{"name":"f"}]}]}`,
		`{"contents":[],"tools":[{"googleSearch":{}}]}`,
		`{"contents":[],"tools":[{"googleSearch":{},"google_search":{}}]}`,
		`{"contents":[],"tools":[{"functionDeclarations":[]},{"googleSearch":{"x":1}}]}`,
		`{"contents":[],"tools":[{"googleSearch":{}}],"tools":[]}`,
		`{"contents":[],"tools":[],"tools":[{"googleSearch":{}}]}`,
		`{"contents":[],"tools":[{"googleSearch":1,"googleSearch":2}]}`,
		`{"contents":[],"tools":[1,"x",null,{"googleSearch":null}]}`,
		`{"contents":[],"tools":{"googleSearch":{}}}`,
		`{"contents":[],"tools":[{"googlesearch":{}}]}`,
		`{"contents":[],"tools":[{"googleSearch":{}}]`,
		`[{"tools":[{"googleSearch":{}}]}]`,
		`not json`,
		``,
	}
	for _, b := range bodies {
		require.Equal(t, string(normalizeGeminiRequestForAIStudioDecoded([]byte(b))), string(normalizeGeminiRequestForAIStudio([]byte(b))), b)
	}
}
