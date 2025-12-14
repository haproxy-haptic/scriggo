// Copyright 2019 The Scriggo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package scriggo

import (
	"testing"
	"time"
)

func TestCallKind_String(t *testing.T) {
	tests := []struct {
		kind     CallKind
		expected string
	}{
		{CallKindScriggoFunc, "scriggo_func"},
		{CallKindNativeFunc, "native_func"},
		{CallKindMacro, "macro"},
		{CallKind(99), "unknown"},
	}
	for _, tc := range tests {
		if got := tc.kind.String(); got != tc.expected {
			t.Errorf("CallKind(%d).String() = %q, want %q", tc.kind, got, tc.expected)
		}
	}
}

func TestProfile_Tree_Empty(t *testing.T) {
	p := &Profile{}
	tree := p.Tree()
	if tree == nil {
		t.Fatal("Tree() returned nil for empty profile")
	}
	if tree.Call != nil {
		t.Error("root Call should be nil")
	}
	if len(tree.Children) != 0 {
		t.Error("root Children should be empty")
	}
}

func TestProfile_Tree_SingleCall(t *testing.T) {
	p := &Profile{
		Calls: []*CallRecord{
			{ID: 1, ParentID: 0, Name: "func1", Package: "main"},
		},
	}
	tree := p.Tree()
	if len(tree.Children) != 1 {
		t.Fatalf("expected 1 root child, got %d", len(tree.Children))
	}
	if tree.Children[0].Call.Name != "func1" {
		t.Errorf("expected func1, got %s", tree.Children[0].Call.Name)
	}
}

func TestProfile_Tree_NestedCalls(t *testing.T) {
	p := &Profile{
		Calls: []*CallRecord{
			{ID: 1, ParentID: 0, Name: "func1"},
			{ID: 2, ParentID: 1, Name: "func2"},
			{ID: 3, ParentID: 1, Name: "func3"},
			{ID: 4, ParentID: 2, Name: "func4"},
		},
	}
	tree := p.Tree()
	if len(tree.Children) != 1 {
		t.Fatalf("expected 1 root child, got %d", len(tree.Children))
	}
	func1 := tree.Children[0]
	if func1.Call.Name != "func1" {
		t.Errorf("expected func1, got %s", func1.Call.Name)
	}
	if len(func1.Children) != 2 {
		t.Fatalf("expected 2 children of func1, got %d", len(func1.Children))
	}
	// func2 should have func4 as child
	var func2 *CallNode
	for _, child := range func1.Children {
		if child.Call.Name == "func2" {
			func2 = child
			break
		}
	}
	if func2 == nil {
		t.Fatal("func2 not found")
	}
	if len(func2.Children) != 1 || func2.Children[0].Call.Name != "func4" {
		t.Error("func2 should have func4 as child")
	}
}

func TestProfile_Tree_Caching(t *testing.T) {
	p := &Profile{
		Calls: []*CallRecord{
			{ID: 1, ParentID: 0, Name: "func1"},
		},
	}
	tree1 := p.Tree()
	tree2 := p.Tree()
	if tree1 != tree2 {
		t.Error("Tree() should return cached result")
	}
}

func TestProfile_Stats_Empty(t *testing.T) {
	p := &Profile{}
	stats := p.Stats()
	if stats.TotalCalls != 0 {
		t.Errorf("expected 0 total calls, got %d", stats.TotalCalls)
	}
	if len(stats.ByFunction) != 0 {
		t.Errorf("expected 0 functions, got %d", len(stats.ByFunction))
	}
}

func TestProfile_Stats_SingleFunction(t *testing.T) {
	p := &Profile{
		Calls: []*CallRecord{
			{ID: 1, Name: "func1", Package: "main", Kind: CallKindNativeFunc,
				Duration: 10 * time.Millisecond, SelfDuration: 10 * time.Millisecond},
		},
	}
	stats := p.Stats()
	if stats.TotalCalls != 1 {
		t.Errorf("expected 1 total call, got %d", stats.TotalCalls)
	}
	if stats.NativeCalls != 1 {
		t.Errorf("expected 1 native call, got %d", stats.NativeCalls)
	}
	fs, ok := stats.ByFunction["main.func1"]
	if !ok {
		t.Fatal("func1 not found in stats")
	}
	if fs.CallCount != 1 {
		t.Errorf("expected call count 1, got %d", fs.CallCount)
	}
	if fs.TotalTime != 10*time.Millisecond {
		t.Errorf("expected total time 10ms, got %v", fs.TotalTime)
	}
}

func TestProfile_Stats_MultipleCallsSameFunction(t *testing.T) {
	p := &Profile{
		Calls: []*CallRecord{
			{ID: 1, Name: "func1", Package: "main", Kind: CallKindNativeFunc,
				Duration: 10 * time.Millisecond, SelfDuration: 10 * time.Millisecond},
			{ID: 2, Name: "func1", Package: "main", Kind: CallKindNativeFunc,
				Duration: 20 * time.Millisecond, SelfDuration: 20 * time.Millisecond},
			{ID: 3, Name: "func1", Package: "main", Kind: CallKindNativeFunc,
				Duration: 5 * time.Millisecond, SelfDuration: 5 * time.Millisecond},
		},
	}
	stats := p.Stats()
	if stats.TotalCalls != 3 {
		t.Errorf("expected 3 total calls, got %d", stats.TotalCalls)
	}
	fs, ok := stats.ByFunction["main.func1"]
	if !ok {
		t.Fatal("func1 not found in stats")
	}
	if fs.CallCount != 3 {
		t.Errorf("expected call count 3, got %d", fs.CallCount)
	}
	if fs.TotalTime != 35*time.Millisecond {
		t.Errorf("expected total time 35ms, got %v", fs.TotalTime)
	}
	if fs.MinTime != 5*time.Millisecond {
		t.Errorf("expected min time 5ms, got %v", fs.MinTime)
	}
	if fs.MaxTime != 20*time.Millisecond {
		t.Errorf("expected max time 20ms, got %v", fs.MaxTime)
	}
}

func TestProfile_Stats_DifferentCallKinds(t *testing.T) {
	p := &Profile{
		Calls: []*CallRecord{
			{ID: 1, Name: "native1", Kind: CallKindNativeFunc},
			{ID: 2, Name: "scriggo1", Kind: CallKindScriggoFunc},
			{ID: 3, Name: "macro1", Kind: CallKindMacro},
			{ID: 4, Name: "native2", Kind: CallKindNativeFunc},
		},
	}
	stats := p.Stats()
	if stats.TotalCalls != 4 {
		t.Errorf("expected 4 total calls, got %d", stats.TotalCalls)
	}
	if stats.NativeCalls != 2 {
		t.Errorf("expected 2 native calls, got %d", stats.NativeCalls)
	}
	if stats.ScriggoCalls != 1 {
		t.Errorf("expected 1 scriggo call, got %d", stats.ScriggoCalls)
	}
	if stats.MacroCalls != 1 {
		t.Errorf("expected 1 macro call, got %d", stats.MacroCalls)
	}
}

func TestProfile_Stats_Caching(t *testing.T) {
	p := &Profile{
		Calls: []*CallRecord{
			{ID: 1, Name: "func1"},
		},
	}
	stats1 := p.Stats()
	stats2 := p.Stats()
	if stats1 != stats2 {
		t.Error("Stats() should return cached result")
	}
}

func TestProfile_Stats_MacroNoPackage(t *testing.T) {
	p := &Profile{
		Calls: []*CallRecord{
			{ID: 1, Name: "myMacro", Package: "", Kind: CallKindMacro,
				Duration: 10 * time.Millisecond},
		},
	}
	stats := p.Stats()
	// Macro without package should just use name as key
	fs, ok := stats.ByFunction["myMacro"]
	if !ok {
		t.Fatal("myMacro not found in stats")
	}
	if fs.Name != "myMacro" {
		t.Errorf("expected name myMacro, got %s", fs.Name)
	}
}
