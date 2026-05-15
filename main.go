// find-ternary-candidates reports Go if-statements that could plausibly
// benefit from a ternary operator.
package main

import (
	"bytes"
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"os"
	"path/filepath"
)

var verbose bool

type Counter struct {
	IfElseLiberal      int // any if { 1 stmt } else { 1 stmt } — high FP rate
	IfElseConservative int // subset: same-LHS assign, matching return, or same-callee one-differing-arg
	InitThenIfAssign   int
}

func main() {
	flag.BoolVar(&verbose, "verbose", false, "print original source for each candidate")
	flag.Parse()
	if flag.NArg() == 0 {
		fmt.Fprintf(os.Stderr, "usage: %s <go files or dirs>\n", os.Args[0])
		os.Exit(2)
	}

	files := collectGoFiles(flag.Args())
	fset := token.NewFileSet()

	var counts Counter

	for _, file := range files {
		f, err := parser.ParseFile(fset, file, nil, 0)
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s: %v\n", file, err)
			continue
		}

		ast.Inspect(f, func(n ast.Node) bool {
			block, ok := n.(*ast.BlockStmt)
			if !ok {
				return true
			}

			lib, cons := findSimpleIfElse(fset, block)
			counts.IfElseLiberal += lib
			counts.IfElseConservative += cons
			counts.InitThenIfAssign += findInitThenIfAssign(fset, block)

			return true
		})
	}

	fmt.Fprintf(os.Stderr, "\nsummary:\n")
	fmt.Fprintf(os.Stderr, "  if-else (liberal):      %d\n", counts.IfElseLiberal)
	fmt.Fprintf(os.Stderr, "  if-else (conservative): %d\n", counts.IfElseConservative)
	fmt.Fprintf(os.Stderr, "  init-then-if-assign:    %d\n", counts.InitThenIfAssign)
	fmt.Fprintf(os.Stderr, "  total (conservative):   %d\n",
		counts.IfElseConservative+counts.InitThenIfAssign)
}

func findSimpleIfElse(fset *token.FileSet, block *ast.BlockStmt) (liberal, conservative int) {
	for _, stmt := range block.List {
		ifs, ok := stmt.(*ast.IfStmt)
		if !ok || !isLiberalIfElse(ifs) {
			continue
		}

		pos := fset.Position(ifs.Pos())
		tier := "if-else-liberal"
		if isConservativeIfElse(ifs) {
			conservative++
			tier = "if-else-conservative"
		}
		fmt.Printf("%s:%d:%d %s\n", pos.Filename, pos.Line, pos.Column, tier)
		if verbose {
			printNode(fset, ifs)
		}
		liberal++
	}
	return
}

// isLiberalIfElse matches if { single simple stmt } else { single simple stmt }
// with no init clause. Both arms must contain a simple statement (assignment,
// return, expression, or inc/dec) — not a nested if or block — to avoid the
// high false-positive rate of matching compound nested structures.
func isLiberalIfElse(ifs *ast.IfStmt) bool {
	if ifs.Init != nil || ifs.Body == nil || len(ifs.Body.List) != 1 {
		return false
	}
	elseBlock, ok := ifs.Else.(*ast.BlockStmt)
	if !ok || len(elseBlock.List) != 1 {
		return false
	}
	return isSimpleStmt(ifs.Body.List[0]) && isSimpleStmt(elseBlock.List[0])
}

// isSimpleStmt reports whether s is a statement type that could plausibly be
// expressed as the branch of a ternary: assignment, return, expression
// statement, or inc/dec. Nested ifs, blocks, and other compound forms are
// excluded.
func isSimpleStmt(s ast.Stmt) bool {
	switch s.(type) {
	case *ast.AssignStmt, *ast.ReturnStmt, *ast.ExprStmt, *ast.IncDecStmt:
		return true
	default:
		return false
	}
}

// isConservativeIfElse matches patterns where a ternary clearly applies:
//   - both arms assign to the same LHS
//   - both arms return a single value
//   - both arms call the same function/method with identical args except exactly one
func isConservativeIfElse(ifs *ast.IfStmt) bool {
	elseBlock := ifs.Else.(*ast.BlockStmt) // safe: isLiberalIfElse already verified
	thenStmt := ifs.Body.List[0]
	elseStmt := elseBlock.List[0]

	// Both arms return a single value.
	if thenRet, ok := thenStmt.(*ast.ReturnStmt); ok {
		elseRet, ok := elseStmt.(*ast.ReturnStmt)
		return ok && len(thenRet.Results) == 1 && len(elseRet.Results) == 1
	}

	// Both arms assign to the same LHS.
	if thenAssign, ok := thenStmt.(*ast.AssignStmt); ok {
		elseAssign, ok := elseStmt.(*ast.AssignStmt)
		return ok &&
			isSinglePlainAssignment(thenAssign) && thenAssign.Tok == token.ASSIGN &&
			isSinglePlainAssignment(elseAssign) && elseAssign.Tok == token.ASSIGN &&
			sameSimpleLHS(thenAssign.Lhs[0], elseAssign.Lhs[0])
	}

	// Both arms call the same function/method with exactly one differing argument.
	if thenExpr, ok := thenStmt.(*ast.ExprStmt); ok {
		elseExpr, ok := elseStmt.(*ast.ExprStmt)
		if !ok {
			return false
		}
		return isSameCalleeOneArgDiffers(thenExpr, elseExpr)
	}

	return false
}

// isSameCalleeOneArgDiffers reports whether two expression statements are calls
// to the same function/method with the same number of arguments and exactly one
// argument position that differs.
func isSameCalleeOneArgDiffers(a, b *ast.ExprStmt) bool {
	ca, ok := a.X.(*ast.CallExpr)
	if !ok {
		return false
	}
	cb, ok := b.X.(*ast.CallExpr)
	if !ok {
		return false
	}
	if !sameExpr(ca.Fun, cb.Fun) || len(ca.Args) != len(cb.Args) {
		return false
	}
	diff := 0
	for i := range ca.Args {
		if !sameExpr(ca.Args[i], cb.Args[i]) {
			diff++
		}
	}
	return diff == 1
}

func findInitThenIfAssign(fset *token.FileSet, block *ast.BlockStmt) int {
	count := 0

	for i := 0; i+1 < len(block.List); i++ {
		initAssign, ok := block.List[i].(*ast.AssignStmt)
		if !ok || !isSinglePlainAssignment(initAssign) {
			continue
		}

		ifs, ok := block.List[i+1].(*ast.IfStmt)
		if !ok || !isSingleArmIfAssign(ifs) {
			continue
		}

		bodyAssign := ifs.Body.List[0].(*ast.AssignStmt)

		if !sameSimpleLHS(initAssign.Lhs[0], bodyAssign.Lhs[0]) {
			continue
		}

		lhs := initAssign.Lhs[0]

		// Skip if the condition references the LHS; inlining the init RHS
		// into the condition would change evaluation order or repeat side effects.
		if exprContainsLHS(ifs.Cond, lhs) {
			continue
		}

		// Skip if the body RHS references the LHS; the assignment is a
		// self-referential update, not a simple conditional initialization.
		if exprContainsLHS(bodyAssign.Rhs[0], lhs) {
			continue
		}

		pos := fset.Position(initAssign.Pos())
		ifpos := fset.Position(ifs.Pos())

		fmt.Printf("%s:%d:%d init-then-if-assign lhs=%s if=%d:%d\n",
			pos.Filename,
			pos.Line,
			pos.Column,
			lhsName(initAssign.Lhs[0]),
			ifpos.Line,
			ifpos.Column,
		)
		if verbose {
			printNode(fset, initAssign)
			printNode(fset, ifs)
		}

		count++
	}

	return count
}

func printNode(fset *token.FileSet, node ast.Node) {
	var buf bytes.Buffer
	printer.Fprint(&buf, fset, node)
	fmt.Println(buf.String())
}

func isSingleArmIfAssign(ifs *ast.IfStmt) bool {
	if ifs.Init != nil || ifs.Else != nil {
		return false
	}
	if ifs.Body == nil || len(ifs.Body.List) != 1 {
		return false
	}

	assign, ok := ifs.Body.List[0].(*ast.AssignStmt)
	if !ok {
		return false
	}

	return isSinglePlainAssignment(assign) && assign.Tok == token.ASSIGN
}

func isSinglePlainAssignment(assign *ast.AssignStmt) bool {
	if len(assign.Lhs) != 1 || len(assign.Rhs) != 1 {
		return false
	}

	switch assign.Tok {
	case token.ASSIGN, token.DEFINE:
		return true
	default:
		return false
	}
}

// exprContainsLHS reports whether lhs (an *ast.Ident or *ast.SelectorExpr)
// appears anywhere inside expr.
func exprContainsLHS(expr ast.Expr, lhs ast.Expr) bool {
	found := false
	ast.Inspect(expr, func(n ast.Node) bool {
		if found {
			return false
		}
		e, ok := n.(ast.Expr)
		if !ok {
			return true
		}
		if sameSimpleLHS(e, lhs) {
			found = true
			return false
		}
		return true
	})
	return found
}

// sameExpr reports structural equality for simple expression forms.
func sameExpr(a, b ast.Expr) bool {
	switch x := a.(type) {
	case *ast.Ident:
		y, ok := b.(*ast.Ident)
		return ok && x.Name == y.Name
	case *ast.BasicLit:
		y, ok := b.(*ast.BasicLit)
		return ok && x.Kind == y.Kind && x.Value == y.Value
	case *ast.SelectorExpr:
		y, ok := b.(*ast.SelectorExpr)
		return ok && sameExpr(x.X, y.X) && x.Sel.Name == y.Sel.Name
	case *ast.CallExpr:
		y, ok := b.(*ast.CallExpr)
		if !ok || !sameExpr(x.Fun, y.Fun) || len(x.Args) != len(y.Args) {
			return false
		}
		for i := range x.Args {
			if !sameExpr(x.Args[i], y.Args[i]) {
				return false
			}
		}
		return true
	default:
		return false
	}
}

func sameSimpleLHS(a, b ast.Expr) bool {
	switch x := a.(type) {
	case *ast.Ident:
		y, ok := b.(*ast.Ident)
		return ok && x.Name == y.Name

	case *ast.SelectorExpr:
		y, ok := b.(*ast.SelectorExpr)
		return ok &&
			sameSimpleLHS(x.X, y.X) &&
			x.Sel.Name == y.Sel.Name

	default:
		return false
	}
}

func lhsName(e ast.Expr) string {
	switch x := e.(type) {
	case *ast.Ident:
		return x.Name
	case *ast.SelectorExpr:
		return lhsName(x.X) + "." + x.Sel.Name
	default:
		return "<complex>"
	}
}

func collectGoFiles(args []string) []string {
	var files []string

	for _, arg := range args {
		info, err := os.Stat(arg)
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s: %v\n", arg, err)
			continue
		}

		if !info.IsDir() {
			if filepath.Ext(arg) == ".go" {
				files = append(files, arg)
			}
			continue
		}

		_ = filepath.WalkDir(arg, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if d.IsDir() {
				switch d.Name() {
				case ".git", "vendor", "testdata":
					return filepath.SkipDir
				}
				return nil
			}
			if filepath.Ext(path) == ".go" {
				files = append(files, path)
			}
			return nil
		})
	}

	return files
}
