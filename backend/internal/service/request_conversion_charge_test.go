package service

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// requestConverters are the functions that rewrite a request body into
// another protocol, or decode all of it: each allocates a multiple of the
// body. apicompat and antigravity converters, the Claude -> Gemini converter
// of this package (review rr4 blocker c), and the full decode of an OpenAI
// Responses body (getOpenAIRequestBodyMap, reached from Forward's
// ensureReqBody: the Codex OAuth transform and the other decoded rewrites).
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
	"getOpenAIRequestBodyMap":                      true,
}

type serviceFunc struct {
	name   string
	pos    string
	gin    bool                 // has a *gin.Context parameter
	calls  map[string]token.Pos // first call of each callee name
	charge token.Pos            // first ChargeRequestConversion call, 0 if none
}

// chargeExempt: entry points that convert a body the gateway builds itself,
// never a client body, so no request charge exists to top up.
var chargeExempt = map[string]string{
	"testAntigravityAccountConnection": "admin account test: a fixed small test request",
}

// TestRequestConvertersChargeFirst (review rr3 blocker 2, rr4 blocker c):
// every function of this package that reaches a request converter takes the
// converter top-up (ChargeRequestConversion) before it. A helper that
// converts passes the duty to its callers, transitively: a helper is a
// function without a *gin.Context, or one this package calls (by name). An
// entry point (a *gin.Context function no code here calls, as the handlers
// call Forward) that skips the top-up fails here.
func TestRequestConvertersChargeFirst(t *testing.T) {
	funcs := parseServiceFuncs(t)
	converting := map[string]bool{} // converters and helpers that reach one
	for name := range requestConverters {
		converting[name] = true
	}
	called := map[string]bool{}
	for _, f := range funcs {
		for callee := range f.calls {
			called[callee] = true
		}
	}
	var failures, entries []string
	for changed := true; changed; {
		changed = false
		for _, f := range funcs {
			if converting[f.name] {
				continue
			}
			first := token.NoPos
			for callee, pos := range f.calls {
				if converting[callee] && (first == token.NoPos || pos < first) {
					first = pos
				}
			}
			if first == token.NoPos || (f.charge != token.NoPos && f.charge < first) {
				continue
			}
			if !f.gin || (called[f.name] && chargeExempt[f.name] == "") {
				converting[f.name], changed = true, true
			}
		}
	}
	for _, f := range funcs {
		first := token.NoPos
		for callee, pos := range f.calls {
			if converting[callee] && (first == token.NoPos || pos < first) {
				first = pos
			}
		}
		switch {
		case first == token.NoPos:
		case f.charge != token.NoPos && f.charge < first:
			entries = append(entries, f.name)
		case f.gin && !called[f.name] && chargeExempt[f.name] == "":
			failures = append(failures, f.pos+" "+f.name)
		}
	}
	// The converters of this package still exist under these names, and are
	// called: a rename must update the list.
	for _, name := range []string{"convertClaudeMessagesToGeminiGenerateContent", "getOpenAIRequestBodyMap"} {
		declared, called := false, false
		for _, f := range funcs {
			declared = declared || f.name == name
			_, ok := f.calls[name]
			called = called || ok
		}
		require.True(t, declared && called, "converter %s is not declared and called in this package; update requestConverters", name)
	}
	sort.Strings(entries)
	sort.Strings(failures)
	t.Logf("converter entry points that take the top-up: %v", entries)
	require.Empty(t, failures, "these functions reach a request converter before ChargeRequestConversion")
	require.GreaterOrEqual(t, len(entries), 10)
}

func parseServiceFuncs(t *testing.T) []serviceFunc {
	t.Helper()
	fset := token.NewFileSet()
	paths, err := filepath.Glob("*.go")
	require.NoError(t, err)
	var out []serviceFunc
	for _, path := range paths {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		src, err := os.ReadFile(path)
		require.NoError(t, err)
		file, err := parser.ParseFile(fset, path, src, 0)
		require.NoError(t, err)
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			f := serviceFunc{name: fn.Name.Name, pos: fset.Position(fn.Pos()).String(), calls: map[string]token.Pos{}}
			for _, field := range fn.Type.Params.List {
				if star, ok := field.Type.(*ast.StarExpr); ok {
					if sel, ok := star.X.(*ast.SelectorExpr); ok && sel.Sel.Name == "Context" {
						if id, ok := sel.X.(*ast.Ident); ok && id.Name == "gin" {
							f.gin = true
						}
					}
				}
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				var name string
				switch fun := call.Fun.(type) {
				case *ast.Ident:
					name = fun.Name
				case *ast.SelectorExpr:
					name = fun.Sel.Name
				}
				// A helper without a *gin.Context takes the charge as a
				// chargeConversion func parameter.
				if (name == "ChargeRequestConversion" || name == "chargeConversion") && f.charge == token.NoPos {
					f.charge = call.Pos()
				}
				if _, seen := f.calls[name]; name != "" && !seen {
					f.calls[name] = call.Pos()
				}
				return true
			})
			out = append(out, f)
		}
	}
	return out
}
