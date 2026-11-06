package service

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// Source scan for TestRequestConvertersChargeFirst: every function of the
// service and handler packages (test files excluded), with its calls and
// the block position of each, so the test can ask whether a charge comes
// before a converter on every path.

type chargeScanFunc struct {
	name    string // Recv.Name, or Name
	bare    string // Name: calls are matched by bare name
	pos     string
	gin     bool           // has a *gin.Context parameter
	charger map[string]int // func() error parameters, by name: index
	locals  map[string]bool
	charges []chargeScanSite // ChargeRequestConversion(...), or p() of a charger param or local
	calls   []chargeScanCall
}

// chargeScanSite is where a call sits: the enclosing blocks, innermost
// last, each with the index of the statement on the path (-1: a case
// clause header).
type chargeScanSite struct {
	pos    token.Pos
	chain  []chargeScanBlock
	inCond bool // in the Init or Cond of the innermost statement, an if
	// conditional: the call may not run when its statement does: it is the
	// right operand of && or || (if cond && ChargeRequestConversion(c) != nil).
	// Such a charge dominates nothing. (A charge in a func literal already
	// dominates nothing outside it: its block is not on their chain.)
	conditional bool
}

type chargeScanBlock struct {
	block ast.Node
	idx   int
}

type chargeScanCall struct {
	chargeScanSite
	name string
	args []ast.Expr
}

// dominates: s runs before c on every path that reaches c. s's innermost
// block encloses c, at an earlier statement (or s is in the condition of
// the if statement that holds c). A charge in another branch, or in a block
// c is not in (a dead branch), does not dominate.
func (s chargeScanSite) dominates(c chargeScanSite) bool {
	if s.conditional {
		return false
	}
	in := s.chain[len(s.chain)-1]
	for _, b := range c.chain {
		if b.block == in.block {
			return b.idx > in.idx || (b.idx == in.idx && s.inCond && c.pos > s.pos)
		}
	}
	return false
}

func chargeScanSiteOf(stack []ast.Node, pos token.Pos) chargeScanSite {
	site := chargeScanSite{pos: pos}
	for i := 0; i+1 < len(stack); i++ {
		if n, ok := stack[i].(*ast.BinaryExpr); ok && (n.Op == token.LAND || n.Op == token.LOR) && ast.Node(n.Y) == stack[i+1] {
			site.conditional = true
		}
	}
	for i := 0; i+1 < len(stack); i++ {
		var list []ast.Stmt
		switch b := stack[i].(type) {
		case *ast.BlockStmt:
			list = b.List
		case *ast.CaseClause:
			list = b.Body
		case *ast.CommClause:
			list = b.Body
		default:
			continue
		}
		idx := -1
		for j, st := range list {
			if ast.Node(st) == stack[i+1] {
				idx = j
			}
		}
		site.chain = append(site.chain, chargeScanBlock{block: stack[i], idx: idx})
		site.inCond = false
		if ifs, ok := stack[i+1].(*ast.IfStmt); ok && idx >= 0 {
			inInit := ifs.Init != nil && pos >= ifs.Init.Pos() && pos < ifs.Init.End()
			site.inCond = inInit || (pos >= ifs.Cond.Pos() && pos < ifs.Cond.End())
		}
	}
	return site
}

func isChargerType(e ast.Expr) bool {
	ft, ok := e.(*ast.FuncType)
	if !ok || (ft.Params != nil && len(ft.Params.List) > 0) || ft.Results == nil || len(ft.Results.List) != 1 {
		return false
	}
	id, ok := ft.Results.List[0].Type.(*ast.Ident)
	return ok && id.Name == "error"
}

func calleeName(call *ast.CallExpr) string {
	switch fun := call.Fun.(type) {
	case *ast.Ident:
		return fun.Name
	case *ast.SelectorExpr:
		return fun.Sel.Name
	}
	return ""
}

// scanChargeFuncs parses dirs; overlay replaces the source of a file (by
// its glob path) for the mutation checks.
func scanChargeFuncs(t *testing.T, overlay map[string]string, dirs ...string) []*chargeScanFunc {
	t.Helper()
	fset := token.NewFileSet()
	var out []*chargeScanFunc
	for _, dir := range dirs {
		paths, err := filepath.Glob(filepath.Join(dir, "*.go"))
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
				if fn, ok := decl.(*ast.FuncDecl); ok && fn.Body != nil {
					out = append(out, scanChargeFunc(fset, fn))
				}
			}
		}
	}
	return out
}

func scanChargeFunc(fset *token.FileSet, fn *ast.FuncDecl) *chargeScanFunc {
	f := &chargeScanFunc{name: fn.Name.Name, bare: fn.Name.Name, pos: fset.Position(fn.Pos()).String(),
		charger: map[string]int{}, locals: map[string]bool{}}
	if fn.Recv != nil && len(fn.Recv.List) == 1 {
		recv := fn.Recv.List[0].Type
		if star, ok := recv.(*ast.StarExpr); ok {
			recv = star.X
		}
		if id, ok := recv.(*ast.Ident); ok {
			f.name = id.Name + "." + f.name
		}
	}
	index := 0
	for _, field := range fn.Type.Params.List {
		if star, ok := field.Type.(*ast.StarExpr); ok {
			if sel, ok := star.X.(*ast.SelectorExpr); ok && sel.Sel.Name == "Context" {
				if id, ok := sel.X.(*ast.Ident); ok && id.Name == "gin" {
					f.gin = true
				}
			}
		}
		names := max(1, len(field.Names))
		for i, name := range field.Names {
			// A charger is a func() error parameter named for it (charge,
			// chargeConversion, ...): close and rebuild callbacks share the type.
			if isChargerType(field.Type) && strings.Contains(strings.ToLower(name.Name), "charge") {
				f.charger[name.Name] = index + i
			}
		}
		index += names
	}
	// Locals holding RequestConversionCharger(...).
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		if as, ok := n.(*ast.AssignStmt); ok && len(as.Lhs) == 1 && len(as.Rhs) == 1 {
			if call, ok := as.Rhs[0].(*ast.CallExpr); ok && calleeName(call) == "RequestConversionCharger" {
				if id, ok := as.Lhs[0].(*ast.Ident); ok {
					f.locals[id.Name] = true
				}
			}
		}
		return true
	})
	var stack []ast.Node
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		if n == nil {
			stack = stack[:len(stack)-1]
			return true
		}
		stack = append(stack, n)
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		name := calleeName(call)
		site := chargeScanSiteOf(stack, call.Pos())
		if id, ok := call.Fun.(*ast.Ident); name == "ChargeRequestConversion" ||
			(ok && len(call.Args) == 0 && (f.locals[id.Name] || hasKey(f.charger, id.Name))) {
			f.charges = append(f.charges, site)
		}
		if name != "" {
			f.calls = append(f.calls, chargeScanCall{chargeScanSite: site, name: name, args: call.Args})
		}
		return true
	})
	return f
}

func hasKey(m map[string]int, k string) bool {
	_, ok := m[k]
	return ok
}

// chargedBefore: some charge of f dominates c.
func (f *chargeScanFunc) chargedBefore(c chargeScanSite) bool {
	for _, ch := range f.charges {
		if ch.dominates(c) {
			return true
		}
	}
	return false
}
