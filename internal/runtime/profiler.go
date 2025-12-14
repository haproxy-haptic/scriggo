// Copyright 2019 The Scriggo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime

import (
	"time"
)

// Initial capacity constants for profiler data structures.
const (
	profilerCallStackCapacity = 32
	profilerRecordsCapacity   = 64
)

// ProfileCallKind distinguishes different types of calls in profiling.
// These values map to scriggo.CallKind in the public API (see profile.go).
type ProfileCallKind int

const (
	ProfileCallKindScriggoFunc ProfileCallKind = iota
	ProfileCallKindNativeFunc
	ProfileCallKindMacro
)

// ProfileCallRecord represents a single function or macro invocation during profiling.
// This type maps to scriggo.CallRecord in the public API (see profile.go).
type ProfileCallRecord struct {
	ID           uint64
	ParentID     uint64
	Name         string
	Package      string
	File         string
	Line         int
	Kind         ProfileCallKind
	StartTime    time.Time
	Duration     time.Duration
	SelfDuration time.Duration
	TemplatePath string
}

// ProfileData contains the raw profiling data collected during execution.
// This type maps to scriggo.Profile in the public API (see profile.go).
type ProfileData struct {
	StartTime     time.Time
	EndTime       time.Time
	TotalDuration time.Duration
	Calls         []*ProfileCallRecord
}

// profiler tracks execution timing when profiling is enabled.
type profiler struct {
	nextID        uint64
	callStack     []uint64 // stack of active call IDs
	records       []*ProfileCallRecord
	recordsByID   map[uint64]*ProfileCallRecord
	startTime     time.Time
	childDuration map[uint64]time.Duration // tracks child duration for self-time calculation
}

// newProfiler creates a new profiler instance.
func newProfiler() *profiler {
	return &profiler{
		nextID:        1,
		callStack:     make([]uint64, 0, profilerCallStackCapacity),
		records:       make([]*ProfileCallRecord, 0, profilerRecordsCapacity),
		recordsByID:   make(map[uint64]*ProfileCallRecord),
		startTime:     time.Now(),
		childDuration: make(map[uint64]time.Duration),
	}
}

// enterCall records the start of a function/macro call and pushes it onto the call stack.
func (p *profiler) enterCall(name, pkg, file string, line int, kind ProfileCallKind, templatePath string) {
	id := p.nextID
	p.nextID++

	var parentID uint64
	if len(p.callStack) > 0 {
		parentID = p.callStack[len(p.callStack)-1]
	}

	record := &ProfileCallRecord{
		ID:           id,
		ParentID:     parentID,
		Name:         name,
		Package:      pkg,
		File:         file,
		Line:         line,
		Kind:         kind,
		StartTime:    time.Now(),
		TemplatePath: templatePath,
	}

	p.records = append(p.records, record)
	p.recordsByID[id] = record
	p.callStack = append(p.callStack, id)
}

// exitCall records the end of the current top-of-stack function/macro call.
// It calculates the duration and self-duration, then pops the call from the stack.
// Returns false if the call stack is empty (mismatched enter/exit), true otherwise.
func (p *profiler) exitCall() bool {
	if len(p.callStack) == 0 {
		return false
	}

	// Pop the call ID from the stack
	id := p.callStack[len(p.callStack)-1]
	p.callStack = p.callStack[:len(p.callStack)-1]

	record, ok := p.recordsByID[id]
	if !ok {
		return false
	}

	record.Duration = time.Since(record.StartTime)

	// Calculate self duration by subtracting child durations
	childDur := p.childDuration[id]
	record.SelfDuration = record.Duration - childDur

	// Add this call's duration to parent's child duration
	if record.ParentID != 0 {
		p.childDuration[record.ParentID] += record.Duration
	}

	// Clean up childDuration entry to prevent memory growth
	delete(p.childDuration, id)

	return true
}

// getData returns the collected profiling data.
func (p *profiler) getData() *ProfileData {
	endTime := time.Now()
	return &ProfileData{
		StartTime:     p.startTime,
		EndTime:       endTime,
		TotalDuration: endTime.Sub(p.startTime),
		Calls:         p.records,
	}
}
