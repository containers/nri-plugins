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

package fsm

import (
	"fmt"
	"strings"
)

// TableText is a transition table in words, for the documentation.
type TableText struct {
	Object, Field string
	Observed      bool
	States        []string
	Rows          []RowText
	Refused       []string
}

// RowText is a row of a transition table in words.
type RowText struct {
	From, To []string
	Stays    bool
	Triggers []string
	When     string
	Guard    string
	Effects  string
}

// Text returns the table in words.
func (t *Table[S]) Text() TableText {
	tt := TableText{Object: t.Object, Field: t.Field, Observed: t.Observed, States: Texts(t.States)}
	for _, r := range t.Rows {
		rt := RowText{
			From:     Texts(r.From),
			Stays:    r.Stays,
			Triggers: Texts(r.Triggers),
			When:     r.When,
			Guard:    r.Guard,
			Effects:  r.Effects,
		}
		if !r.Stays {
			rt.To = []string{fmt.Sprint(r.To)}
		}
		tt.Rows = append(tt.Rows, rt)
	}
	for _, r := range t.Refused {
		to := "any state"
		if len(r.To) > 0 {
			to = strings.Join(Texts(r.To), ", ")
		}
		if len(r.Except) > 0 {
			to += " except on " + strings.Join(Texts(r.Except), ", ")
		}
		tt.Refused = append(tt.Refused, fmt.Sprintf("%s -> %s: %s", strings.Join(Texts(r.From), ", "), to, r.Why))
	}
	return tt
}

// RowName returns the name of row i of the table, "<object>.<field>
// <from> -> <to> on <triggers>", with "(unchanged)" for the to of a row
// that keeps the state.
func (t TableText) RowName(i int) string {
	r := t.Rows[i]
	to := strings.Join(r.To, ",")
	if r.Stays {
		to = "(unchanged)"
	}
	return fmt.Sprintf("%s.%s %s -> %s on %s", t.Object, t.Field, strings.Join(r.From, ","), to,
		strings.Join(r.Triggers, ","))
}

// Texts returns the values as text.
func Texts[S any](values []S) []string {
	out := make([]string, 0, len(values))
	for _, v := range values {
		out = append(out, fmt.Sprint(v))
	}
	return out
}
