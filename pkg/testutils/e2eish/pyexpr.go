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
	"strconv"
	"strings"
	"unicode"
)

// The python of e2e verify expressions the generator translates to Go: an
// expression of names, subscripts, calls, attributes, int and string
// literals, set and list literals, set difference, comparisons and boolean
// operators. Anything else is a parse error, and the expression stays python.
type (
	pyNode any
	pyName struct{ id string }
	pyStr  struct{ v string }
	pyInt  struct{ v int }
	pySet  struct{ elts []pyNode }
	pyList struct{ elts []pyNode }
	pySub  struct{ x, key pyNode }
	pyCall struct {
		fn   pyNode
		args []pyNode
	}
	pyAttr struct {
		x    pyNode
		name string
	}
	pyBin struct{ a, b pyNode } // a - b
	pyCmp struct {
		left   pyNode
		ops    []string // == != < <= > >= in, "not in"
		rights []pyNode
	}
	pyBool struct {
		op   string // and, or
		vals []pyNode
	}
	pyNot struct{ x pyNode }
)

type pyToken struct {
	kind string // name, int, str, op, end
	text string
}

func tokenizePy(src string) ([]pyToken, error) {
	var toks []pyToken
	for i := 0; i < len(src); {
		c := rune(src[i])
		switch {
		case unicode.IsSpace(c):
			i++
		case c == '_' || unicode.IsLetter(c):
			j := i
			for j < len(src) && (src[j] == '_' || unicode.IsLetter(rune(src[j])) || unicode.IsDigit(rune(src[j]))) {
				j++
			}
			toks = append(toks, pyToken{"name", src[i:j]})
			i = j
		case unicode.IsDigit(c):
			j := i
			for j < len(src) && unicode.IsDigit(rune(src[j])) {
				j++
			}
			toks = append(toks, pyToken{"int", src[i:j]})
			i = j
		case c == '"' || c == '\'':
			var b strings.Builder
			j := i + 1
			for ; j < len(src) && src[j] != src[i]; j++ {
				if src[j] == '\\' {
					return nil, fmt.Errorf("an escape in a string")
				}
				b.WriteByte(src[j])
			}
			if j >= len(src) {
				return nil, fmt.Errorf("unterminated string")
			}
			toks = append(toks, pyToken{"str", b.String()})
			i = j + 1
		default:
			op := src[i : i+1]
			if i+1 < len(src) && strings.Contains("== != <= >= **", src[i:i+2]) {
				op = src[i : i+2]
			}
			if !strings.Contains("()[]{},.-<>", op) && !strings.Contains("== != <= >=", op) {
				return nil, fmt.Errorf("unsupported %q", op)
			}
			toks = append(toks, pyToken{"op", op})
			i += len(op)
		}
	}
	return append(toks, pyToken{kind: "end"}), nil
}

type pyParser struct {
	toks []pyToken
	pos  int
}

// parsePy parses an expression of the python the generator translates.
func parsePy(src string) (pyNode, error) {
	toks, err := tokenizePy(src)
	if err != nil {
		return nil, err
	}
	for _, t := range toks {
		if t.kind == "name" && (t.text == "for" || t.text == "if" || t.text == "lambda") {
			return nil, fmt.Errorf("unsupported %q", t.text)
		}
	}
	p := &pyParser{toks: toks}
	n, err := p.or()
	if err != nil {
		return nil, err
	}
	if t := p.peek(); t.kind != "end" {
		return nil, fmt.Errorf("unexpected %q", t.text)
	}
	return n, nil
}

func (p *pyParser) peek() pyToken { return p.toks[p.pos] }

func (p *pyParser) next() pyToken {
	t := p.toks[p.pos]
	if t.kind != "end" {
		p.pos++
	}
	return t
}

// is tells if the next token is the operator or keyword, and takes it if so.
func (p *pyParser) is(text string) bool {
	if t := p.peek(); (t.kind == "op" || t.kind == "name") && t.text == text {
		p.pos++
		return true
	}
	return false
}

func (p *pyParser) expect(text string) error {
	if !p.is(text) {
		return fmt.Errorf("expected %q, got %q", text, p.peek().text)
	}
	return nil
}

func (p *pyParser) or() (pyNode, error)  { return p.boolOp("or", p.and) }
func (p *pyParser) and() (pyNode, error) { return p.boolOp("and", p.not) }

func (p *pyParser) boolOp(op string, operand func() (pyNode, error)) (pyNode, error) {
	n, err := operand()
	if err != nil {
		return nil, err
	}
	vals := []pyNode{n}
	for p.is(op) {
		if n, err = operand(); err != nil {
			return nil, err
		}
		vals = append(vals, n)
	}
	if len(vals) == 1 {
		return vals[0], nil
	}
	return pyBool{op, vals}, nil
}

func (p *pyParser) not() (pyNode, error) {
	if p.is("not") {
		x, err := p.not()
		return pyNot{x}, err
	}
	return p.cmp()
}

func (p *pyParser) cmp() (pyNode, error) {
	left, err := p.sum()
	if err != nil {
		return nil, err
	}
	c := pyCmp{left: left}
	for {
		var op string
		switch t := p.peek(); {
		case t.kind == "op" && strings.Contains("== != < <= > >=", t.text):
			op = p.next().text
		case t.kind == "name" && t.text == "in":
			op = p.next().text
		case t.kind == "name" && t.text == "not" && p.toks[p.pos+1].text == "in":
			p.pos += 2
			op = "not in"
		}
		if op == "" {
			break
		}
		right, err := p.sum()
		if err != nil {
			return nil, err
		}
		c.ops, c.rights = append(c.ops, op), append(c.rights, right)
	}
	if len(c.ops) == 0 {
		return left, nil
	}
	return c, nil
}

func (p *pyParser) sum() (pyNode, error) {
	n, err := p.postfix()
	for err == nil && p.is("-") {
		var b pyNode
		if b, err = p.postfix(); err == nil {
			n = pyBin{n, b}
		}
	}
	return n, err
}

func (p *pyParser) postfix() (pyNode, error) {
	n, err := p.atom()
	for err == nil {
		switch {
		case p.is("("):
			var args []pyNode
			if args, err = p.elts(")"); err == nil {
				n = pyCall{n, args}
			}
		case p.is("["):
			var key pyNode
			if key, err = p.or(); err == nil {
				err = p.expect("]")
				n = pySub{n, key}
			}
		case p.is("."):
			t := p.next()
			if t.kind != "name" {
				return nil, fmt.Errorf("expected a name after '.'")
			}
			n = pyAttr{n, t.text}
		default:
			return n, nil
		}
	}
	return nil, err
}

// elts parses the elements of a literal or the arguments of a call, up to
// the closing token. A trailing comma is fine.
func (p *pyParser) elts(end string) ([]pyNode, error) {
	var elts []pyNode
	for !p.is(end) {
		n, err := p.or()
		if err != nil {
			return nil, err
		}
		elts = append(elts, n)
		if !p.is(",") {
			if err := p.expect(end); err != nil {
				return nil, err
			}
			break
		}
	}
	return elts, nil
}

func (p *pyParser) atom() (pyNode, error) {
	t := p.next()
	switch {
	case t.kind == "name" && !strings.Contains(" and or not in ", " "+t.text+" "):
		return pyName{t.text}, nil
	case t.kind == "int":
		v, err := strconv.Atoi(t.text)
		return pyInt{v}, err
	case t.kind == "str":
		return pyStr{t.text}, nil
	case t.text == "(":
		n, err := p.or()
		if err == nil {
			err = p.expect(")")
		}
		return n, err
	case t.text == "{":
		elts, err := p.elts("}")
		if err == nil && len(elts) == 0 {
			err = fmt.Errorf("an empty dict")
		}
		return pySet{elts}, err
	case t.text == "[":
		elts, err := p.elts("]")
		return pyList{elts}, err
	}
	return nil, fmt.Errorf("unexpected %q", t.text)
}
