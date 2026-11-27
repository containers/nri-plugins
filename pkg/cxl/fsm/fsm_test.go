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
	"errors"
	"slices"
	"testing"
)

const (
	open   = "OPEN"
	closed = "CLOSED"
	broken = "BROKEN"
)

// door is an applied table of two triggers with a stays row.
var door = Table[string]{
	Object: "Door",
	Field:  "State",
	States: []string{open, closed, broken},
	Rows: []Transition[string]{
		{From: []string{closed}, To: open, Triggers: []Trigger{"push"}, When: "pushed"},
		{From: []string{open}, To: closed, Triggers: []Trigger{"pull", "wind"}, Effects: "slams"},
		{From: []string{open}, Stays: true, Triggers: []Trigger{"push"}, When: "pushed again"},
		{From: []string{open, closed}, To: broken, Triggers: []Trigger{"kick"}, Guard: "hard"},
	},
	Refused: []Refusal[string]{{From: []string{broken}, Why: "a broken door stays broken"}},
}

// lamp is an observed table.
var lamp = Table[string]{
	Object:   "Lamp",
	Field:    "State",
	Observed: true,
	States:   []string{open, closed},
	Rows:     []Transition[string]{{From: []string{open}, To: closed, When: "switched off"}},
}

func TestApplyNeedsARow(t *testing.T) {
	s := closed
	if err := Apply(&door, &s, open, "push"); err != nil || s != open {
		t.Fatalf("CLOSED -> OPEN on push: %v, %s", err, s)
	}
	if err := Apply(&door, &s, open, "push"); err != nil || s != open {
		t.Fatalf("OPEN stays on push: %v, %s", err, s)
	}
	err := Apply(&door, &s, open, "pull")
	var te *TransitionError
	if !errors.As(err, &te) || !errors.Is(err, ErrTransition) || s != open {
		t.Fatalf("OPEN stays on pull: %v, %s", err, s)
	}
	if want := "Door.State: OPEN -> OPEN on pull: transition not in the table"; err.Error() != want {
		t.Errorf("error %q, want %q", err, want)
	}
	if err := Apply(&door, &s, closed, "wind"); err != nil || s != closed {
		t.Fatalf("OPEN -> CLOSED on wind: %v, %s", err, s)
	}
	if err := Apply(&door, &s, open, AnyTrigger); err == nil || s != closed {
		t.Fatalf("a transition applied without a trigger: %s", s)
	}
}

func TestApplyRefusesObservedTables(t *testing.T) {
	s := open
	err := Apply(&lamp, &s, closed, "switch")
	if !errors.Is(err, ErrTransition) || s != open {
		t.Fatalf("an observed table applied: %v, %s", err, s)
	}
	if want := "Lamp.State: OPEN -> CLOSED on switch: the states of the table are observed, never applied"; err.Error() != want {
		t.Errorf("error %q, want %q", err, want)
	}
}

func TestCheck(t *testing.T) {
	for _, c := range []struct {
		table    *Table[string]
		from, to string
		trigger  Trigger
		ok       bool
	}{
		{&lamp, open, closed, AnyTrigger, true},
		{&lamp, open, open, AnyTrigger, true},
		{&lamp, closed, closed, AnyTrigger, true},
		{&lamp, closed, open, AnyTrigger, false},
		{&door, open, open, "push", true},
		{&door, open, open, "pull", false},
		{&door, open, open, AnyTrigger, true},
		{&door, closed, closed, AnyTrigger, false},
		{&door, closed, broken, AnyTrigger, true},
		{&door, closed, broken, "push", false},
	} {
		err := c.table.Check(c.from, c.to, c.trigger)
		if (err == nil) != c.ok {
			t.Errorf("%s: %s -> %s on %q: %v", c.table.Object, c.from, c.to, c.trigger, err)
		}
	}
	err := lamp.Check(closed, open, AnyTrigger)
	if want := "Lamp.State: CLOSED -> OPEN: transition not in the table"; err == nil || err.Error() != want {
		t.Errorf("error %v, want %q", err, want)
	}
}

func TestCountRows(t *testing.T) {
	rows := CountRows()
	s := closed
	if err := Apply(&door, &s, open, "push"); err != nil {
		t.Fatal(err)
	}
	if err := door.Check(open, closed, "pull"); err != nil {
		t.Fatal(err)
	}
	if !rows.Applied("Door", "State", 0) || rows.Applied("Door", "State", 1) {
		t.Errorf("applied rows: 0 %v, 1 %v", rows.Applied("Door", "State", 0), rows.Applied("Door", "State", 1))
	}
	if got := rows.Unapplied(door.Text()); !slices.Equal(got, []int{1, 2, 3}) {
		t.Errorf("unapplied rows %v", got)
	}
	again := CountRows()
	if err := Apply(&door, &s, broken, "kick"); err != nil {
		t.Fatal(err)
	}
	if rows.Applied("Door", "State", 3) || !again.Applied("Door", "State", 3) || again.Applied("Door", "State", 0) {
		t.Errorf("a row counted in the wrong Rows")
	}
}

func TestText(t *testing.T) {
	tt := door.Text()
	if tt.Object != "Door" || tt.Field != "State" || tt.Observed || !slices.Equal(tt.States, []string{open, closed, broken}) {
		t.Fatalf("text %+v", tt)
	}
	if len(tt.Rows) != 4 {
		t.Fatalf("rows %+v", tt.Rows)
	}
	r := tt.Rows[1]
	if !slices.Equal(r.From, []string{open}) || !slices.Equal(r.To, []string{closed}) ||
		!slices.Equal(r.Triggers, []string{"pull", "wind"}) || r.Effects != "slams" {
		t.Errorf("row 1 %+v", r)
	}
	if r := tt.Rows[2]; !r.Stays || r.To != nil {
		t.Errorf("row 2 %+v", r)
	}
	if !slices.Equal(tt.Refused, []string{"BROKEN -> any state: a broken door stays broken"}) {
		t.Errorf("refused %v", tt.Refused)
	}
	if lt := lamp.Text(); !lt.Observed || lt.Rows[0].Triggers == nil || len(lt.Rows[0].Triggers) != 0 {
		t.Errorf("observed text %+v", lt)
	}
}
