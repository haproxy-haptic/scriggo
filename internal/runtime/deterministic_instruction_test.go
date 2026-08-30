// Copyright 2026 The Scriggo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime

import (
	"errors"
	"testing"
)

func TestProgramValidatorsSkipInstructionOperandWords(t *testing.T) {
	callee := &Function{}
	forbiddenOperand := Instruction{Op: OpGo, A: int8(OpGoRender), B: int8(OpGoRenderIndirect)}
	main := &Function{
		Body: []Instruction{
			{Op: OpCallFunc},
			forbiddenOperand,
			{Op: OpReturn},
		},
		Functions: []*Function{callee},
	}
	if err := ValidateDeterministicProgram(main); err != nil {
		t.Fatalf("deterministic validation rejected operand words: %v", err)
	}
	if err := ValidateBatchProgram(main); err != nil {
		t.Fatalf("batch validation rejected operand words: %v", err)
	}

	for _, operation := range []Operation{OpGo, OpGoRender, OpGoRenderIndirect} {
		program := &Function{Body: []Instruction{{Op: operation}}}
		if err := ValidateDeterministicProgram(program); !errors.Is(err, ErrDeterministicGo) {
			t.Fatalf("deterministic validation error for %v = %v, want %v", operation, err, ErrDeterministicGo)
		}
		if err := ValidateBatchProgram(program); !errors.Is(err, ErrBatchDetachedWork) {
			t.Fatalf("batch validation error for %v = %v, want %v", operation, err, ErrBatchDetachedWork)
		}
	}
}
