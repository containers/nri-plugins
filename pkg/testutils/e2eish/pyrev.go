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
	"go/token"
	"go/types"
	"strconv"
	"strings"
)

// revTr translates the require assertions of a Script back to e2e verify
// expressions: the inverse of translatePy. s and harness are the names of
// the Script and of this package in the test.
type revTr struct {
	s, harness string
	w          pyWidths
}

// revVal is a translated value: python, its type as pyVal has it, and if
// it is a literal.
type revVal struct {
	py, typ string
	lit     bool
}

// isAssertion tells if a call is one revTr translates, or should.
func isAssertion(c *ast.CallExpr, harness string) bool {
	sel, ok := c.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	x, ok := sel.X.(*ast.Ident)
	return ok && (x.Name == "require" || x.Name == harness && sel.Sel.Name == "RequireDisjoint")
}

var revCmp = map[string]string{
	"Greater": ">", "GreaterOrEqual": ">=", "Less": "<", "LessOrEqual": "<=",
}

// statement is the verify expression of an assertion.
func (tr revTr) statement(c *ast.CallExpr) (string, error) {
	if !isAssertion(c, tr.harness) {
		return "", fmt.Errorf("%s is no assertion", types.ExprString(c.Fun))
	}
	fn := c.Fun.(*ast.SelectorExpr).Sel.Name
	if len(c.Args) == 0 || !isIdent(c.Args[0], "t") {
		return "", fmt.Errorf("an assertion not on t")
	}
	args := c.Args[1:]
	want := func(n int) error {
		if len(args) < n {
			return fmt.Errorf("require.%s with %d arguments", fn, len(args))
		}
		return nil
	}
	if fn == "RequireDisjoint" {
		vals, err := tr.values(args, "set")
		return "disjoint_sets(" + strings.Join(vals, ", ") + ")", err
	}
	switch fn {
	case "True", "False":
		if err := want(1); err != nil {
			return "", err
		}
		b, err := tr.boolean(args[0])
		if fn == "False" {
			b = "not (" + b + ")"
		}
		return b, err
	case "Len":
		if err := want(2); err != nil {
			return "", err
		}
		x, err := tr.typed(args[0], "set")
		if err != nil {
			return "", err
		}
		n, err := tr.typed(args[1], "int")
		return "len(" + x.py + ") == " + n.py, err
	case "Greater", "GreaterOrEqual", "Less", "LessOrEqual":
		if err := want(2); err != nil {
			return "", err
		}
		a, err := tr.typed(args[0], "int")
		if err != nil {
			return "", err
		}
		b, err := tr.typed(args[1], "int")
		return a.py + " " + revCmp[fn] + " " + b.py, err
	case "Equal", "NotEqual":
		if err := want(2); err != nil {
			return "", err
		}
		e, err := tr.value(args[0])
		if err != nil {
			return "", err
		}
		a, err := tr.value(args[1])
		if err != nil {
			return "", err
		}
		if e.typ != a.typ || e.typ == "bool" || e.typ == "list" {
			return "", fmt.Errorf("require.%s of a %s and a %s", fn, e.typ, a.typ)
		}
		op := map[string]string{"Equal": "==", "NotEqual": "!="}[fn]
		if e.lit && !a.lit {
			return a.py + " " + op + " " + e.py, nil
		}
		return e.py + " " + op + " " + a.py, nil
	case "Contains", "NotContains":
		if err := want(2); err != nil {
			return "", err
		}
		b, err := tr.value(args[0])
		if err != nil {
			return "", err
		}
		a, err := tr.value(args[1])
		if err != nil {
			return "", err
		}
		strInSet, setInList := a.typ == "str" && b.typ == "set", a.typ == "set" && b.typ == "list"
		if !strInSet && !setInList {
			return "", fmt.Errorf("require.%s of a %s in a %s", fn, a.typ, b.typ)
		}
		op := map[string]string{"Contains": " in ", "NotContains": " not in "}[fn]
		return a.py + op + b.py, nil
	case "Subset":
		if err := want(2); err != nil {
			return "", err
		}
		b, err := tr.typed(args[0], "set")
		if err != nil {
			return "", err
		}
		x, err := tr.typed(args[1], "set")
		return x.py + ".issubset(" + b.py + ")", err
	}
	return "", fmt.Errorf("require.%s has no python form", fn)
}

func (tr revTr) typed(e ast.Expr, typ string) (revVal, error) {
	v, err := tr.value(e)
	if err == nil && v.typ != typ {
		err = fmt.Errorf("a %s where a %s is expected: %s", v.typ, typ, types.ExprString(e))
	}
	return v, err
}

func (tr revTr) values(args []ast.Expr, typ string) ([]string, error) {
	var out []string
	for _, a := range args {
		v, err := tr.typed(a, typ)
		if err != nil {
			return nil, err
		}
		out = append(out, v.py)
	}
	return out, nil
}

func (tr revTr) boolean(e ast.Expr) (string, error) {
	v, err := tr.typed(e, "bool")
	return v.py, err
}

func (tr revTr) value(e ast.Expr) (revVal, error) {
	switch x := e.(type) {
	case *ast.ParenExpr:
		v, err := tr.value(x.X)
		v.py = "(" + v.py + ")"
		return v, err
	case *ast.BasicLit:
		switch x.Kind {
		case token.INT:
			return revVal{x.Value, "int", true}, nil
		case token.STRING:
			s, err := strconv.Unquote(x.Value)
			return revVal{strconv.Quote(s), "str", true}, err
		}
	case *ast.CompositeLit:
		return tr.composite(x)
	case *ast.UnaryExpr:
		if x.Op == token.NOT {
			b, err := tr.boolean(x.X)
			return revVal{"not (" + b + ")", "bool", false}, err
		}
	case *ast.BinaryExpr:
		return tr.binary(x)
	case *ast.CallExpr:
		return tr.call(x)
	}
	return revVal{}, fmt.Errorf("no python form for %s", types.ExprString(e))
}

func (tr revTr) composite(c *ast.CompositeLit) (revVal, error) {
	switch typ := types.ExprString(c.Type); typ {
	case "[]int":
		ints, err := tr.values(c.Elts, "int")
		if err == nil && len(ints) == 0 {
			err = fmt.Errorf("an empty []int")
		}
		return revVal{"{" + strings.Join(ints, ", ") + "}", "ids", true}, err
	case "[]" + tr.harness + ".Set":
		sets, err := tr.values(c.Elts, "set")
		return revVal{"[" + strings.Join(sets, ", ") + "]", "list", true}, err
	default:
		return revVal{}, fmt.Errorf("no python form for a %s", typ)
	}
}

func (tr revTr) binary(b *ast.BinaryExpr) (revVal, error) {
	switch b.Op {
	case token.LAND, token.LOR:
		l, err := tr.boolean(b.X)
		if err != nil {
			return revVal{}, err
		}
		r, err := tr.boolean(b.Y)
		op := map[token.Token]string{token.LAND: " and ", token.LOR: " or "}[b.Op]
		return revVal{l + op + r, "bool", false}, err
	case token.EQL, token.NEQ, token.LSS, token.LEQ, token.GTR, token.GEQ:
		l, err := tr.value(b.X)
		if err != nil {
			return revVal{}, err
		}
		r, err := tr.value(b.Y)
		if err != nil {
			return revVal{}, err
		}
		if l.typ != r.typ || l.typ != "int" && l.typ != "str" {
			return revVal{}, fmt.Errorf("%s on a %s and a %s", b.Op, l.typ, r.typ)
		}
		return revVal{l.py + " " + b.Op.String() + " " + r.py, "bool", false}, nil
	}
	return revVal{}, fmt.Errorf("no python form for %s", b.Op)
}

func (tr revTr) call(c *ast.CallExpr) (revVal, error) {
	if isIdent(c.Fun, "len") && len(c.Args) == 1 {
		v, err := tr.typed(c.Args[0], "set")
		return revVal{"len(" + v.py + ")", "int", false}, err
	}
	sel, ok := c.Fun.(*ast.SelectorExpr)
	if !ok {
		return revVal{}, fmt.Errorf("no python form for %s", types.ExprString(c))
	}
	if x, ok := sel.X.(*ast.Ident); ok {
		switch x.Name {
		case tr.s:
			return tr.scriptCall(sel.Sel.Name, c.Args)
		case tr.harness:
			return tr.harnessCall(sel.Sel.Name, c.Args)
		}
	}
	recv, err := tr.typed(sel.X, "set")
	if err != nil || len(c.Args) != 1 {
		return revVal{}, fmt.Errorf("no python form for %s", types.ExprString(c))
	}
	switch sel.Sel.Name {
	case "Has":
		a, err := tr.typed(c.Args[0], "str")
		return revVal{a.py + " in " + recv.py, "bool", false}, err
	case "Equal":
		a, err := tr.typed(c.Args[0], "set")
		return revVal{recv.py + " == " + a.py, "bool", false}, err
	case "IsSubset":
		a, err := tr.typed(c.Args[0], "set")
		return revVal{recv.py + ".issubset(" + a.py + ")", "bool", false}, err
	}
	return revVal{}, fmt.Errorf("no python form for %s", types.ExprString(c))
}

// scriptCall is an accessor of the state, or a set of ids, of the Script.
func (tr revTr) scriptCall(name string, args []ast.Expr) (revVal, error) {
	for v, acc := range accessors {
		if acc != name {
			continue
		}
		if len(args) != 1 {
			return revVal{}, fmt.Errorf("s.%s with %d arguments", name, len(args))
		}
		key, err := tr.typed(args[0], "str")
		if err != nil || !key.lit {
			return revVal{}, fmt.Errorf("s.%s of other than a string literal", name)
		}
		return revVal{v + "[" + key.py + "]", "set", false}, nil
	}
	level, width := map[string]string{"CPUIDs": "cpu", "NodeIDs": "node", "PackageIDs": "package"}[name], 0
	switch level {
	case "cpu":
		width = tr.w.cpu
	case "node":
		width = tr.w.node
	case "package":
		width = tr.w.pkg
	default:
		return revVal{}, fmt.Errorf("s.%s has no python form", name)
	}
	if len(args) == 0 {
		return revVal{}, fmt.Errorf("s.%s of no ids", name)
	}
	var names []string
	for _, a := range args {
		lit, ok := a.(*ast.BasicLit)
		if !ok || lit.Kind != token.INT {
			return revVal{}, fmt.Errorf("s.%s of other than int literals", name)
		}
		id, _ := strconv.Atoi(lit.Value)
		names = append(names, strconv.Quote(pad(level, id, width)))
	}
	return revVal{"{" + strings.Join(names, ", ") + "}", "set", true}, nil
}

func (tr revTr) harnessCall(name string, args []ast.Expr) (revVal, error) {
	switch name {
	case "NewSet":
		strs, err := tr.values(args, "str")
		if err == nil && len(strs) == 0 {
			err = fmt.Errorf("an empty NewSet")
		}
		return revVal{"{" + strings.Join(strs, ", ") + "}", "set", true}, err
	case "IDs":
		if len(args) == 1 {
			if c, ok := args[0].(*ast.CallExpr); ok {
				if sel, ok := c.Fun.(*ast.SelectorExpr); ok && isIdent(sel.X, tr.s) {
					for helper, vars := range idHelpers {
						for _, v := range vars {
							if accessors[v] == sel.Sel.Name {
								x, err := tr.typed(args[0], "set")
								return revVal{helper + "(" + x.py + ")", "ids", false}, err
							}
						}
					}
				}
			}
		}
		return revVal{}, fmt.Errorf("IDs of other than CPUs, Nodes, Mems or Packages")
	case "Union", "Intersection":
		sets, err := tr.values(args, "set")
		return revVal{"set." + strings.ToLower(name) + "(" + strings.Join(sets, ", ") + ")", "set", false}, err
	case "Difference":
		sets, err := tr.values(args, "set")
		if err == nil && len(sets) != 2 {
			err = fmt.Errorf("Difference of %d sets", len(sets))
		}
		if err != nil {
			return revVal{}, err
		}
		return revVal{"(" + sets[0] + " - " + sets[1] + ")", "set", false}, nil
	case "Disjoint":
		sets, err := tr.values(args, "set")
		return revVal{"disjoint_sets(" + strings.Join(sets, ", ") + ")", "bool", false}, err
	}
	return revVal{}, fmt.Errorf("%s.%s has no python form", tr.harness, name)
}

func isIdent(e ast.Expr, name string) bool {
	id, ok := e.(*ast.Ident)
	return ok && id.Name == name
}
