// Copyright 2019 The Scriggo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package misc

import (
	"bytes"
	"fmt"
	"testing"
	"time"

	"github.com/haproxy-haptic/scriggo"
	"github.com/haproxy-haptic/scriggo/internal/fstest"
	"github.com/haproxy-haptic/scriggo/native"
)

func profilingGlobals() native.Declarations {
	return native.Declarations{
		"sprint": func(a ...interface{}) string {
			return fmt.Sprint(a...)
		},
		"slowFunc": func() string {
			time.Sleep(5 * time.Millisecond)
			return "done"
		},
	}
}

func TestProfiling_Disabled(t *testing.T) {
	// Build without EnableProfiling - profile should be empty.
	fsys := fstest.Files{"index.html": `{{ sprint("hello") }}`}
	template, err := scriggo.BuildTemplate(fsys, "index.html", &scriggo.BuildOptions{
		Globals: profilingGlobals(),
	})
	if err != nil {
		t.Fatalf("build error: %s", err)
	}

	var profile scriggo.Profile
	var buf bytes.Buffer
	err = template.Run(&buf, nil, &scriggo.RunOptions{Profile: &profile})
	if err != nil {
		t.Fatalf("run error: %s", err)
	}

	// Profile should be empty when profiling not enabled at build time.
	if len(profile.Calls) != 0 {
		t.Errorf("expected no calls when profiling disabled, got %d", len(profile.Calls))
	}
	if profile.TotalDuration != 0 {
		t.Errorf("expected zero duration when profiling disabled, got %v", profile.TotalDuration)
	}
}

func TestProfiling_NilProfileOption(t *testing.T) {
	// Build with profiling enabled but don't pass Profile in RunOptions.
	// Should not panic, profiling data is simply discarded.
	fsys := fstest.Files{"index.html": `{{ sprint("hello") }}`}
	template, err := scriggo.BuildTemplate(fsys, "index.html", &scriggo.BuildOptions{
		EnableProfiling: true,
		Globals:         profilingGlobals(),
	})
	if err != nil {
		t.Fatalf("build error: %s", err)
	}

	var buf bytes.Buffer
	// Should not panic when Profile is nil.
	err = template.Run(&buf, nil, nil)
	if err != nil {
		t.Fatalf("run error: %s", err)
	}
}

func TestProfiling_NativeCall(t *testing.T) {
	fsys := fstest.Files{"index.html": `{{ sprint("hello") }}`}
	template, err := scriggo.BuildTemplate(fsys, "index.html", &scriggo.BuildOptions{
		EnableProfiling: true,
		Globals:         profilingGlobals(),
	})
	if err != nil {
		t.Fatalf("build error: %s", err)
	}

	var profile scriggo.Profile
	var buf bytes.Buffer
	err = template.Run(&buf, nil, &scriggo.RunOptions{Profile: &profile})
	if err != nil {
		t.Fatalf("run error: %s", err)
	}

	// Should have recorded the native call.
	if len(profile.Calls) < 1 {
		t.Fatalf("expected at least 1 call, got %d", len(profile.Calls))
	}

	// Check that we have a native function call.
	hasNativeCall := false
	for _, call := range profile.Calls {
		if call.Kind == scriggo.CallKindNativeFunc {
			hasNativeCall = true
			break
		}
	}
	if !hasNativeCall {
		t.Error("expected at least one native function call")
	}
}

func TestProfiling_MacroCall(t *testing.T) {
	fsys := fstest.Files{"index.html": `{% macro greet %}Hello{% end %}{% show greet() %}`}
	template, err := scriggo.BuildTemplate(fsys, "index.html", &scriggo.BuildOptions{
		EnableProfiling: true,
	})
	if err != nil {
		t.Fatalf("build error: %s", err)
	}

	var profile scriggo.Profile
	var buf bytes.Buffer
	err = template.Run(&buf, nil, &scriggo.RunOptions{Profile: &profile})
	if err != nil {
		t.Fatalf("run error: %s", err)
	}

	// Should have recorded the macro call.
	hasMacroCall := false
	for _, call := range profile.Calls {
		if call.Kind == scriggo.CallKindMacro {
			hasMacroCall = true
			if call.Name != "greet" {
				t.Errorf("expected macro name 'greet', got '%s'", call.Name)
			}
			break
		}
	}
	if !hasMacroCall {
		t.Error("expected at least one macro call")
	}

	// Verify output is correct.
	if buf.String() != "Hello" {
		t.Errorf("expected output 'Hello', got '%s'", buf.String())
	}
}

func TestProfiling_NestedCalls(t *testing.T) {
	// Declare inner first since outer calls inner
	fsys := fstest.Files{"index.html": `{% macro inner %}X{% end %}{% macro outer %}{% show inner() %}{% end %}{% show outer() %}`}
	template, err := scriggo.BuildTemplate(fsys, "index.html", &scriggo.BuildOptions{
		EnableProfiling: true,
	})
	if err != nil {
		t.Fatalf("build error: %s", err)
	}

	var profile scriggo.Profile
	var buf bytes.Buffer
	err = template.Run(&buf, nil, &scriggo.RunOptions{Profile: &profile})
	if err != nil {
		t.Fatalf("run error: %s", err)
	}

	// Should have recorded both macro calls.
	if len(profile.Calls) < 2 {
		t.Fatalf("expected at least 2 calls, got %d", len(profile.Calls))
	}

	// Check call tree structure.
	tree := profile.Tree()
	if len(tree.Children) == 0 {
		t.Fatal("expected at least one root call")
	}

	// Find the outer macro call - it should have inner as a child.
	var outerNode *scriggo.CallNode
	for _, child := range tree.Children {
		if child.Call.Name == "outer" {
			outerNode = child
			break
		}
	}
	if outerNode == nil {
		t.Fatal("outer macro not found in tree")
	}
	if len(outerNode.Children) == 0 {
		t.Error("outer macro should have inner as child")
	}
}

func TestProfiling_Timing(t *testing.T) {
	fsys := fstest.Files{"index.html": `{{ slowFunc() }}`}
	template, err := scriggo.BuildTemplate(fsys, "index.html", &scriggo.BuildOptions{
		EnableProfiling: true,
		Globals:         profilingGlobals(),
	})
	if err != nil {
		t.Fatalf("build error: %s", err)
	}

	var profile scriggo.Profile
	var buf bytes.Buffer
	err = template.Run(&buf, nil, &scriggo.RunOptions{Profile: &profile})
	if err != nil {
		t.Fatalf("run error: %s", err)
	}

	// Verify total duration is at least 5ms (slowFunc sleeps for 5ms).
	if profile.TotalDuration < 5*time.Millisecond {
		t.Errorf("total duration too short: %v (expected >= 5ms)", profile.TotalDuration)
	}

	// Verify the slow function call duration.
	if len(profile.Calls) < 1 {
		t.Fatal("expected at least 1 call")
	}
	for _, call := range profile.Calls {
		if call.Kind == scriggo.CallKindNativeFunc {
			if call.Duration < 5*time.Millisecond {
				t.Errorf("call duration too short: %v (expected >= 5ms)", call.Duration)
			}
			break
		}
	}
}

func TestProfiling_Stats(t *testing.T) {
	fsys := fstest.Files{"index.html": `{{ sprint("a") }}{{ sprint("b") }}{{ sprint("c") }}`}
	template, err := scriggo.BuildTemplate(fsys, "index.html", &scriggo.BuildOptions{
		EnableProfiling: true,
		Globals:         profilingGlobals(),
	})
	if err != nil {
		t.Fatalf("build error: %s", err)
	}

	var profile scriggo.Profile
	var buf bytes.Buffer
	err = template.Run(&buf, nil, &scriggo.RunOptions{Profile: &profile})
	if err != nil {
		t.Fatalf("run error: %s", err)
	}

	// Check stats.
	stats := profile.Stats()
	if stats.TotalCalls < 3 {
		t.Errorf("expected at least 3 total calls, got %d", stats.TotalCalls)
	}
	if stats.NativeCalls < 3 {
		t.Errorf("expected at least 3 native calls, got %d", stats.NativeCalls)
	}
}

func TestProfiling_EmptyTemplate(t *testing.T) {
	fsys := fstest.Files{"index.html": `Hello World`}
	template, err := scriggo.BuildTemplate(fsys, "index.html", &scriggo.BuildOptions{
		EnableProfiling: true,
	})
	if err != nil {
		t.Fatalf("build error: %s", err)
	}

	var profile scriggo.Profile
	var buf bytes.Buffer
	err = template.Run(&buf, nil, &scriggo.RunOptions{Profile: &profile})
	if err != nil {
		t.Fatalf("run error: %s", err)
	}

	// Empty template should have no function calls.
	if len(profile.Calls) != 0 {
		t.Errorf("expected 0 calls for text-only template, got %d", len(profile.Calls))
	}

	// But TotalDuration should be set.
	if profile.TotalDuration == 0 {
		t.Error("expected non-zero TotalDuration")
	}
}
