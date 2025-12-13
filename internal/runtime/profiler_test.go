// Copyright 2019 The Scriggo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime

import (
	"testing"
	"time"
)

func TestProfiler_NewProfiler(t *testing.T) {
	p := newProfiler()
	if p == nil {
		t.Fatal("newProfiler returned nil")
	}
	if p.nextID != 1 {
		t.Errorf("expected nextID to be 1, got %d", p.nextID)
	}
	if len(p.callStack) != 0 {
		t.Errorf("expected empty callStack, got %d elements", len(p.callStack))
	}
	if len(p.records) != 0 {
		t.Errorf("expected empty records, got %d elements", len(p.records))
	}
}

func TestProfiler_EnterCall_SingleCall(t *testing.T) {
	p := newProfiler()
	p.enterCall("func1", "pkg", "file.go", 10, ProfileCallKindNativeFunc, "")

	if len(p.records) != 1 {
		t.Fatalf("expected 1 record, got %d", len(p.records))
	}

	record := p.records[0]
	if record.ID != 1 {
		t.Errorf("expected first call ID to be 1, got %d", record.ID)
	}
	if record.Name != "func1" {
		t.Errorf("expected name 'func1', got '%s'", record.Name)
	}
	if record.Package != "pkg" {
		t.Errorf("expected package 'pkg', got '%s'", record.Package)
	}
	if record.File != "file.go" {
		t.Errorf("expected file 'file.go', got '%s'", record.File)
	}
	if record.Line != 10 {
		t.Errorf("expected line 10, got %d", record.Line)
	}
	if record.Kind != ProfileCallKindNativeFunc {
		t.Errorf("expected kind NativeFunc, got %d", record.Kind)
	}
	if record.ParentID != 0 {
		t.Errorf("expected root call to have parentID 0, got %d", record.ParentID)
	}
}

func TestProfiler_EnterCall_NestedCalls(t *testing.T) {
	p := newProfiler()

	p.enterCall("func1", "pkg", "file.go", 10, ProfileCallKindNativeFunc, "")
	p.enterCall("func2", "pkg", "file.go", 20, ProfileCallKindMacro, "template.html")

	if len(p.records) != 2 {
		t.Fatalf("expected 2 records, got %d", len(p.records))
	}
	if p.records[0].ID != 1 {
		t.Errorf("expected first call ID to be 1, got %d", p.records[0].ID)
	}
	if p.records[1].ID != 2 {
		t.Errorf("expected second call ID to be 2, got %d", p.records[1].ID)
	}
	if len(p.callStack) != 2 {
		t.Errorf("expected callStack length 2, got %d", len(p.callStack))
	}

	// Check that func2 has func1 as parent
	record2 := p.records[1]
	if record2.ParentID != 1 {
		t.Errorf("expected func2 parent to be 1, got %d", record2.ParentID)
	}
	if record2.TemplatePath != "template.html" {
		t.Errorf("expected template path 'template.html', got '%s'", record2.TemplatePath)
	}
}

func TestProfiler_ExitCall(t *testing.T) {
	p := newProfiler()

	p.enterCall("func1", "", "", 0, ProfileCallKindNativeFunc, "")
	time.Sleep(5 * time.Millisecond)
	ok := p.exitCall()

	if !ok {
		t.Error("exitCall should return true for valid exit")
	}

	record := p.records[0]
	if record.Duration < 5*time.Millisecond {
		t.Errorf("duration too short: %v (expected >= 5ms)", record.Duration)
	}
	if len(p.callStack) != 0 {
		t.Errorf("expected empty callStack after exit, got %d elements", len(p.callStack))
	}
}

func TestProfiler_ExitCall_EmptyStack(t *testing.T) {
	p := newProfiler()
	// Should return false when exiting with empty stack
	ok := p.exitCall()
	if ok {
		t.Error("exitCall should return false for empty stack")
	}
}

func TestProfiler_SelfDuration(t *testing.T) {
	p := newProfiler()

	// Enter parent call
	p.enterCall("parent", "", "", 0, ProfileCallKindNativeFunc, "")
	time.Sleep(2 * time.Millisecond)

	// Enter child call
	p.enterCall("child", "", "", 0, ProfileCallKindNativeFunc, "")
	time.Sleep(5 * time.Millisecond)
	p.exitCall() // exit child

	time.Sleep(2 * time.Millisecond)
	p.exitCall() // exit parent

	parentRecord := p.records[0]
	childRecord := p.records[1]

	// Parent's total duration should be >= 9ms (2 + 5 + 2)
	if parentRecord.Duration < 9*time.Millisecond {
		t.Errorf("parent duration too short: %v (expected >= 9ms)", parentRecord.Duration)
	}

	// Child's duration should be >= 5ms
	if childRecord.Duration < 5*time.Millisecond {
		t.Errorf("child duration too short: %v (expected >= 5ms)", childRecord.Duration)
	}

	// Parent's self duration should be roughly 4ms (total minus child)
	// Allow some tolerance for timing variance
	if parentRecord.SelfDuration < 3*time.Millisecond {
		t.Errorf("parent self duration too short: %v (expected >= 3ms)", parentRecord.SelfDuration)
	}

	// Parent's self duration should be less than total duration
	if parentRecord.SelfDuration >= parentRecord.Duration {
		t.Error("parent self duration should be less than total duration")
	}
}

func TestProfiler_ChildDurationCleanup(t *testing.T) {
	p := newProfiler()

	// Enter and exit a call
	p.enterCall("func1", "", "", 0, ProfileCallKindNativeFunc, "")
	p.exitCall()

	// childDuration map should be cleaned up
	if len(p.childDuration) != 0 {
		t.Errorf("expected childDuration to be empty after exit, got %d entries", len(p.childDuration))
	}
}

func TestProfiler_GetData(t *testing.T) {
	p := newProfiler()

	p.enterCall("func1", "", "", 0, ProfileCallKindNativeFunc, "")
	p.exitCall()
	p.enterCall("func2", "", "", 0, ProfileCallKindMacro, "")
	p.exitCall()

	data := p.getData()

	if data == nil {
		t.Fatal("getData returned nil")
	}
	if len(data.Calls) != 2 {
		t.Errorf("expected 2 calls, got %d", len(data.Calls))
	}
	if data.TotalDuration <= 0 {
		t.Error("expected positive total duration")
	}
	if data.StartTime.IsZero() {
		t.Error("expected non-zero start time")
	}
	if data.EndTime.IsZero() {
		t.Error("expected non-zero end time")
	}
	if !data.EndTime.After(data.StartTime) {
		t.Error("end time should be after start time")
	}
}

func TestProfiler_MultipleRootCalls(t *testing.T) {
	p := newProfiler()

	// Two sequential root-level calls
	p.enterCall("func1", "", "", 0, ProfileCallKindNativeFunc, "")
	p.exitCall()

	p.enterCall("func2", "", "", 0, ProfileCallKindNativeFunc, "")
	p.exitCall()

	record1 := p.records[0]
	record2 := p.records[1]

	// Both should have parentID 0 (root level)
	if record1.ParentID != 0 {
		t.Errorf("expected func1 parentID 0, got %d", record1.ParentID)
	}
	if record2.ParentID != 0 {
		t.Errorf("expected func2 parentID 0, got %d", record2.ParentID)
	}
}

func TestProfiler_AllCallKinds(t *testing.T) {
	tests := []struct {
		kind ProfileCallKind
		name string
	}{
		{ProfileCallKindScriggoFunc, "scriggo_func"},
		{ProfileCallKindNativeFunc, "native_func"},
		{ProfileCallKindMacro, "macro"},
		{ProfileCallKindInclude, "include"},
		{ProfileCallKindDynamicInclude, "dynamic_include"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := newProfiler()
			p.enterCall(tc.name, "", "", 0, tc.kind, "")
			p.exitCall()

			record := p.records[0]
			if record.Kind != tc.kind {
				t.Errorf("expected kind %d, got %d", tc.kind, record.Kind)
			}
		})
	}
}

func TestProfiler_DeeplyNested(t *testing.T) {
	p := newProfiler()
	depth := 10

	// Enter nested calls
	for i := 0; i < depth; i++ {
		p.enterCall("func", "", "", i, ProfileCallKindNativeFunc, "")
	}

	if len(p.callStack) != depth {
		t.Errorf("expected callStack length %d, got %d", depth, len(p.callStack))
	}

	// Exit all calls in reverse order
	for i := depth - 1; i >= 0; i-- {
		p.exitCall()
		if len(p.callStack) != i {
			t.Errorf("after exiting call %d, expected callStack length %d, got %d", i, i, len(p.callStack))
		}
	}

	// Verify parent-child relationships
	for i := 1; i < depth; i++ {
		record := p.records[i]
		expectedParentID := p.records[i-1].ID
		if record.ParentID != expectedParentID {
			t.Errorf("call %d should have parent %d, got %d", i, expectedParentID, record.ParentID)
		}
	}
}

func TestProfiler_CapacityConstants(t *testing.T) {
	// Verify that constants are reasonable values
	if profilerCallStackCapacity < 16 {
		t.Errorf("profilerCallStackCapacity too small: %d", profilerCallStackCapacity)
	}
	if profilerRecordsCapacity < 32 {
		t.Errorf("profilerRecordsCapacity too small: %d", profilerRecordsCapacity)
	}
}
