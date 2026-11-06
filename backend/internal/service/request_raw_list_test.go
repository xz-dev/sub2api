//go:build unit

package service

import (
	"net/http"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/openai_compat"
	"github.com/stretchr/testify/require"
)

// TestVerifiedRawResponsesRequest (rr6 fail-safe): the request half of the
// verified-raw list; anything not listed is not raw.
func TestVerifiedRawResponsesRequest(t *testing.T) {
	const pi = `{"type":"reasoning","id":"rs_1","summary":[{"type":"summary_text","text":"s"}],"encrypted_content":"gAAA","content":[]}`
	cases := []struct {
		name, body string
		header     map[string]string
		ok         bool
	}{
		{"pi production reasoning", `{"model":"m","input":[` + pi + `,{"role":"user","content":"hi"}],"stream":true}`, nil, true},
		{"string input", `{"model":"m","input":"hi"}`, nil, true},
		{"reasoning content null", `{"input":[{"type":"reasoning","content":null}]}`, nil, true},
		{"reasoning content spaced empty", `{"input":[{"type":"reasoning","content":[ ]}]}`, nil, true},
		{"message parts and tools", `{"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"a"},{"type":"input_image","image_url":"data:image/png;base64,QUJD"}]},{"type":"function_call","call_id":"c","name":"f","arguments":"{}"},{"type":"function_call_output","call_id":"c","output":"o"}],"tools":[{"type":"function","name":"f","parameters":{}},{"type":"custom","name":"apply_patch"}]}`, nil, true},
		{"lite header", `{"input":[]}`, map[string]string{responsesLiteHeader: "true"}, true},
		{"reasoning with content", `{"input":[{"type":"reasoning","content":[{"type":"reasoning_text","text":"t"}]}]}`, nil, false},
		{"unknown item type", `{"input":[{"type":"compaction_trigger"}]}`, nil, false},
		{"item without type or role", `{"input":[{"content":"x"}]}`, nil, false},
		{"namespace item", `{"input":[{"type":"message","role":"user","namespace":"codex_tui","content":"x"}]}`, nil, false},
		{"duplicate item type: last wins", `{"input":[{"type":"message","type":"compaction_trigger"}]}`, nil, false},
		{"tool_search tool", `{"tools":[{"type":"tool_search"}]}`, nil, false},
		{"image_generation tool", `{"tools":[{"type":"image_generation"}]}`, nil, false},
		{"namespace tool", `{"tools":[{"type":"namespace","tools":[]}]}`, nil, false},
		{"unknown root member", `{"input":[],"prompt":{"id":"p"}}`, nil, false},
		{"duplicate root member", `{"store":false,"store":false}`, nil, false},
		{"escaped root name", `{"mod\u0065l":"m"}`, nil, false},
		{"escaped item member name", `{"input":[{"typ\u0065":"compaction_trigger","role":"user"}]}`, nil, false},
		{"empty base64 image", `{"input":[{"role":"user","content":[{"type":"input_image","image_url":"data:image/png;base64,"}]}]}`, nil, false},
		{"unknown part type", `{"input":[{"role":"user","content":[{"type":"input_audio","input_audio":{"data":"QUJD"}}]}]}`, nil, false},
		{"unknown role", `{"input":[{"role":"tool","content":"x"}]}`, nil, false},
		{"not an object", `[1]`, nil, false},
		{"invalid json", `{"input":[}`, nil, false},
	}
	for _, tc := range cases {
		h := http.Header{}
		for k, v := range tc.header {
			h.Set(k, v)
		}
		ok, reason := VerifiedRawResponsesRequest([]byte(tc.body), h)
		require.Equal(t, tc.ok, ok, "%s: %s", tc.name, reason)
		if !ok {
			require.NotEmpty(t, reason, tc.name)
		}
	}
}

// TestIsVerifiedRawAccount: only the measured account shape is raw.
func TestIsVerifiedRawAccount(t *testing.T) {
	base := func() *Account {
		return &Account{Platform: PlatformOpenAI, Type: AccountTypeAPIKey,
			Credentials: map[string]any{"api_key": "k"},
			Extra:       map[string]any{openai_compat.ExtraKeyResponsesMode: "force_responses", "openai_passthrough": false}}
	}
	ok, reason := IsVerifiedRawAccount(base(), OpenAIUpstreamTransportHTTPSSE)
	require.True(t, ok, reason)
	for name, mutate := range map[string]func(a *Account){
		"oauth":       func(a *Account) { a.Type = AccountTypeOAuth },
		"passthrough": func(a *Account) { a.Extra["openai_passthrough"] = true },
		"auto mode":   func(a *Account) { delete(a.Extra, openai_compat.ExtraKeyResponsesMode) },
		"force chat":  func(a *Account) { a.Extra[openai_compat.ExtraKeyResponsesMode] = "force_chat_completions" },
		"anthropic":   func(a *Account) { a.Platform = PlatformAnthropic },
		"flatten":     func(a *Account) { a.Extra["openai_responses_flatten_namespaces"] = true },
	} {
		a := base()
		mutate(a)
		ok, _ := IsVerifiedRawAccount(a, OpenAIUpstreamTransportHTTPSSE)
		require.False(t, ok, name)
	}
	ok, _ = IsVerifiedRawAccount(base(), OpenAIUpstreamTransportResponsesWebsocketV2)
	require.False(t, ok, "WS mode on")
}
