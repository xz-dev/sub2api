package service

import (
	"go/ast"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// requestConverters are the functions that rewrite a request body into
// another protocol, or decode all of it: each allocates a multiple of the
// body. apicompat and antigravity converters, the Claude -> Gemini converter
// of this package (review rr4 blocker c), the whole-body decodes of the
// Gemini native and Antigravity Gemini paths (review rr5), and the handler's
// Codex bootstrap scan.
var requestConverters = map[string]bool{
	"ResponsesToChatCompletionsRequest":            true,
	"ResponsesToChatCompletionsRequestWithOptions": true,
	"ResponsesToAnthropicRequest":                  true,
	"ChatCompletionsToResponses":                   true,
	"AnthropicToResponses":                         true,
	"AnthropicToChatCompletionsRequest":            true,
	"TransformClaudeToGemini":                      true,
	"TransformClaudeToGeminiWithOptions":           true,
	"convertClaudeMessagesToGeminiGenerateContent": true,
	"filterEmptyPartsFromGeminiRequest":            true,
	"ensureGeminiFunctionCallThoughtSignatures":    true,
	"injectIdentityPatchToGeminiRequest":           true,
	"cleanGeminiRequest":                           true,
	"enableMixedGeminiToolInvocations":             true,
	"wrapV1InternalRequest":                        true,
	"extractImageInputSize":                        true,
	"hasUniqueJSONMembers":                         true,
}

// chargerTaking are functions that take the k_conv charger as a parameter
// (index, receiver not counted) and call it before they allocate. The test
// adds every function that calls a func() error parameter or passes one on.
var chargerTaking = map[string]int{"DecodeFullRequestBody": 0}

// noChargeAllowed may pass NoConversionCharge or a charger the scan cannot
// follow: the body has no request charge, or the charger comes from a field.
var noChargeAllowed = map[string]string{
	"OpenAIGatewayService.ProxyResponsesWebSocketFromClient":    "WebSocket message: no request charge",
	"OpenAIGatewayService.proxyOpenAIWSHTTPBridgeTurn":          "WebSocket message bridged to HTTP: no request charge",
	"OpenAIGatewayService.proxyResponsesWebSocketV2Passthrough": "WebSocket message: no request charge",
	"normalizeOpenAIResponsesCompatibilityBodyWithOptions": "opts.ChargeConversion: RequestConversionCharger on HTTP " +
		"(openai_gateway_passthrough.go), NoConversionCharge for WebSocket",
}

// chargedByCallers: functions with a *gin.Context that convert and leave
// the top-up to their callers. They pass the duty on like a helper without
// a *gin.Context, so each caller is checked instead.
var chargedByCallers = map[string]string{
	"AntigravityGatewayService.forwardAntigravityCompat":             "ForwardAsChatCompletions / ForwardAsResponses charge first",
	"AntigravityGatewayService.prepareAntigravityCompatCall":         "ForwardAsChatCompletions / ForwardAsResponses charge first",
	"GeminiMessagesCompatService.forwardClaudeBodyAsChatCompletions": "ForwardAsChatCompletions / ForwardAsResponses charge first",
}

// chargeExempt: entry points that convert a body the gateway builds itself,
// never a client body, so no request charge exists to top up.
var chargeExempt = map[string]string{
	"AccountTestService.testAntigravityAccountConnection": "admin account test: a fixed small test request",
}

// TestRequestConvertersChargeFirst (review rr3 blocker 2, rr4 blocker c,
// rr5): in the service and handler packages, a converter call must come
// after a charge on every path. A charge is ChargeRequestConversion(...),
// or a call of a func() error parameter or of a local holding
// RequestConversionCharger(c). "On every path" is block dominance: the
// charge's block encloses the converter at a later statement, so a charge
// in another branch or a dead branch does not count. A function with a
// *gin.Context that misses it fails by name; one without passes the duty
// to its callers (by name, self-calls ignored).
//
// Second rule: every call of a charger-taking function (DecodeFullRequestBody
// and the functions that pass their charger on) passes
// RequestConversionCharger(...), a local holding one, its own charger
// parameter, or, only where noChargeAllowed says so, NoConversionCharge.
//
// What it cannot see (the charge-cover property test is the primary gate):
// a decode not named here (409 json.Unmarshal sites; only listed converters
// and DecodeFullRequestBody are tracked); a charge whose error is ignored;
// a charger passed through a struct field, a closure variable or a function
// value; calls through an interface or a method value; a closure defined
// before the charge and called after it (read as before); name collisions
// (calls are matched by bare name).
func TestRequestConvertersChargeFirst(t *testing.T) {
	failures, entries := chargeFirstFailures(scanChargeFuncs(t, nil, ".", "../handler"))
	t.Logf("converter entry points that take the top-up first: %v", entries)
	for _, f := range failures {
		t.Errorf("charge missing: %s", f)
	}
	require.GreaterOrEqual(t, len(entries), 10)
}

// chargeFirstFailures applies both rules to funcs; it returns the failures
// (each names the function, its position, and the chain to the converter)
// and the entry points that charge first.
func chargeFirstFailures(funcs []*chargeScanFunc) (failures, entries []string) {
	converting := map[string]string{} // name -> the converter it reaches
	for name := range requestConverters {
		converting[name] = name
	}
	taking := map[string]int{}
	for name, idx := range chargerTaking {
		taking[name] = idx
	}
	// Fixpoint: helpers that convert without a charge, and functions that
	// take a charger.
	for changed := true; changed; {
		changed = false
		for _, f := range funcs {
			for _, c := range f.calls {
				if idx, ok := f.charger[c.name]; ok && len(c.args) == 0 {
					if _, seen := taking[f.bare]; !seen {
						taking[f.bare], changed = idx, true
					}
				}
				if idx, ok := taking[c.name]; ok && idx < len(c.args) && c.name != f.bare {
					if id, ok := c.args[idx].(*ast.Ident); ok && hasKey(f.charger, id.Name) {
						if _, seen := taking[f.bare]; !seen {
							taking[f.bare], changed = f.charger[id.Name], true
						}
					}
				}
				via, conv := converting[c.name]
				if !conv || c.name == f.bare || (f.gin && chargedByCallers[f.name] == "") || f.chargedBefore(c.chargeScanSite) {
					continue
				}
				if _, seen := converting[f.bare]; !seen {
					converting[f.bare], changed = c.name+chainSuffix(c.name, via), true
				}
			}
		}
	}
	for _, f := range funcs {
		reached := false
		var missing []string
		for _, c := range f.calls {
			if via, conv := converting[c.name]; conv && c.name != f.bare && f.gin && chargedByCallers[f.name] == "" {
				reached = true
				if m := c.name + chainSuffix(c.name, via); !f.chargedBefore(c.chargeScanSite) && chargeExempt[f.name] == "" && !slices.Contains(missing, m) {
					missing = append(missing, m)
				}
			}
			idx, ok := taking[c.name]
			if !ok || idx >= len(c.args) || c.name == f.bare {
				continue
			}
			if why := chargerArgProblem(f, c.args[idx]); why != "" {
				failures = append(failures, f.pos+" "+f.name+": passes "+why+" as the charger of "+c.name)
			}
		}
		if len(missing) > 0 {
			failures = append(failures, f.pos+" "+f.name+": calls "+strings.Join(missing, ", ")+
				" with no ChargeRequestConversion before on every path")
		} else if reached && chargeExempt[f.name] == "" {
			entries = append(entries, f.name)
		}
	}
	sort.Strings(entries)
	sort.Strings(failures)
	return failures, entries
}

// chainSuffix: " -> rest" when name is a helper whose path is via.
func chainSuffix(name, via string) string {
	if via == name {
		return ""
	}
	return " -> " + via
}

// chargerArgProblem: "" when arg is a charger f may pass, else what it is.
func chargerArgProblem(f *chargeScanFunc, arg ast.Expr) string {
	switch a := arg.(type) {
	case *ast.CallExpr:
		if calleeName(a) == "RequestConversionCharger" {
			return ""
		}
	case *ast.Ident:
		if f.locals[a.Name] || hasKey(f.charger, a.Name) {
			return ""
		}
	}
	if noChargeAllowed[f.name] != "" {
		return ""
	}
	switch a := arg.(type) {
	case *ast.Ident:
		return a.Name
	case *ast.SelectorExpr:
		return a.Sel.Name
	}
	return "an expression"
}

// getOpenAIRequestBodyMap is the full decode without a request charge, for
// tests.
func getOpenAIRequestBodyMap(_ *gin.Context, body []byte) (map[string]any, error) {
	return newOpenAIRequestView(body).DecodeFullBody(NoConversionCharge)
}
