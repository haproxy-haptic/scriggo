// Copyright 2025 The Scriggo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package scriggo

import (
	"context"
	"strings"
	"sync"
	"testing"

	"gitlab.com/haproxy-haptic/scriggo/native"
)

// TestPooledVMWithCancellableContext pins the interaction that took a
// controller down while every offline suite stayed green.
//
// runFunc starts a watchdog goroutine when the run has a cancellable context
// (env.doneChan != nil). Pooling the VM at the end of a native→closure call
// resets it — vm.env = nil — and the watchdog could still be racing to enter
// its select, where it read vm.env.ctx. Nil dereference, process down.
//
// The distinguishing condition is the Context: templates run without one never
// start the watchdog, so a suite that never passes a cancellable context
// cannot see this no matter how many closures it calls.
func TestPooledVMWithCancellableContext(t *testing.T) {
	// each calls the closure once per element — the shape that borrows a
	// pooled VM per element.
	each := func(n int, fn func(int) int) int {
		total := 0
		for i := range n {
			total += fn(i)
		}
		return total
	}

	fsys := Files{"index.html": []byte(
		`{{ each(40, func(i int) int { var t = 0; for _, v := range []int{i, 1} { t = t + v }; return t }) }}`)}
	tmpl, err := BuildTemplate(fsys, "index.html", &BuildOptions{
		Globals: native.Declarations{"each": each},
	})
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 40 {
				var out strings.Builder
				// A Context is what makes runFunc start the watchdog.
				if err := tmpl.Run(&out, nil, &RunOptions{Context: ctx}); err != nil {
					t.Errorf("run: %v", err)
					return
				}
				if got := out.String(); got != "820" {
					t.Errorf("output: got %q, want %q", got, "820")
					return
				}
			}
		}()
	}
	wg.Wait()
}
