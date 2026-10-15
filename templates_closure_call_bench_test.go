// Copyright 2025 The Scriggo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package scriggo

import (
	"io"
	"testing"

	"github.com/haproxy-haptic/scriggo/native"
)

// BenchmarkNativeCallsTemplateClosure measures the cost of a native function
// invoking a template closure once per element.
//
// This path used to build a whole VM per call — four register banks of
// stackSize entries, ~28 KB — while the parallel-render and goroutine paths
// already took theirs from vmPool. Any native helper shaped like map/filter
// therefore paid a full VM allocation per element.
func BenchmarkNativeCallsTemplateClosure(b *testing.B) {
	// applyN calls the closure once per element, the shape of a collection
	// helper.
	applyN := func(n int, fn func(int) int) int {
		total := 0
		for i := range n {
			total += fn(i)
		}
		return total
	}

	for _, n := range []int{1, 100, 1000} {
		b.Run(itoa(n), func(b *testing.B) {
			fsys := Files{"index.html": []byte(
				`{{ applyN(` + itoa(n) + `, func(i int) int { return i * 2 }) }}`)}
			tmpl, err := BuildTemplate(fsys, "index.html", &BuildOptions{
				Globals: native.Declarations{"applyN": applyN},
			})
			if err != nil {
				b.Fatalf("build: %v", err)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				if err := tmpl.Run(io.Discard, nil, nil); err != nil {
					b.Fatalf("run: %v", err)
				}
			}
		})
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
