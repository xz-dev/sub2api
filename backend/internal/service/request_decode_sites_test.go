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

// scanDirectDecodes: for each function of dirs (test files excluded) with a
// direct JSON decode, its key ("<dir>/<file>:<Recv.>Name") and the decode
// positions. overlay replaces a file's source (by its glob path).
func scanDirectDecodes(t *testing.T, overlay map[string]string, dirs ...string) map[string][]string {
	t.Helper()
	fset := token.NewFileSet()
	out := map[string][]string{}
	for _, dir := range dirs {
		paths, err := filepath.Glob(filepath.Join(dir, "*.go"))
		require.NoError(t, err)
		abs, err := filepath.Abs(dir)
		require.NoError(t, err)
		for _, path := range paths {
			if strings.HasSuffix(path, "_test.go") {
				continue
			}
			src, err := os.ReadFile(path)
			require.NoError(t, err)
			if o, ok := overlay[path]; ok {
				src = []byte(o)
			}
			file, err := parser.ParseFile(fset, path, src, 0)
			require.NoError(t, err)
			for _, decl := range file.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok || fn.Body == nil {
					continue
				}
				key := filepath.Base(abs) + "/" + filepath.Base(path) + ":" + scanFuncName(fn)
				ast.Inspect(fn.Body, func(n ast.Node) bool {
					if call, ok := n.(*ast.CallExpr); ok && isDirectDecode(call) {
						out[key] = append(out[key], fset.Position(call.Pos()).String())
					}
					return true
				})
			}
		}
	}
	return out
}

func scanFuncName(fn *ast.FuncDecl) string {
	if fn.Recv != nil && len(fn.Recv.List) == 1 {
		recv := fn.Recv.List[0].Type
		if star, ok := recv.(*ast.StarExpr); ok {
			recv = star.X
		}
		if id, ok := recv.(*ast.Ident); ok {
			return id.Name + "." + fn.Name.Name
		}
	}
	return fn.Name.Name
}

// isDirectDecode: json.Unmarshal, jsonv2.Unmarshal, json.NewDecoder,
// decodeOpenAIJSONUseNumber, or a no-argument Value() (gjson.Result.Value
// builds Go values from the raw JSON).
func isDirectDecode(call *ast.CallExpr) bool {
	switch fun := call.Fun.(type) {
	case *ast.Ident:
		return fun.Name == "decodeOpenAIJSONUseNumber"
	case *ast.SelectorExpr:
		if fun.Sel.Name == "Value" && len(call.Args) == 0 {
			return true
		}
		x, ok := fun.X.(*ast.Ident)
		if !ok {
			return false
		}
		switch fun.Sel.Name {
		case "Unmarshal":
			return x.Name == "json" || x.Name == "jsonv2"
		case "NewDecoder":
			return x.Name == "json"
		}
	}
	return false
}

// decodeSiteProblems checks sites against requestDecodeSites: each failure
// names the function.
func decodeSiteProblems(sites map[string][]string) []string {
	var out []string
	for key, pos := range sites {
		v, ok := requestDecodeSites[key]
		if !ok {
			out = append(out, key+": direct decode at "+strings.Join(pos, ", ")+
				" outside DecodeFullRequestBody and outside requestDecodeSites")
			continue
		}
		if why := decodeClassProblem(key, v); why != "" {
			out = append(out, key+": "+why)
		}
	}
	for key := range requestDecodeSites {
		if _, ok := sites[key]; !ok {
			out = append(out, key+": listed in requestDecodeSites but has no direct decode")
		}
	}
	sort.Strings(out)
	return out
}

func decodeClassProblem(key, v string) string {
	class, rest, _ := strings.Cut(v, " ")
	class = strings.TrimSuffix(class, ":")
	switch class {
	case "helper", "bounded", "not-request", "ws", "route":
		return ""
	case "charged":
		bare := key[strings.LastIndexAny(key, ":.")+1:]
		if !requestConverters[bare] {
			return "charged but not a listed request converter"
		}
		return ""
	case "gated":
		gate, _, _ := strings.Cut(rest, ": ")
		kind, arg, _ := strings.Cut(gate, ":")
		switch kind {
		case "account":
			if _, ok := rawDecisiveAccountReads[arg]; !ok {
				return "gate " + gate + " is not a decisive account read (rawDecisiveAccountReads)"
			}
			return ""
		case "body":
			if ok, _ := VerifiedRawResponsesRequest([]byte(arg), nil); ok {
				return "gate " + gate + " is on the verified-raw list"
			}
			return ""
		}
		return "unknown gate " + gate
	}
	return "unknown class " + class
}

// TestRequestDecodeSitesAreClassified (rr6 item 3): a direct decode of
// JSON in the service or handler packages sits in DecodeFullRequestBody or
// in a function requestDecodeSites classifies; every gate it names holds.
func TestRequestDecodeSitesAreClassified(t *testing.T) {
	sites := scanDirectDecodes(t, nil, ".", "../handler")
	require.Greater(t, len(sites), 300)
	for _, p := range decodeSiteProblems(sites) {
		t.Error(p)
	}
}

// TestRequestDecodeSitesMutations: a new decode in an unlisted function,
// and a gate that no longer holds, both fail by name.
func TestRequestDecodeSitesMutations(t *testing.T) {
	const file = "openai_request_raw_input.go"
	src, err := os.ReadFile(file)
	require.NoError(t, err)
	// Parsed only, never built: json need not be imported.
	mutated := string(src) + "\nfunc rr6DecodeProbe(body []byte) {\n\tvar v any\n\t_ = json.Unmarshal(body, &v)\n}\n"
	problems := decodeSiteProblems(scanDirectDecodes(t, map[string]string{file: mutated}, ".", "../handler"))
	require.Len(t, problems, 1)
	require.Contains(t, problems[0], "service/openai_request_raw_input.go:rr6DecodeProbe: direct decode")

	require.Contains(t, decodeClassProblem("k", "gated account:NoSuchFlag: x"), "not a decisive account read")
	require.Contains(t, decodeClassProblem("k", `gated body:{"input":[]}: x`), "on the verified-raw list")
	require.Empty(t, decodeClassProblem("k", `gated body:{"input":[{"type":"compaction_trigger"}]}: x`))
}
