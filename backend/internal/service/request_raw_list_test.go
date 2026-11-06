//go:build unit

package service

import (
	"net/http"
	"strings"
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
		{"namespace codex_app", `{"input":[{"type":"function_call_output","namespace":"codex_app","call_id":"c","output":"o"}]}`, nil, false},
		{"namespace escaped codex_tui", `{"input":[{"type":"message","role":"user","namespace":"codex\u005ftui","content":"x"}]}`, nil, false},
		{"namespace not a string", `{"input":[{"type":"function_call","namespace":1,"call_id":"c","name":"f","arguments":"{}"}]}`, nil, false},
		{"namespace other function_call", `{"input":[{"type":"function_call","namespace":"pi_tools","call_id":"c","name":"f","arguments":"{}"}]}`, nil, true},
		{"namespace other message", `{"input":[{"type":"message","role":"user","namespace":"codex_tuix","content":"x"}]}`, nil, true},
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

// TestVerifiedRawIdentifierCap (rr10): identifier members are capped at
// 4 KiB raw bytes; the off-list reason names the field. Content-bearing
// members are never capped.
func TestVerifiedRawIdentifierCap(t *testing.T) {
	at := strings.Repeat("a", rawIdentMaxBytes)
	over := at + "a"
	esc := strings.Repeat(`\u003c`, rawIdentMaxBytes/6+1) // 4 KiB of escapes as sent, 683 bytes decoded
	big := strings.Repeat(`\u003c`, 1<<16)
	item := func(member, v string) string {
		return `{"input":[{"type":"function_call","call_id":"c","name":"f","arguments":"{}",` + member + `:` + v + `}]}`
	}
	capped := []struct{ name, body, want string }{
		{"prompt_cache_key", `{"prompt_cache_key":"%s"}`, "root member prompt_cache_key: longer than 4096 bytes"},
		{"model", `{"model":"%s"}`, "root member model: longer than 4096 bytes"},
		{"user", `{"user":"%s"}`, "root member user: longer than 4096 bytes"},
		{"safety_identifier", `{"safety_identifier":"%s"}`, "root member safety_identifier: longer than 4096 bytes"},
		{"service_tier", `{"service_tier":"%s"}`, "root member service_tier: longer than 4096 bytes"},
		{"truncation", `{"truncation":"%s"}`, "root member truncation: longer than 4096 bytes"},
		{"previous_response_id", `{"previous_response_id":"%s"}`, "root member previous_response_id: longer than 4096 bytes"},
		{"conversation", `{"conversation":"%s"}`, "root member conversation: longer than 4096 bytes"},
		{"conversation.id", `{"conversation":{"id":"%s"}}`, "root member conversation: id longer than 4096 bytes"},
		{"reasoning.effort", `{"reasoning":{"effort":"%s"}}`, "root member reasoning: effort longer than 4096 bytes"},
		{"reasoning.summary", `{"reasoning":{"effort":"high","summary":"%s"}}`, "root member reasoning: summary longer than 4096 bytes"},
		{"metadata key", `{"metadata":{"%s":"v"}}`, "root member metadata: key longer than 4096 bytes"},
		{"metadata value", `{"metadata":{"k":"%s"}}`, "root member metadata: value longer than 4096 bytes"},
		{"item id", item(`"id"`, `"%s"`), "root member input: item id longer than 4096 bytes"},
		{"item call_id", `{"input":[{"type":"function_call_output","output":"o","call_id":"%s"}]}`, "root member input: item call_id longer than 4096 bytes"},
		{"item status", item(`"status"`, `"%s"`), "root member input: item status longer than 4096 bytes"},
		{"item name", `{"input":[{"type":"function_call","call_id":"c","arguments":"{}","name":"%s"}]}`, "root member input: item name longer than 4096 bytes"},
		{"item namespace", item(`"namespace"`, `"%s"`), "root member input: item namespace longer than 4096 bytes"},
		{"message id", `{"input":[{"type":"message","role":"user","content":"x","id":"%s"}]}`, "root member input: item id longer than 4096 bytes"},
	}
	for _, tc := range capped {
		ok, reason := VerifiedRawResponsesRequest([]byte(strings.Replace(tc.body, "%s", at, 1)), http.Header{})
		require.True(t, ok, "%s at the cap: %s", tc.name, reason)
		for _, v := range []string{over, esc, big} {
			ok, reason = VerifiedRawResponsesRequest([]byte(strings.Replace(tc.body, "%s", v, 1)), http.Header{})
			require.False(t, ok, tc.name)
			require.Equal(t, tc.want, reason)
		}
	}
	// Non-string identifier values count their raw text.
	ok, reason := VerifiedRawResponsesRequest([]byte(`{"user":["`+over+`"]}`), http.Header{})
	require.False(t, ok)
	require.Equal(t, "root member user: longer than 4096 bytes", reason)

	// Content-bearing members are not capped: a body-sized escaped value
	// in each stays on the list.
	uncapped := []string{
		`{"instructions":"%s","input":"%s"}`,
		`{"input":[{"type":"reasoning","id":"rs_1","summary":[{"type":"summary_text","text":"%s"}],"encrypted_content":"%s","content":[]}]}`,
		`{"input":[{"type":"function_call","call_id":"c","name":"f","arguments":"%s"},{"type":"function_call_output","call_id":"c","output":"%s"}]}`,
		`{"input":[{"type":"custom_tool_call","call_id":"c","name":"f","input":"%s"},{"type":"custom_tool_call_output","call_id":"c","output":"%s"}]}`,
		`{"input":[{"role":"developer","content":[{"type":"input_text","text":"%s"},{"type":"input_image","image_url":"data:image/png;base64,%s"}]}]}`,
		`{"input":[{"role":"user","content":[{"type":"input_file","file_data":"%s","filename":"f"}]}],"text":{"format":{"type":"json_schema","name":"s","schema":{"description":"%s"}}}}`,
		`{"tools":[{"type":"function","name":"f","description":"%s","parameters":{"type":"object","description":"%s"}}],"input":"q"}`,
		`{"input":[{"type":"configuration_update","reasoning":{"effort":"%s"}}],"reasoning":{"effort":"low","summary":"auto"},"metadata":{"k":"%s"}}`,
	}
	for i, body := range uncapped {
		if i == len(uncapped)-1 {
			body = strings.Replace(body, "%s", big, 1)
			body = strings.Replace(body, "%s", at, 1)
		} else {
			body = strings.ReplaceAll(body, "%s", big)
		}
		ok, reason := VerifiedRawResponsesRequest([]byte(body), http.Header{})
		require.True(t, ok, "uncapped %d: %s", i, reason)
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
