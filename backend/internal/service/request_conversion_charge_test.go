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

// requestConverters are the apicompat functions that rewrite a request body
// into another protocol: each allocates a multiple of the body.
var requestConverters = map[string]bool{
	"ResponsesToChatCompletionsRequestWithOptions": true,
	"ResponsesToAnthropicRequest":                  true,
	"ChatCompletionsToResponses":                   true,
	"AnthropicToResponses":                         true,
	"AnthropicToChatCompletionsRequest":            true,
}

type serviceFunc struct {
	name   string
	pos    string
	gin    bool                 // has a *gin.Context parameter
	calls  map[string]token.Pos // first call of each callee name
	charge token.Pos            // first ChargeRequestConversion call, 0 if none
}

// TestRequestConvertersChargeFirst (review rr3 blocker 2): every function of
// this package that reaches a request converter takes the converter top-up
// (ChargeRequestConversion) before it. A helper without a *gin.Context that
// converts passes the duty to its callers, transitively. A new converter
// entry point that skips the top-up fails here.
func TestRequestConvertersChargeFirst(t *testing.T) {
	funcs := parseServiceFuncs(t)
	converting := map[string]bool{} // apicompat converters and helpers that reach one
	for name := range requestConverters {
		converting[name] = true
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
			if !f.gin {
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
		case f.gin:
			failures = append(failures, f.pos+" "+f.name)
		}
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
