package service

import (
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestRequestConvertersChargeFirstMutations: each mutation of the real
// source must fail TestRequestConvertersChargeFirst's rules, and the
// failure must name the mutated function (review rr5 item 6).
func TestRequestConvertersChargeFirstMutations(t *testing.T) {
	const charge = "if err := ChargeRequestConversion(c); err != nil {\n\t\treturn nil, err\n\t}"
	mutations := []struct {
		name, file, old, new, want string
	}{
		{"main-path charge removed, a branch still charges", "openai_gateway_chat_completions.go",
			"\t// Chat -> Responses: a protocol converter, its charge first (review rr3).\n\t" + charge, "",
			"OpenAIGatewayService.forwardAsChatCompletions"},
		{"converter before the charge in self-recursive ForwardAsAnthropic", "openai_gateway_messages.go",
			"\t// Anthropic -> Responses: a protocol converter, its charge first (review rr3).\n",
			"\tAnthropicToResponses(nil)\n", "OpenAIGatewayService.ForwardAsAnthropic"},
		{"charge in a dead branch", "gemini_messages_compat_service.go",
			"(review rr5). A refusal is written\n\t// and final.\n\t" + charge,
			"\n\tif false {\n\t" + charge + "\n\t}", "GeminiMessagesCompatService.ForwardNative"},
		{"conditional charge: if cond && ChargeRequestConversion", "gemini_messages_compat_service.go",
			"(review rr5). A refusal is written\n\t// and final.\n\t" + charge,
			"\n\tif len(body) > 1<<30 && ChargeRequestConversion(c) != nil {\n\t\treturn nil, ErrRequestConversionRefused\n\t}",
			"GeminiMessagesCompatService.ForwardNative"},
		{"conditional charge: if cond || ChargeRequestConversion", "gemini_messages_compat_service.go",
			"(review rr5). A refusal is written\n\t// and final.\n\t" + charge,
			"\n\tif len(body) == 0 || ChargeRequestConversion(c) != nil {\n\t\treturn nil, ErrRequestConversionRefused\n\t}",
			"GeminiMessagesCompatService.ForwardNative"},
		{"Gemini Forward charge removed", "gemini_messages_compat_service.go",
			"(review rr4: 35.76 x on small Claude messages).\n\t" + charge, "", "GeminiMessagesCompatService.Forward"},
		{"Antigravity Claude Forward charge removed", "antigravity_gateway_claude.go",
			"charge before the body is decoded (review rr4).\n\t" + charge, "", "AntigravityGatewayService.Forward"},
		{"Antigravity ForwardGemini charge removed", "antigravity_gateway_gemini.go",
			"refusal is written and final.\n\t" + charge, "", "AntigravityGatewayService.ForwardGemini"},
		{"ensureReqBody decodes without a charge", "openai_gateway_forward.go",
			"requestView.DecodeFullBody(RequestConversionCharger(c))", "requestView.DecodeFullBody(NoConversionCharge)",
			"OpenAIGatewayService.Forward"},
		{"Responses-Lite without a charge", "openai_gateway_forward.go",
			"normalizeOpenAIResponsesLitePayloadForAccount(body, account, RequestConversionCharger(c))",
			"normalizeOpenAIResponsesLitePayloadForAccount(body, account, nil)", "OpenAIGatewayService.Forward"},
		{"lineage strip without a charge", "openai_gateway_forward.go",
			`"invalid_encrypted_lineage_strip", account.ID, 0, RequestConversionCharger(c)`,
			`"invalid_encrypted_lineage_strip", account.ID, 0, NoConversionCharge`, "OpenAIGatewayService.Forward"},
		{"namespace flatten without a charge", "openai_responses_namespace.go",
			"DecodeFullRequestBody(RequestConversionCharger(c), body, &requestBody)",
			"DecodeFullRequestBody(NoConversionCharge, body, &requestBody)", "flattenOpenAIResponsesNamespaces"},
		{"handler Codex bootstrap without a charge", "../handler/openai_gateway_handler.go",
			"chargeDecode := service.RequestConversionCharger(c)", "chargeDecode := service.NoConversionCharge",
			"OpenAIGatewayHandler.Responses"},
		{"Grok count_tokens estimate without a charge", "../handler/openai_gateway_count_tokens.go",
			"\tif err := service.ChargeRequestConversion(c); err != nil {\n\t\treturn\n\t}\n\testimated", "\testimated",
			"OpenAIGatewayHandler.GrokCountTokens"},
	}
	for _, m := range mutations {
		t.Run(m.name, func(t *testing.T) {
			src, err := os.ReadFile(m.file)
			require.NoError(t, err)
			require.Equal(t, 1, strings.Count(string(src), m.old), "mutation anchor in %s", m.file)
			mutated := strings.Replace(string(src), m.old, m.new, 1)
			failures, _ := chargeFirstFailures(scanChargeFuncs(t, map[string]string{m.file: mutated}, ".", "../handler"))
			require.NotEmpty(t, failures, "the mutation passed")
			for _, f := range failures {
				require.Contains(t, f, " "+m.want+":", "a failure that does not name the mutated function")
			}
			t.Logf("%s", failures)
		})
	}
}
