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

#include "textflag.h"

// func flushLine(addr uintptr)
TEXT ·flushLine(SB), NOSPLIT, $0-8
	MOVQ addr+0(FP), AX
	CLFLUSH (AX)
	RET

// func memoryFence()
TEXT ·memoryFence(SB), NOSPLIT, $0-0
	MFENCE
	RET
