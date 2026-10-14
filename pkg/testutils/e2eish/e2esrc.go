// Copyright The NRI Plugins Authors. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package e2eish

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"slices"
	"strconv"
	"strings"
)

// e2eStmt is a statement of a test function: its lines, and if it is a
// step of the Script or the verify expression of an assertion.
type e2eStmt struct {
	from, to int
	step     bool
	verify   string
}

// e2eSteps are the Script steps a reverse generated test may call.
var e2eSteps = []string{
	"Launch", "LaunchMayFail", "LaunchFails", "Terminate",
	"SyncWithRuntimePods", "ResumeWithRuntimePods",
	"CreateGuaranteed", "CreateBurstable", "CreateBestEffort", "CreatePod",
	"DeletePods", "DeleteAllPodsIn", "SetNUMA", "SetCmdline",
	"Consts", "VerifyPy", "Skip",
}

const e2eishPath = "github.com/containers/nri-plugins/pkg/testutils/e2eish"

// e2eSource reads the test function of a reverse generated test: its doc
// comment and its statements.
func e2eSource(path, test string, w pyWidths) (string, []e2eStmt, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
	if err != nil {
		return "", nil, err
	}
	harness := "e2eish"
	for _, imp := range f.Imports {
		if p, _ := strconv.Unquote(imp.Path.Value); p == e2eishPath && imp.Name != nil {
			harness = imp.Name.Name
		}
	}
	var fn *ast.FuncDecl
	for _, d := range f.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok && fd.Recv == nil && fd.Name.Name == test {
			fn = fd
		}
	}
	if fn == nil {
		return "", nil, fmt.Errorf("%s: no func %s", path, test)
	}
	at := func(n ast.Node) string { return fset.Position(n.Pos()).String() }

	// The statements before s := NewE2EScript(...) run before any step.
	body, s := fn.Body.List, ""
	for len(body) > 0 && s == "" {
		s = scriptVar(body[0])
		body = body[1:]
	}
	if s == "" {
		return "", nil, fmt.Errorf("%s: %s has no s := NewE2EScript(...)", at(fn), test)
	}

	tr := revTr{s: s, harness: harness, w: w}
	var stmts []e2eStmt
	for _, st := range body {
		lines := e2eStmt{from: fset.Position(st.Pos()).Line, to: fset.Position(st.End()).Line}
		es, ok := st.(*ast.ExprStmt)
		var call *ast.CallExpr
		if ok {
			call, ok = es.X.(*ast.CallExpr)
		}
		if !ok {
			return "", nil, fmt.Errorf("%s: a %s, where only steps and assertions are translated",
				at(st), strings.TrimPrefix(fmt.Sprintf("%T", st), "*ast."))
		}
		if sel, ok := call.Fun.(*ast.SelectorExpr); ok && isIdent(sel.X, s) {
			if !slices.Contains(e2eSteps, sel.Sel.Name) {
				return "", nil, fmt.Errorf("%s: %s.%s has no e2e form", at(st), s, sel.Sel.Name)
			}
			lines.step = true
			stmts = append(stmts, lines)
			continue
		}
		py, err := tr.statement(call)
		if err != nil {
			return "", nil, fmt.Errorf("%s: %w", at(st), err)
		}
		lines.verify = py
		stmts = append(stmts, lines)
	}
	doc := ""
	if fn.Doc != nil {
		doc = fn.Doc.Text()
	}
	return doc, stmts, nil
}

// mergeE2E is the script of the statements, with the steps recorded at
// their lines. Consecutive assertions form one verify. py_consts holds for
// all of them, so Consts must come before the first.
func mergeE2E(stmts []e2eStmt, recs []e2eRec) ([]string, error) {
	at := make([][]e2eRec, len(stmts))
	for _, r := range recs {
		i := slices.IndexFunc(stmts, func(st e2eStmt) bool { return st.from <= r.line && r.line <= st.to })
		if i < 0 || !stmts[i].step {
			return nil, fmt.Errorf("line %d: a step outside the steps of the test", r.line)
		}
		at[i] = append(at[i], r)
	}
	var lines, group []string
	verified := false
	flush := func() error {
		for _, v := range group {
			if strings.Contains(v, `\`) {
				return fmt.Errorf("a backslash in %s, which pyexec would unescape", v)
			}
		}
		for i, v := range group {
			if i == 0 {
				lines = append(lines, "verify \\")
			}
			line := "    " + shellQuoteAlways(v)
			if i < len(group)-1 {
				line += " \\"
			}
			lines = append(lines, line)
		}
		group = nil
		return nil
	}
	for i, st := range stmts {
		if !st.step {
			group = append(group, st.verify)
			verified = true
			continue
		}
		for _, r := range at[i] {
			switch {
			case r.consts && verified:
				return nil, fmt.Errorf("line %d: Consts after a verify, which py_consts holds for too", r.line)
			case r.verify != "":
				group = append(group, r.verify)
				verified = true
			case len(r.cmds) > 0:
				if err := flush(); err != nil {
					return nil, err
				}
				lines = append(lines, r.cmds...)
			}
		}
	}
	if err := flush(); err != nil {
		return nil, err
	}
	return lines, nil
}

func shellQuoteAlways(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// scriptVar is the variable of s := NewE2EScript(...), if the statement is
// that.
func scriptVar(st ast.Stmt) string {
	as, ok := st.(*ast.AssignStmt)
	if !ok || as.Tok != token.DEFINE || len(as.Lhs) != 1 || len(as.Rhs) != 1 {
		return ""
	}
	c, ok := as.Rhs[0].(*ast.CallExpr)
	if !ok {
		return ""
	}
	sel, ok := c.Fun.(*ast.SelectorExpr)
	id, isIdent := as.Lhs[0].(*ast.Ident)
	if !ok || !isIdent || sel.Sel.Name != "NewE2EScript" {
		return ""
	}
	return id.Name
}

// e2eSummary tells what a merged script holds.
func e2eSummary(lines []string) string {
	var cmds, asserts, verifies int
	for _, l := range lines {
		switch {
		case l == "verify \\":
			verifies++
		case strings.HasPrefix(l, "    "):
			asserts++
		default:
			cmds++
		}
	}
	plural := func(n int, one, many string) string {
		if n == 1 {
			return fmt.Sprintf("%d %s", n, one)
		}
		return fmt.Sprintf("%d %s", n, many)
	}
	return plural(cmds, "command", "commands") + ", " + plural(asserts, "assertion", "assertions") +
		" in " + plural(verifies, "verify", "verifies")
}
