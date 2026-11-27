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

package server

import "testing"

func TestSerials(t *testing.T) {
	s := newSerials(0xc1ae0000, 0xc1ee0000)
	if err := s.Use(0xc1ee0001, "a"); err != nil {
		t.Fatal(err)
	}
	if sn := s.Next("b", false); sn != 0xc1ee0002 {
		t.Fatalf("unexpected serial 0x%x", sn)
	}
	if sn := s.Next("s", true); sn != 0xc1ae0001 {
		t.Fatalf("unexpected shared serial 0x%x", sn)
	}
	if err := s.Use(0xc1ee0002, "c"); err == nil {
		t.Fatal("expected conflict")
	}
	if err := s.Use(0xc1ae0001, "c"); err == nil {
		t.Fatal("expected conflict across kinds")
	}
	// uniqueness is over all serials: an explicit exclusive-looking serial
	// on a shared device blocks it for the exclusive allocator too
	if err := s.Use(0xc1ee0003, "s2"); err != nil {
		t.Fatal(err)
	}
	if sn := s.Next("e", false); sn != 0xc1ee0004 {
		t.Fatalf("unexpected serial 0x%x", sn)
	}
	s.Free(0xc1ee0001)
	if sn := s.Next("d", false); sn != 0xc1ee0001 {
		t.Fatalf("unexpected serial 0x%x", sn)
	}
	if sn := s.NextExcept("x", true, func(sn uint64) bool { return sn == 0xc1ae0002 }); sn != 0xc1ae0003 {
		t.Fatalf("unexpected serial 0x%x", sn)
	}
}
