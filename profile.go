// Copyright 2019 The Scriggo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package scriggo

import (
	"time"
)

// CallKind distinguishes different types of calls in profiling.
// Internal implementation: runtime.ProfileCallKind (internal/runtime/profiler.go)
type CallKind int

const (
	// CallKindScriggoFunc is a call to a Scriggo-defined function.
	CallKindScriggoFunc CallKind = iota
	// CallKindNativeFunc is a call to a native Go function.
	CallKindNativeFunc
	// CallKindMacro is a macro invocation.
	CallKindMacro
)

// String returns a string representation of the CallKind.
func (k CallKind) String() string {
	switch k {
	case CallKindScriggoFunc:
		return "scriggo_func"
	case CallKindNativeFunc:
		return "native_func"
	case CallKindMacro:
		return "macro"
	default:
		return "unknown"
	}
}

// CallRecord represents a single function or macro invocation during profiling.
// Internal implementation: runtime.ProfileCallRecord (internal/runtime/profiler.go)
type CallRecord struct {
	// ID is a unique identifier for this call.
	ID uint64
	// ParentID links to the parent call (0 for root calls).
	ParentID uint64

	// Name is the function or macro name.
	Name string
	// Package is the package path (empty for macros).
	Package string
	// File is the source file path.
	File string
	// Line is the source line number.
	Line int

	// Kind indicates the type of call.
	Kind CallKind

	// StartTime is when the call began.
	StartTime time.Time
	// Duration is how long the call took.
	Duration time.Duration
	// SelfDuration excludes time spent in child calls.
	SelfDuration time.Duration

	// TemplatePath is the template path for include calls.
	TemplatePath string
}

// CallNode represents a node in the call tree.
type CallNode struct {
	// Call is the call record for this node (nil for the root node).
	Call *CallRecord
	// Children are the child calls.
	Children []*CallNode
}

// FunctionStats contains per-function statistics.
type FunctionStats struct {
	// Name is the function or macro name.
	Name string
	// Package is the package path (empty for macros).
	Package string
	// CallCount is the number of times this function was called.
	CallCount int
	// TotalTime is the sum of all call durations.
	TotalTime time.Duration
	// SelfTime is the sum of all self durations.
	SelfTime time.Duration
	// MinTime is the minimum call duration.
	MinTime time.Duration
	// MaxTime is the maximum call duration.
	MaxTime time.Duration
}

// ProfileStats contains aggregated profiling statistics.
type ProfileStats struct {
	// TotalCalls is the total number of function/macro calls.
	TotalCalls int
	// NativeCalls is the count of native Go function calls.
	NativeCalls int
	// ScriggoCalls is the count of Scriggo function calls.
	ScriggoCalls int
	// MacroCalls is the count of macro invocations.
	MacroCalls int
	// ByFunction maps function key (pkg.name or name for macros) to stats.
	ByFunction map[string]*FunctionStats
}

// Profile contains the profiling results from a template execution.
// Internal implementation: runtime.ProfileData (internal/runtime/profiler.go)
type Profile struct {
	// StartTime is when execution began.
	StartTime time.Time
	// EndTime is when execution completed.
	EndTime time.Time
	// TotalDuration is the wall-clock execution time.
	TotalDuration time.Duration

	// Calls contains all recorded function/macro calls in order.
	Calls []*CallRecord

	// Lazily computed results.
	tree  *CallNode
	stats *ProfileStats
}

// Tree returns the hierarchical call tree. The root node has a nil Call
// and its Children are the top-level calls. This method builds the tree
// lazily on first call and caches the result.
func (p *Profile) Tree() *CallNode {
	if p.tree != nil {
		return p.tree
	}

	root := &CallNode{}
	if len(p.Calls) == 0 {
		p.tree = root
		return root
	}

	// Build a map of ID to node for quick parent lookup.
	nodeByID := make(map[uint64]*CallNode, len(p.Calls))
	for _, call := range p.Calls {
		nodeByID[call.ID] = &CallNode{Call: call}
	}

	// Link children to parents.
	for _, call := range p.Calls {
		node := nodeByID[call.ID]
		if call.ParentID == 0 {
			root.Children = append(root.Children, node)
		} else if parent, ok := nodeByID[call.ParentID]; ok {
			parent.Children = append(parent.Children, node)
		} else {
			// Orphan call, add to root.
			root.Children = append(root.Children, node)
		}
	}

	p.tree = root
	return root
}

// Stats returns aggregated statistics by function. This method computes
// the statistics lazily on first call and caches the result.
func (p *Profile) Stats() *ProfileStats {
	if p.stats != nil {
		return p.stats
	}

	stats := &ProfileStats{
		ByFunction: make(map[string]*FunctionStats),
	}

	for _, call := range p.Calls {
		stats.TotalCalls++

		switch call.Kind {
		case CallKindNativeFunc:
			stats.NativeCalls++
		case CallKindScriggoFunc:
			stats.ScriggoCalls++
		case CallKindMacro:
			stats.MacroCalls++
		}

		// Build function key.
		key := call.Name
		if call.Package != "" {
			key = call.Package + "." + call.Name
		}

		fs, ok := stats.ByFunction[key]
		if !ok {
			fs = &FunctionStats{
				Name:    call.Name,
				Package: call.Package,
				MinTime: call.Duration,
				MaxTime: call.Duration,
			}
			stats.ByFunction[key] = fs
		}

		fs.CallCount++
		fs.TotalTime += call.Duration
		fs.SelfTime += call.SelfDuration

		if call.Duration < fs.MinTime {
			fs.MinTime = call.Duration
		}
		if call.Duration > fs.MaxTime {
			fs.MaxTime = call.Duration
		}
	}

	p.stats = stats
	return stats
}
