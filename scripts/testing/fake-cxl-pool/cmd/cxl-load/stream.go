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
	"unsafe"
)

// wordSize is the size in bytes of the unit of the stream: a 64-bit word.
const wordSize = 8

// words returns the memory of buf as 64-bit words.
func words(buf []byte) []uint64 {
	if len(buf) < wordSize {
		return nil
	}
	return unsafe.Slice((*uint64)(unsafe.Pointer(unsafe.SliceData(buf))), len(buf)/wordSize)
}

// readWords returns the sum of the words of w, reading all of them. A
// whole cache line (8 words) is read per round.
func readWords(w []uint64) uint64 {
	var s0, s1, s2, s3 uint64
	for len(w) >= 8 {
		line := (*[8]uint64)(w)
		s0 += line[0] + line[4]
		s1 += line[1] + line[5]
		s2 += line[2] + line[6]
		s3 += line[3] + line[7]
		w = w[8:]
	}
	for _, v := range w {
		s0 += v
	}
	return s0 + s1 + s2 + s3
}

// writeWords stores first, first+1, ... into the words of w and returns
// the value that follows the last one.
func writeWords(w []uint64, first uint64) uint64 {
	v := first
	for len(w) >= 8 {
		line := (*[8]uint64)(w)
		line[0], line[1], line[2], line[3] = v, v+1, v+2, v+3
		line[4], line[5], line[6], line[7] = v+4, v+5, v+6, v+7
		v += 8
		w = w[8:]
	}
	for i := range w {
		w[i] = v
		v++
	}
	return v
}

// isWriteBlock tells if the block number n of a stream is written, not
// read, when percent of the blocks are written: exactly percent blocks of
// every 100 consecutive ones, evenly spread.
func isWriteBlock(n, percent int) bool {
	return (n+1)*percent/100 > n*percent/100
}
