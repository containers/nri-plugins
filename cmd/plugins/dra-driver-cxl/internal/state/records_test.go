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

package state_test

import (
	"reflect"
	"testing"
	"time"

	"github.com/containers/nri-plugins/cmd/plugins/dra-driver-cxl/internal/state"
)

// TestCloneSharesNothing fills every field of the records, clones them and
// checks that no pointer, slice or map of the clone points into the
// original.
func TestCloneSharesNothing(t *testing.T) {
	var r state.Records
	fill(reflect.ValueOf(&r).Elem(), 0)
	c := r.Clone()
	if !reflect.DeepEqual(r, c) {
		t.Fatalf("clone of the filled records differs")
	}
	shared(t, reflect.ValueOf(r), reflect.ValueOf(c), "Records")
}

var timeType = reflect.TypeOf(time.Time{})

// fill sets every field of v to a value that is not zero: one element in
// every slice and map, an allocated value behind every pointer.
func fill(v reflect.Value, depth int) {
	if depth > 12 {
		return
	}
	switch v.Kind() {
	case reflect.Struct:
		if v.Type() == timeType {
			v.Set(reflect.ValueOf(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)))
			return
		}
		for i := 0; i < v.NumField(); i++ {
			if v.Field(i).CanSet() {
				fill(v.Field(i), depth+1)
			}
		}
	case reflect.Pointer:
		p := reflect.New(v.Type().Elem())
		fill(p.Elem(), depth+1)
		v.Set(p)
	case reflect.Slice:
		s := reflect.MakeSlice(v.Type(), 1, 1)
		fill(s.Index(0), depth+1)
		v.Set(s)
	case reflect.Map:
		m := reflect.MakeMap(v.Type())
		k := reflect.New(v.Type().Key()).Elem()
		fill(k, depth+1)
		e := reflect.New(v.Type().Elem()).Elem()
		fill(e, depth+1)
		m.SetMapIndex(k, e)
		v.Set(m)
	case reflect.String:
		v.SetString("x")
	case reflect.Bool:
		v.SetBool(true)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		v.SetInt(1)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		v.SetUint(1)
	}
}

// shared reports the pointers, slices and maps of b that are those of a.
func shared(t *testing.T, a, b reflect.Value, path string) {
	t.Helper()
	switch a.Kind() {
	case reflect.Struct:
		if a.Type() == timeType {
			return
		}
		for i := 0; i < a.NumField(); i++ {
			shared(t, a.Field(i), b.Field(i), path+"."+a.Type().Field(i).Name)
		}
	case reflect.Pointer:
		if a.IsNil() {
			return
		}
		if a.Pointer() == b.Pointer() {
			t.Errorf("%s: the clone shares the pointer", path)
			return
		}
		shared(t, a.Elem(), b.Elem(), path)
	case reflect.Slice:
		if a.Len() == 0 {
			return
		}
		if a.Pointer() == b.Pointer() {
			t.Errorf("%s: the clone shares the slice", path)
			return
		}
		for i := 0; i < a.Len(); i++ {
			shared(t, a.Index(i), b.Index(i), path+"[]")
		}
	case reflect.Map:
		if a.Len() == 0 {
			return
		}
		if a.Pointer() == b.Pointer() {
			t.Errorf("%s: the clone shares the map", path)
			return
		}
		for _, k := range a.MapKeys() {
			shared(t, a.MapIndex(k), b.MapIndex(k), path+"[k]")
		}
	}
}

// TestFirstDax: the first dax device of a region is its whole device, else
// its daxN.0, "" for none.
func TestFirstDax(t *testing.T) {
	for _, c := range []struct {
		devs []state.DaxDevice
		want string
	}{
		{[]state.DaxDevice{{Name: "dax0.0"}, {Name: "dax0.1", Whole: true}}, "dax0.1"},
		{[]state.DaxDevice{{Name: "dax0.1"}, {Name: "dax0.0"}}, "dax0.0"},
		{[]state.DaxDevice{{Name: "dax0.1"}}, ""},
		{nil, ""},
	} {
		if got := (&state.Region{DaxDevices: c.devs}).FirstDax(); got != c.want {
			t.Errorf("%+v: %q, want %q", c.devs, got, c.want)
		}
	}
}
