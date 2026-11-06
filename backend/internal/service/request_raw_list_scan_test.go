package service

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// rawBodyPathFiles: the files whose functions decode, convert, rewrite or
// route an OpenAI Responses request body between the ingest and the
// upstream send (Forward and the helpers it hands the body to, the
// passthrough path and the handler's failover body). Every account field,
// method or Extra key they read must be in rawDecisiveAccountReads or
// rawInertAccountReads (TestRawBodyPathAccountReadsAreClassified).
var rawBodyPathFiles = []string{
	"openai_gateway_forward.go",
	"openai_gateway_request_body.go",
	"openai_gateway_passthrough.go",
	"openai_request_skeleton.go",
	"openai_request_raw_input.go",
	"openai_responses_tool_schema.go",
	"openai_responses_lite_tools.go",
	"openai_lite_mapped_gpt55.go",
	"openai_responses_namespace.go",
	"openai_responses_item_id.go",
	"openai_responses_ingress_compat.go",
	"openai_responses_rejected_field_retry.go",
	"openai_codex_tool_names.go",
	"openai_codex_fingerprint.go",
	"openai_codex_account_identity.go",
	"openai_agent_identity.go",
	"openai_compact_body_signal.go",
	"openai_compact_fallback.go",
	"openai_content_session_seed.go",
	"openai_oauth_web_search_history.go",
	"openai_body_spool.go",
	"image_generation_intent.go",
	"openai_client_restriction_detector.go",
	"openai_routing_hint.go",
	"openai_ws_execution_scope.go",
	"openai_opencode_session.go",
	"openai_gateway_ollama_cloud_max_tokens.go",
	"request_conversion_charge.go",
	"../handler/openai_gateway_reasoning_failover.go",
}

// rawAccountReads parses the body-path files and returns each account read
// ("Field", "Method" or "Extra[key]") with its positions. An account value
// is an identifier named account, or a parameter or receiver of type
// Account / *Account; methods of Account itself are units (not entered).
func rawAccountReads(t *testing.T) map[string][]string {
	t.Helper()
	fset := token.NewFileSet()
	reads := map[string][]string{}
	for _, name := range rawBodyPathFiles {
		src, err := os.ReadFile(filepath.FromSlash(name))
		require.NoError(t, err)
		file, err := parser.ParseFile(fset, name, src, 0)
		require.NoError(t, err)
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			names := map[string]bool{"account": true}
			fields := fn.Type.Params.List
			if fn.Recv != nil {
				if isAccountTypeExpr(fn.Recv.List[0].Type) {
					continue
				}
				fields = append(append([]*ast.Field{}, fn.Recv.List...), fields...)
			}
			for _, f := range fields {
				if isAccountTypeExpr(f.Type) {
					for _, n := range f.Names {
						names[n.Name] = true
					}
				}
			}
			var stack []ast.Node
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				if n == nil {
					stack = stack[:len(stack)-1]
					return true
				}
				stack = append(stack, n)
				sel, ok := n.(*ast.SelectorExpr)
				if !ok || !isAccountIdent(sel.X, names) {
					return true
				}
				name := sel.Sel.Name
				if (name == "Extra" || name == "Credentials") && len(stack) > 1 {
					switch p := stack[len(stack)-2].(type) {
					case *ast.IndexExpr: // account.Extra["key"] or account.Extra[KeyConst]
						switch k := p.Index.(type) {
						case *ast.BasicLit:
							key, _ := strconv.Unquote(k.Value)
							name += "[" + key + "]"
						case *ast.Ident:
							name += "[" + k.Name + "]"
						}
					case *ast.CallExpr: // the whole map handed to a reader
						if p.Fun != sel {
							name += "->" + calleeName(p)
						}
					}
				}
				if len(stack) > 1 && strings.HasPrefix(name, "Get") {
					if p, ok := stack[len(stack)-2].(*ast.CallExpr); ok && p.Fun == sel && len(p.Args) > 0 {
						if lit, ok := p.Args[0].(*ast.BasicLit); ok && lit.Kind == token.STRING {
							key, _ := strconv.Unquote(lit.Value)
							name += "(" + key + ")" // GetCredential("base_url")
						}
					}
				}
				reads[name] = append(reads[name], fset.Position(sel.Pos()).String())
				return true
			})
		}
	}
	return reads
}

func isAccountTypeExpr(e ast.Expr) bool {
	if s, ok := e.(*ast.StarExpr); ok {
		e = s.X
	}
	switch x := e.(type) {
	case *ast.Ident:
		return x.Name == "Account"
	case *ast.SelectorExpr:
		return x.Sel.Name == "Account"
	}
	return false
}

func isAccountIdent(e ast.Expr, names map[string]bool) bool {
	id, ok := e.(*ast.Ident)
	return ok && names[id.Name]
}

// TestRawBodyPathAccountReadsAreClassified (rr6 fail-safe): a body-path
// file may read only the account fields, methods and Extra keys listed as
// decisive (checked by IsVerifiedRawAccount at their verified value) or
// inert (never change how the body is decoded, converted, rewritten or
// routed). A new read fails here until it is classified.
func TestRawBodyPathAccountReadsAreClassified(t *testing.T) {
	reads := rawAccountReads(t)
	var unlisted, stale []string
	for name, at := range reads {
		_, decisive := rawDecisiveAccountReads[name]
		_, inert := rawInertAccountReads[name]
		if !decisive && !inert {
			unlisted = append(unlisted, name+" at "+strings.Join(at, ", "))
		}
	}
	for name := range rawDecisiveAccountReads {
		if _, ok := reads[name]; !ok {
			stale = append(stale, "decisive "+name)
		}
	}
	for name := range rawInertAccountReads {
		if _, ok := reads[name]; !ok {
			stale = append(stale, "inert "+name)
		}
	}
	sort.Strings(unlisted)
	sort.Strings(stale)
	require.Empty(t, unlisted, "body-path account reads that are neither decisive nor inert")
	require.Empty(t, stale, "classified account reads no body-path file makes")
}
