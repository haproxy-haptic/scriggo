// Copyright 2025 The Scriggo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime_test

import (
	"strings"
	"testing"

	"github.com/haproxy-haptic/scriggo"
	"github.com/haproxy-haptic/scriggo/internal/fstest"
)

// buildAndRunExpectingPanic is a test helper that builds and runs a program,
// expecting it to panic with a PanicError.
func buildAndRunExpectingPanic(t *testing.T, src string) *scriggo.PanicError {
	t.Helper()
	fsys := fstest.Files{"main.go": src}
	program, err := scriggo.Build(fsys, nil)
	if err != nil {
		t.Fatal(err)
	}

	err = program.Run(nil)
	panicErr, ok := err.(*scriggo.PanicError)
	if !ok {
		t.Fatalf("expected *PanicError, got %T", err)
	}
	return panicErr
}

func TestTypeAssertionStackFrames(t *testing.T) {
	src := `package main
func assert(v interface{}) int {
	return v.(int)
}
func main() {
	var x interface{} = "string"
	assert(x)
}`

	panicErr := buildAndRunExpectingPanic(t, src)
	frames := panicErr.StackFrames()

	// Verify assert() frame has line number (line 3: return v.(int))
	if len(frames) < 1 {
		t.Fatal("expected at least 1 frame")
	}
	if frames[0].Line == 0 {
		t.Errorf("assert() frame missing line number, got Line=%d", frames[0].Line)
	}
	if frames[0].Line != 3 {
		t.Errorf("expected line 3, got %d", frames[0].Line)
	}
}

func TestTypeAssertionErrorMessage(t *testing.T) {
	src := `package main
func main() {
	var x interface{} = "hello world"
	_ = x.(int)
}`

	panicErr := buildAndRunExpectingPanic(t, src)
	errMsg := panicErr.String()
	// Should contain the actual value
	if !strings.Contains(errMsg, "hello world") {
		t.Errorf("error message should contain actual value, got: %s", errMsg)
	}
	// Should contain value: marker
	if !strings.Contains(errMsg, "(value:") {
		t.Errorf("error message should contain '(value:' marker, got: %s", errMsg)
	}
}

func TestTypeAssertionStackTraceText(t *testing.T) {
	src := `package main
func main() {
	var x interface{} = "string"
	_ = x.(int)
}`

	panicErr := buildAndRunExpectingPanic(t, src)
	stackTrace := string(panicErr.StackTrace())

	// Should NOT contain "???" for line numbers
	if strings.Contains(stackTrace, ":???") {
		t.Errorf("stack trace should not contain '???' markers, got:\n%s", stackTrace)
	}

	// Should contain actual line number (line 4)
	if !strings.Contains(stackTrace, ":4") {
		t.Errorf("stack trace should contain line 4, got:\n%s", stackTrace)
	}
}

func TestTypeAssertionLargeValue(t *testing.T) {
	// Test that large values are truncated to keep error messages readable
	src := `package main
func main() {
	var x interface{} = make([]byte, 1000)
	_ = x.(int)
}`

	panicErr := buildAndRunExpectingPanic(t, src)
	errMsg := panicErr.String()

	// Should contain value marker
	if !strings.Contains(errMsg, "(value:") {
		t.Errorf("error message should contain '(value:' marker, got: %s", errMsg)
	}

	// Error message should not be excessively long
	// Maximum reasonable length: ~500 chars for message + truncated value
	if len(errMsg) > 1000 {
		t.Errorf("error message too long (%d chars), should be truncated. Got: %s", len(errMsg), errMsg)
	}
}
