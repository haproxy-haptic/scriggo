// Copyright 2019 The Scriggo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime

import (
	"reflect"
	"runtime"
	"testing"
)

// A VM returned to the pool must hold nothing from the render it just ran.
//
// The pool is deliberately not GC-evictable, so anything a released VM still
// points at stays reachable for the process lifetime. Truncating the frame
// stacks with [:0] leaves the elements inside the capacity: a rangeState pins
// the collection it was iterating and a callFrame pins that render's renderer.
// Measured on a controller rendering an HAProxy configuration, that was tens of
// megabytes of parsed configuration held per pooled VM.
func TestPutPooledVMReleasesFrames(t *testing.T) {
	vm := NewVM()

	// A render deep enough to grow the stacks, then unwound: len is back to 0
	// while cap still holds the elements. This is the state putPooledVM sees.
	vm.calls = append(vm.calls, callFrame{renderer: &renderer{}}, callFrame{renderer: &renderer{}})
	vm.ranges = append(vm.ranges, rangeState{sliceIface: []interface{}{"held"}})
	vm.cases = append(vm.cases, reflect.SelectCase{})
	vm.calls = vm.calls[:0]
	vm.ranges = vm.ranges[:0]
	vm.cases = vm.cases[:0]

	putPooledVM(vm)

	assertCleared(t, "calls", vm.calls, func(f callFrame) bool { return f.renderer != nil })
	assertCleared(t, "ranges", vm.ranges, func(r rangeState) bool { return r.sliceIface != nil })
	assertCleared(t, "cases", vm.cases, func(c reflect.SelectCase) bool { return c.Chan.IsValid() })
}

// assertCleared reports any element still holding a reference inside the
// slice's capacity — the region a bare truncation leaves untouched.
func assertCleared[T any](t *testing.T, name string, s []T, holds func(T) bool) {
	t.Helper()
	if len(s) != 0 {
		t.Errorf("%s: len is %d, want 0", name, len(s))
	}
	full := s[:cap(s)]
	for i := range full {
		if holds(full[i]) {
			t.Errorf("%s[%d] still holds a reference from the previous render; "+
				"the pool is not GC-evictable, so this keeps that render alive forever", name, i)
		}
	}
}

// The register banks are cleared to their high-water mark, but not every write
// path records one: a bulk copy into a pooled VM's banks bypasses noteHigh, so
// clearing only to the mark leaves live reflect.Values in the tail — among them
// the render's globals.
func TestPutPooledVMClearsRegistersBeyondHighWaterMark(t *testing.T) {
	vm := NewVM()

	const beyond = 40
	if len(vm.regs.general) <= beyond || len(vm.regs.string) <= beyond {
		t.Skipf("register banks too small for this test: general=%d string=%d",
			len(vm.regs.general), len(vm.regs.string))
	}

	// Written without noteHigh, exactly as the bulk-copy paths do.
	vm.regs.general[beyond] = reflect.ValueOf(&struct{ payload [64]byte }{})
	vm.regs.string[beyond] = "held by the previous render"
	vm.high = [4]Addr{0, 0, 0, 0}

	putPooledVM(vm)

	if vm.regs.general[beyond].IsValid() {
		t.Error("general register beyond the high-water mark still holds a value; " +
			"a bulk copy writes there without noteHigh, so the mark cannot bound the clear")
	}
	if vm.regs.string[beyond] != "" {
		t.Error("string register beyond the high-water mark still holds a value")
	}
}

// Reset is what drops a Builder's buffer, and it must happen on the way in to
// the pool. Resetting only on the way out leaves every idle pooled builder
// holding the byte array of the largest output it ever produced.
func TestPutStringBuilderReturnsAnEmptyBuilder(t *testing.T) {
	b := stringBuilderPool.get()
	b.WriteString("rendered output that must not stay reachable")

	putStringBuilder(b)

	if b.Len() != 0 {
		t.Errorf("builder returned to the pool with %d bytes still in it", b.Len())
	}
	runtime.KeepAlive(b)
}
