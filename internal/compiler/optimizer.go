// Copyright 2024 The Scriggo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package compiler

import (
	"gitlab.com/haproxy-haptic/scriggo/internal/runtime"
)

// peepholeOptimize performs peephole optimization on the instruction stream,
// fusing common instruction sequences into super-instructions.
//
// Currently optimizes:
// - OpField + OpShow -> OpFieldShow
// - OpMapIndex + OpShow -> OpMapIndexShow
func peepholeOptimize(body []runtime.Instruction) []runtime.Instruction {
	if len(body) < 2 {
		return body
	}

	result := make([]runtime.Instruction, 0, len(body))
	i := 0

	for i < len(body) {
		// Check if we have room for a two-instruction pattern
		if i+1 < len(body) {
			fused, consumed := tryFuse(body[i], body[i+1])
			if consumed > 0 {
				result = append(result, fused...)
				i += consumed
				continue
			}
		}
		// No fusion, copy instruction as-is
		result = append(result, body[i])
		i++
	}

	return result
}

// tryFuse attempts to fuse two consecutive instructions into a super-instruction.
// Returns the fused instructions and the number of original instructions consumed.
// If no fusion is possible, returns nil, 0.
func tryFuse(ins1, ins2 runtime.Instruction) ([]runtime.Instruction, int) {
	// Check if ins2 is OpShow (positive or negative variant)
	isShow := ins2.Op == runtime.OpShow || ins2.Op == -runtime.OpShow
	if !isShow {
		return nil, 0
	}

	// Check register dependency: ins1's output (C) must match ins2's input (B)
	if ins1.C != ins2.B {
		return nil, 0
	}

	// Pattern: OpField + OpShow -> OpFieldShow
	if ins1.Op == runtime.OpField {
		return []runtime.Instruction{
			{Op: runtime.OpFieldShow, A: ins1.A, B: ins1.B, C: ins2.C},
			{Op: runtime.Operation(ins2.A)}, // type index stored in Op field
		}, 2
	}

	// Pattern: OpMapIndex + OpShow -> OpMapIndexShow
	if ins1.Op == runtime.OpMapIndex || ins1.Op == -runtime.OpMapIndex {
		op := runtime.OpMapIndexShow
		if ins1.Op < 0 {
			op = -runtime.OpMapIndexShow
		}
		return []runtime.Instruction{
			{Op: op, A: ins1.A, B: ins1.B, C: ins2.C},
			{Op: runtime.Operation(ins2.A)},
		}, 2
	}

	return nil, 0
}
