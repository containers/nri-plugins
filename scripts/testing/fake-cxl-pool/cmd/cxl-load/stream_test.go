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

package main

import (
	"testing"
)

func TestIsWriteBlock(t *testing.T) {
	for _, percent := range []int{0, 1, 10, 25, 33, 50, 99, 100} {
		// Any 100 consecutive blocks have percent written blocks.
		for _, from := range []int{0, 1, 37, 1000} {
			written := 0
			for n := from; n < from+100; n++ {
				if isWriteBlock(n, percent) {
					written++
				}
			}
			if written != percent {
				t.Errorf("%d%% writes: %d of the 100 blocks from %d are written", percent, written, from)
			}
		}
	}
	// 25% is one block in four, never two in a row.
	for n := range 400 {
		if got, want := isWriteBlock(n, 25), n%4 == 3; got != want {
			t.Errorf("isWriteBlock(%d, 25) = %v, want %v", n, got, want)
		}
	}
}

func TestWords(t *testing.T) {
	buf := make([]byte, 64)
	w := words(buf)
	if len(w) != 8 {
		t.Fatalf("words of 64 bytes: %d, want 8", len(w))
	}
	w[1] = 0x0102030405060708
	if buf[8] == 0 && buf[15] == 0 {
		t.Errorf("a word write is not in the bytes: %v", buf[8:16])
	}
	if got := words(make([]byte, 7)); got != nil {
		t.Errorf("words of 7 bytes: %v, want none", got)
	}
}

func TestReadWords(t *testing.T) {
	// Lengths around the unrolled rounds of 8 words and their tail.
	for _, n := range []int{0, 1, 7, 8, 9, 15, 16, 17, 100} {
		w := make([]uint64, n)
		var want uint64
		for i := range w {
			w[i] = uint64(i*i + 3)
			want += w[i]
		}
		if got := readWords(w); got != want {
			t.Errorf("readWords of %d words = %d, want %d", n, got, want)
		}
	}
}

func TestWriteWords(t *testing.T) {
	for _, n := range []int{0, 1, 7, 8, 9, 16, 21} {
		w := make([]uint64, n)
		next := writeWords(w, 1000)
		for i := range w {
			if w[i] != uint64(1000+i) {
				t.Errorf("writeWords of %d words: word %d is %d, want %d", n, i, w[i], 1000+i)
			}
		}
		if next != uint64(1000+n) {
			t.Errorf("writeWords of %d words returns %d, want %d", n, next, 1000+n)
		}
	}
}

func BenchmarkReadWords(b *testing.B) {
	w := make([]uint64, 32<<10)
	b.SetBytes(int64(len(w) * wordSize))
	for b.Loop() {
		readWords(w)
	}
}

func BenchmarkWriteWords(b *testing.B) {
	w := make([]uint64, 32<<10)
	b.SetBytes(int64(len(w) * wordSize))
	var v uint64
	for b.Loop() {
		v = writeWords(w, v)
	}
}
