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

// Package fsm has the transition tables of state fields as data: the rows
// {from, to, trigger, guard, effects} of one state field, and the rules
// that apply them.
//
// A table is either applied or observed. The states of an applied table
// (the records of a driver) change only through Apply, which refuses a
// transition that no row has, a step that keeps the state included. The
// states of an observed table (the kernel's objects) are read, never
// applied: Check tells whether an observed change is a row of the table,
// and an unchanged state is always allowed.
package fsm

import (
	"errors"
	"fmt"
	"slices"
)

// Trigger names the action or the event that causes a transition.
type Trigger string

// AnyTrigger is the trigger of an observation, which does not know what
// caused a change: it matches the row of every trigger.
const AnyTrigger Trigger = ""

// Transition is one row of a transition table: an object in a state of From
// goes to To (or stays in its state, Stays) when one of Triggers happens as
// When says and Guard holds, with Effects. A row of an observed table may
// have no Triggers: When says what changes the state.
type Transition[S comparable] struct {
	From     []S
	To       S
	Stays    bool
	Triggers []Trigger
	When     string
	Guard    string
	Effects  string
}

// Refusal is a set of transitions that a table refuses on purpose: from a
// state of From to a state of To (every state if To is empty), on every
// trigger but those of Except. It is documentation: a refused transition
// has no row, so Apply and Check refuse it (the tests of a table check
// that no row contradicts a refusal).
type Refusal[S comparable] struct {
	From   []S
	To     []S
	Except []Trigger
	Why    string
}

// Table is the transition table of one state field of an object: its
// states and its transitions. Observed tells that the states are observed
// (Check), never applied (Apply).
type Table[S comparable] struct {
	Object   string
	Field    string
	Observed bool
	States   []S
	Rows     []Transition[S]
	Refused  []Refusal[S]
}

// List returns its arguments as a slice: the states of a row or a table.
func List[S any](values ...S) []S {
	return values
}

// ErrTransition is the kind of the error of a transition that is not in a
// table.
var ErrTransition = errors.New("transition not in the table")

// TransitionError is a transition that a table does not have.
type TransitionError struct {
	Object, Field string
	From, To      string
	// Trigger is the trigger of the transition, AnyTrigger for an
	// observation.
	Trigger Trigger
	// Why tells why the transition is refused when it is not because no
	// row has it.
	Why string
}

// Error returns the refused transition in words.
func (e *TransitionError) Error() string {
	on := ""
	if e.Trigger != AnyTrigger {
		on = " on " + string(e.Trigger)
	}
	why := ErrTransition.Error()
	if e.Why != "" {
		why = e.Why
	}
	return fmt.Sprintf("%s.%s: %s -> %s%s: %s", e.Object, e.Field, e.From, e.To, on, why)
}

// Is tells whether target is ErrTransition.
func (e *TransitionError) Is(target error) bool {
	return target == ErrTransition
}

// row returns the index of the first row that takes from to to on
// trigger, -1 if there is none.
func (t *Table[S]) row(from, to S, trigger Trigger) int {
	for i := range t.Rows {
		r := &t.Rows[i]
		if !slices.Contains(r.From, from) {
			continue
		}
		if trigger != AnyTrigger && !slices.Contains(r.Triggers, trigger) {
			continue
		}
		if (r.Stays && to == from) || (!r.Stays && to == r.To) {
			return i
		}
	}
	return -1
}

// Allows tells whether the table has a row that takes from to to on
// trigger; AnyTrigger matches every row.
func (t *Table[S]) Allows(from, to S, trigger Trigger) bool {
	return t.row(from, to, trigger) >= 0
}

// Check returns nil if the table has the transition from -> to on trigger,
// or if the table is observed and the state is unchanged; a
// *TransitionError otherwise. AnyTrigger matches every row.
func (t *Table[S]) Check(from, to S, trigger Trigger) error {
	if (t.Observed && from == to) || t.Allows(from, to, trigger) {
		return nil
	}
	return t.refusal(from, to, trigger, "")
}

// refusal returns the *TransitionError of from -> to on trigger.
func (t *Table[S]) refusal(from, to S, trigger Trigger, why string) error {
	return &TransitionError{
		Object:  t.Object,
		Field:   t.Field,
		From:    fmt.Sprint(from),
		To:      fmt.Sprint(to),
		Trigger: trigger,
		Why:     why,
	}
}

// Apply sets *state to to if the table has a row that takes *state to to
// on trigger, a step that keeps the state included, and counts the row
// (CountRows). It returns a *TransitionError, leaving *state unchanged,
// for a transition that no row has, for AnyTrigger and for an observed
// table.
func Apply[S comparable](t *Table[S], state *S, to S, trigger Trigger) error {
	switch {
	case t.Observed:
		return t.refusal(*state, to, trigger, "the states of the table are observed, never applied")
	case trigger == AnyTrigger:
		return t.refusal(*state, to, trigger, "a transition is applied on a trigger")
	}
	i := t.row(*state, to, trigger)
	if i < 0 {
		return t.refusal(*state, to, trigger, "")
	}
	if c := counting.Load(); c != nil {
		c.add(t.Object, t.Field, i)
	}
	*state = to
	return nil
}
