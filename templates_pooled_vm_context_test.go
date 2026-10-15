// Copyright 2025 The Scriggo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package scriggo

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/haproxy-haptic/scriggo/native"
)

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
