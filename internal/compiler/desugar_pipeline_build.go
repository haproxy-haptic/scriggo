// Copyright (c) 2025 The Scriggo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package compiler

import (
	"strconv"

	"github.com/haproxy-haptic/scriggo/ast"
)

// buildLoweredPipeline emits the statements implementing a lowered chain:
//
//	var $pipeN    = <source>
//	var $pipeNfn0 = <closure 0>   // hoisted: one closure value, not one per element
//	var target    = <accumulator>
//	for _, $pipeNe0 := range $pipeN { …stages…; target = append(target, …) }
//
// The stages are fused into that single loop, so no stage allocates an
// intermediate slice.
func buildLoweredPipeline(target *ast.Identifier, source ast.Expression, stages []pipelineStage, comp *compilation) []ast.Node {
	pos := target.Position
	prefix := comp.generatePipelineName()

	srcIdent := ast.NewIdentifier(pos, prefix)
	nodes := []ast.Node{
		ast.NewVar(pos, []*ast.Identifier{srcIdent}, nil, []ast.Expression{source}),
	}

	// Hoist each closure into its own variable. Evaluating the literal inside
	// the loop would build a fresh closure value per element — correct, but it
	// would give back part of what the lowering is for.
	fnIdents := make([]*ast.Identifier, len(stages))
	for i, st := range stages {
		fnIdents[i] = ast.NewIdentifier(st.callPos, prefix+"fn"+strconv.Itoa(i))
		// A lambda took its parameter type from the call this hoist removes it
		// from, so point it at the values the stage receives instead.
		if st.fn.Type.Parameters[0].Type == nil {
			origin := &ast.LambdaOrigin{Source: ast.NewIdentifier(st.callPos, prefix)}
			if i > 0 {
				origin = &ast.LambdaOrigin{
					Prev:     ast.NewIdentifier(st.callPos, prefix+"fn"+strconv.Itoa(i-1)),
					PrevKind: stages[i-1].kind,
				}
			}
			st.fn.LambdaOrigin = origin
		}
		nodes = append(nodes, ast.NewVar(st.callPos, []*ast.Identifier{fnIdents[i]}, nil, []ast.Expression{st.fn}))
	}

	nodes = append(nodes, ast.NewVar(pos, []*ast.Identifier{target}, nil,
		[]ast.Expression{accumulatorInit(pos, srcIdent, stages, fnIdents)}))

	elem := ast.NewIdentifier(pos, prefix+"e0")
	body := buildStageBody(target, elem, stages, fnIdents, prefix, 0, 1)
	nodes = append(nodes, ast.NewForRange(pos,
		ast.NewAssignment(pos,
			[]ast.Expression{ast.NewIdentifier(pos, "_"), elem},
			ast.AssignmentDeclaration,
			[]ast.Expression{srcIdent}),
		body, nil))

	return nodes
}

// accumulatorInit produces the accumulator's initial value.
//
// The element type comes from the last stage that determines one: map's
// closure result, or flat_map's (already a slice). When every stage is a
// filter the element type is the source's, which has no name at this point —
// `src[:0:0]` expresses "same type, empty, no shared backing array" without
// needing one.
func accumulatorInit(pos *ast.Position, src *ast.Identifier, stages []pipelineStage, fns []*ast.Identifier) ast.Expression {
	zero := ast.NewBasicLiteral(pos, ast.IntLiteral, "0")
	empty := ast.NewSlicing(pos, src, nil, zero, zero, true)
	for i := len(stages) - 1; i >= 0; i-- {
		switch stages[i].kind {
		case "map", "flat_map":
			// Call the stage's own native on an empty slice. Its AdaptiveFunc
			// computes []U from the closure's result type at check time, so
			// the accumulator gets the right type without this pass naming it
			// — which it cannot do anyway: a nested resource type produced by
			// reflect.StructOf has no name, and an inferred closure has no
			// written result type at all.
			//
			// Cost is one native call per pipeline over a zero-length slice,
			// not one per element.
			return ast.NewCall(pos, ast.NewIdentifier(pos, stages[i].kind),
				[]ast.Expression{empty, fns[i]}, false)
		}
	}
	return empty
}

// buildStageBody builds the fused loop body from stage index i onward. elem
// names the value entering that stage; each stage either filters it, replaces
// it, or (flat_map) opens a nested range whose variable feeds the rest.
func buildStageBody(target, elem *ast.Identifier, stages []pipelineStage, fns []*ast.Identifier, prefix string, i, depth int) []ast.Node {
	if i == len(stages) {
		// End of the chain: keep the value.
		return []ast.Node{ast.NewAssignment(elem.Position,
			[]ast.Expression{target}, ast.AssignmentSimple,
			[]ast.Expression{ast.NewCall(elem.Position,
				ast.NewIdentifier(elem.Position, "append"),
				[]ast.Expression{target, elem}, false)})}
	}

	st := stages[i]
	pos := st.callPos
	call := ast.NewCall(pos, fns[i], []ast.Expression{elem}, false)

	switch st.kind {
	case "filter", "reject":
		cond := ast.Expression(call)
		if st.kind == "filter" {
			// `if !f(e) { continue }` — negating keeps the rest of the chain
			// at one indentation level regardless of stage count.
			cond = ast.NewUnaryOperator(pos, ast.OperatorNot, call)
		}
		guard := ast.NewIf(pos, nil, cond,
			ast.NewBlock(pos, []ast.Node{ast.NewContinue(pos, nil)}), nil)
		return append([]ast.Node{guard},
			buildStageBody(target, elem, stages, fns, prefix, i+1, depth)...)

	case "map":
		next := ast.NewIdentifier(pos, prefix+"e"+strconv.Itoa(depth))
		decl := ast.NewVar(pos, []*ast.Identifier{next}, nil, []ast.Expression{call})
		return append([]ast.Node{decl},
			buildStageBody(target, next, stages, fns, prefix, i+1, depth+1)...)

	default: // flat_map
		next := ast.NewIdentifier(pos, prefix+"e"+strconv.Itoa(depth))
		inner := buildStageBody(target, next, stages, fns, prefix, i+1, depth+1)
		return []ast.Node{ast.NewForRange(pos,
			ast.NewAssignment(pos,
				[]ast.Expression{ast.NewIdentifier(pos, "_"), next},
				ast.AssignmentDeclaration,
				[]ast.Expression{call}),
			inner, nil)}
	}
}
