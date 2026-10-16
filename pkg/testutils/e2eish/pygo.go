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
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// translatePy translates an e2e verify expression to Go statements, each a
// require assertion on the state a Script computes on a machine of the id
// widths. An error means the expression stays python.
func translatePy(src string, w pyWidths) ([]string, error) {
	n, err := parsePy(src)
	if err != nil {
		return nil, err
	}
	return pyTr{w}.pyStatements(n, src)
}

// pyWidths are the widths topology.py pads the CPU, node and package ids
// of a machine to, 0 if not known.
type pyWidths struct{ cpu, node, pkg int }

// pyTr translates for a machine.
type pyTr struct{ w pyWidths }

// pyVal is a translated value: Go code, and its type, one of set, int, str,
// ids, bool and list. A literal is the expected value of a comparison.
type pyVal struct {
	code, typ string
	lit       bool
}

var accessors = map[string]string{
	"cpus": "CPUs", "mems": "Mems", "nodes": "Nodes", "cores": "Cores",
	"threads": "Threads", "dies": "Dies", "packages": "Packages",
}

// idHelpers are the *_ids helpers translated, with the variables of single
// level names python gives them. The others give tuples.
var idHelpers = map[string][]string{
	"cpu_ids": {"cpus"}, "node_ids": {"nodes", "mems"}, "package_ids": {"packages"},
}

func (tr pyTr) pyStatements(n pyNode, src string) ([]string, error) {
	switch x := n.(type) {
	case pyBool:
		if x.op == "and" {
			var out []string
			for _, v := range x.vals {
				stmts, err := tr.pyStatements(v, src)
				if err != nil {
					return nil, err
				}
				out = append(out, stmts...)
			}
			return out, nil
		}
	case pyCall:
		if stmt, ok, err := tr.pyCallStatement(x); ok || err != nil {
			return []string{stmt}, err
		}
	case pyCmp:
		if stmts, ok, err := tr.pyCmpStatements(x); ok || err != nil {
			return stmts, err
		}
	}
	b, err := tr.pyBoolean(n)
	if err != nil {
		return nil, err
	}
	return []string{"require.True(t, " + b + ", " + strconv.Quote(src) + ")"}, nil
}

// pyCallStatement is the assertion of a call which is one, if it is.
func (tr pyTr) pyCallStatement(c pyCall) (string, bool, error) {
	if name, ok := c.fn.(pyName); ok && name.id == "disjoint_sets" {
		args, err := tr.pyArgs(c.args, "set")
		return "harness.RequireDisjoint(t, " + strings.Join(args, ", ") + ")", true, err
	}
	attr, ok := c.fn.(pyAttr)
	if !ok || (attr.name != "isdisjoint" && attr.name != "issubset") || len(c.args) != 1 {
		return "", false, nil
	}
	x, err := tr.pyValue(attr.x)
	if err != nil || x.typ != "set" {
		return "", false, nil
	}
	args, err := tr.pyArgs(c.args, "set")
	if err != nil {
		return "", true, err
	}
	if attr.name == "isdisjoint" {
		return "harness.RequireDisjoint(t, " + x.code + ", " + args[0] + ")", true, nil
	}
	return "require.Subset(t, " + args[0] + ", " + x.code + ")", true, nil
}

var lenAssert = map[string]string{
	">": "Greater", ">=": "GreaterOrEqual", "<": "Less", "<=": "LessOrEqual",
}

// pyCmpStatements are the assertions of a comparison, if it has its own.
func (tr pyTr) pyCmpStatements(c pyCmp) ([]string, bool, error) {
	vals := []pyVal{}
	for _, n := range append([]pyNode{c.left}, c.rights...) {
		v, err := tr.pyValue(n)
		if err != nil {
			return nil, false, err
		}
		vals = append(vals, v)
	}
	if len(c.ops) > 1 {
		if slices.ContainsFunc(c.ops, func(op string) bool { return op != "==" }) ||
			slices.ContainsFunc(vals, func(v pyVal) bool { return v.typ != "set" }) {
			return nil, false, nil
		}
		var out []string
		for _, v := range vals[1:] {
			out = append(out, "require.Equal(t, "+vals[0].code+", "+v.code+")")
		}
		return out, true, nil
	}

	a, b, op := vals[0], vals[1], c.ops[0]
	switch {
	case (op == "in" || op == "not in") && (a.typ == "str" && b.typ == "set" || a.typ == "set" && b.typ == "list"):
		fn := map[string]string{"in": "Contains", "not in": "NotContains"}[op]
		return []string{"require." + fn + "(t, " + b.code + ", " + a.code + ")"}, true, nil
	case a.typ != b.typ || a.typ == "list" || a.typ == "bool":
		return nil, false, nil
	}

	if l, ok := c.left.(pyCall); ok && a.typ == "int" && b.lit && pyIsName(l.fn, "len") {
		x := strings.TrimSuffix(strings.TrimPrefix(a.code, "len("), ")")
		switch op {
		case "==":
			return []string{"require.Len(t, " + x + ", " + b.code + ")"}, true, nil
		case "!=":
			return []string{"require.NotEqual(t, " + b.code + ", " + a.code + ")"}, true, nil
		}
		return []string{"require." + lenAssert[op] + "(t, " + a.code + ", " + b.code + ")"}, true, nil
	}
	if fn, ok := lenAssert[op]; ok && a.typ == "int" {
		return []string{"require." + fn + "(t, " + a.code + ", " + b.code + ")"}, true, nil
	}
	if op != "==" && op != "!=" {
		return nil, false, nil
	}
	expected, actual := a, b
	if b.lit && !a.lit {
		expected, actual = b, a
	}
	fn := map[string]string{"==": "Equal", "!=": "NotEqual"}[op]
	return []string{"require." + fn + "(t, " + expected.code + ", " + actual.code + ")"}, true, nil
}

func pyIsName(n pyNode, id string) bool {
	name, ok := n.(pyName)
	return ok && name.id == id
}

func (tr pyTr) pyArgs(args []pyNode, typ string) ([]string, error) {
	var out []string
	for _, n := range args {
		v, err := tr.pyValue(n)
		if err != nil {
			return nil, err
		}
		if v.typ != typ {
			return nil, fmt.Errorf("a %s argument where a %s is expected", v.typ, typ)
		}
		out = append(out, v.code)
	}
	return out, nil
}

// pyBoolean is the Go boolean expression of a python one.
func (tr pyTr) pyBoolean(n pyNode) (string, error) {
	switch x := n.(type) {
	case pyBool:
		var parts []string
		for _, v := range x.vals {
			b, err := tr.pyBoolean(v)
			if err != nil {
				return "", err
			}
			if inner, ok := v.(pyBool); ok && inner.op != x.op {
				b = "(" + b + ")"
			}
			parts = append(parts, b)
		}
		return strings.Join(parts, map[string]string{"and": " && ", "or": " || "}[x.op]), nil
	case pyNot:
		b, err := tr.pyBoolean(x.x)
		if err != nil {
			return "", err
		}
		if _, ok := x.x.(pyCall); !ok {
			b = "(" + b + ")"
		}
		return "!" + b, nil
	case pyCmp:
		return tr.pyCmpBoolean(x)
	}
	v, err := tr.pyValue(n)
	if err != nil {
		return "", err
	}
	if v.typ != "bool" {
		return "", fmt.Errorf("a %s as a boolean", v.typ)
	}
	return v.code, nil
}

func (tr pyTr) pyCmpBoolean(c pyCmp) (string, error) {
	left, err := tr.pyValue(c.left)
	if err != nil {
		return "", err
	}
	var parts []string
	for i, op := range c.ops {
		right, err := tr.pyValue(c.rights[i])
		if err != nil {
			return "", err
		}
		var b string
		switch {
		case (op == "in" || op == "not in") && left.typ == "str" && right.typ == "set":
			b = right.code + ".Has(" + left.code + ")"
			if op == "not in" {
				b = "!" + b
			}
		case left.typ != right.typ:
			return "", fmt.Errorf("comparing a %s and a %s", left.typ, right.typ)
		case left.typ == "set" && (op == "==" || op == "!="):
			b = left.code + ".Equal(" + right.code + ")"
			if op == "!=" {
				b = "!" + b
			}
		case left.typ == "int" || left.typ == "str" && (op == "==" || op == "!="):
			b = left.code + " " + op + " " + right.code
		default:
			return "", fmt.Errorf("%s on a %s", op, left.typ)
		}
		parts = append(parts, b)
		left = right
	}
	return strings.Join(parts, " && "), nil
}

// pyValue is the Go of a python value.
func (tr pyTr) pyValue(n pyNode) (pyVal, error) {
	switch x := n.(type) {
	case pyStr:
		return pyVal{strconv.Quote(x.v), "str", true}, nil
	case pyInt:
		return pyVal{strconv.Itoa(x.v), "int", true}, nil
	case pySub:
		name, ok := x.x.(pyName)
		key, isStr := x.key.(pyStr)
		if !ok || !isStr || accessors[name.id] == "" {
			return pyVal{}, fmt.Errorf("an unsupported subscript")
		}
		return pyVal{"s." + accessors[name.id] + "(" + strconv.Quote(key.v) + ")", "set", false}, nil
	case pySet:
		return tr.pySetLiteral(x)
	case pyList:
		elts, err := tr.pyArgs(x.elts, "set")
		return pyVal{"[]harness.Set{" + strings.Join(elts, ", ") + "}", "list", true}, err
	case pyBin:
		args, err := tr.pyArgs([]pyNode{x.a, x.b}, "set")
		if err != nil {
			return pyVal{}, err
		}
		return pyVal{"harness.Difference(" + args[0] + ", " + args[1] + ")", "set", false}, nil
	case pyCall:
		return tr.pyCallValue(x)
	case pyCmp, pyBool, pyNot:
		b, err := tr.pyBoolean(n)
		return pyVal{b, "bool", false}, err
	}
	return pyVal{}, fmt.Errorf("an unsupported %T", n)
}

func (tr pyTr) pyCallValue(c pyCall) (pyVal, error) {
	switch fn := c.fn.(type) {
	case pyName:
		switch {
		case fn.id == "len" && len(c.args) == 1:
			v, err := tr.pyValue(c.args[0])
			if err != nil || v.typ != "set" {
				return pyVal{}, fmt.Errorf("len of an unsupported value")
			}
			return pyVal{"len(" + v.code + ")", "int", false}, nil
		case fn.id == "disjoint_sets":
			args, err := tr.pyArgs(c.args, "set")
			return pyVal{"harness.Disjoint(" + strings.Join(args, ", ") + ")", "bool", false}, err
		case idHelpers[fn.id] != nil && len(c.args) == 1:
			sub, ok := c.args[0].(pySub)
			if name, isName := sub.x.(pyName); !ok || !isName || !slices.Contains(idHelpers[fn.id], name.id) {
				return pyVal{}, fmt.Errorf("%s of other than %v", fn.id, idHelpers[fn.id])
			}
			args, err := tr.pyArgs(c.args, "set")
			if err != nil {
				return pyVal{}, err
			}
			return pyVal{"harness.IDs(" + args[0] + ")", "ids", false}, nil
		}
	case pyAttr:
		var self []string
		if !pyIsName(fn.x, "set") {
			x, err := tr.pyValue(fn.x)
			if err != nil || x.typ != "set" {
				return pyVal{}, fmt.Errorf("a method of an unsupported value")
			}
			self = []string{x.code}
		}
		args, err := tr.pyArgs(c.args, "set")
		if err != nil {
			return pyVal{}, err
		}
		args = append(self, args...)
		switch {
		case fn.name == "union" || fn.name == "intersection":
			name := map[string]string{"union": "Union", "intersection": "Intersection"}[fn.name]
			return pyVal{"harness." + name + "(" + strings.Join(args, ", ") + ")", "set", false}, nil
		case fn.name == "issubset" && len(args) == 2:
			return pyVal{args[0] + ".IsSubset(" + args[1] + ")", "bool", false}, nil
		case fn.name == "isdisjoint" && len(args) == 2:
			return pyVal{"harness.Disjoint(" + args[0] + ", " + args[1] + ")", "bool", false}, nil
		}
	}
	return pyVal{}, fmt.Errorf("an unsupported call")
}

var idName = regexp.MustCompile(`^(cpu|node|package)([0-9]+)$`)

// pySetLiteral is a set of strings, with the id literals of the Script for
// names of one level padded as the machine pads them, or a set of ints, as
// the ids of *_ids.
func (tr pyTr) pySetLiteral(s pySet) (pyVal, error) {
	var (
		names []string
		ints  []int
	)
	for _, e := range s.elts {
		switch x := e.(type) {
		case pyStr:
			names = append(names, x.v)
		case pyInt:
			ints = append(ints, x.v)
		default:
			return pyVal{}, fmt.Errorf("a set of an unsupported %T", e)
		}
	}
	if len(ints) > 0 {
		if len(names) > 0 {
			return pyVal{}, fmt.Errorf("a set of ints and strings")
		}
		slices.Sort(ints)
		return pyVal{"[]int{" + joinInts(ints) + "}", "ids", true}, nil
	}

	widths := map[string]int{"cpu": tr.w.cpu, "node": tr.w.node, "package": tr.w.pkg}
	level := ""
	for _, n := range names {
		m := idName.FindStringSubmatch(n)
		if m == nil || level != "" && m[1] != level || len(m[2]) != widths[m[1]] {
			level = "-"
			break
		}
		level = m[1]
		id, _ := strconv.Atoi(m[2])
		ints = append(ints, id)
	}
	if fn, ok := map[string]string{"cpu": "CPUIDs", "node": "NodeIDs", "package": "PackageIDs"}[level]; ok {
		slices.Sort(ints)
		return pyVal{"s." + fn + "(" + joinInts(ints) + ")", "set", true}, nil
	}
	quoted := make([]string, 0, len(names))
	for _, n := range names {
		quoted = append(quoted, strconv.Quote(n))
	}
	return pyVal{"harness.NewSet(" + strings.Join(quoted, ", ") + ")", "set", true}, nil
}

func joinInts(ints []int) string {
	strs := make([]string, 0, len(ints))
	for _, i := range ints {
		strs = append(strs, strconv.Itoa(i))
	}
	return strings.Join(strs, ", ")
}
