// Copyright 2025 The Scriggo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package scriggo

import (
	"fmt"
	"strings"
	"testing"
)

// TestGoRenderIndexLimit guards against silent index truncation of the
// parallel "go render" operand. The operand is a single byte, so a function
// referencing more than 256 distinct render targets must fail at build time
// with a LimitExceededError rather than wrap modulo 256 and render the wrong
// target's body (issue #169).
func TestGoRenderIndexLimit(t *testing.T) {
	const overLimit = 300 // > maxScriggoFunctionsCount (256)

	files := Files{}
	var body strings.Builder
	for i := 0; i < overLimit; i++ {
		path := fmt.Sprintf("p%d.txt", i)
		files[path] = []byte(fmt.Sprintf("<%d>", i))
		body.WriteString(fmt.Sprintf(`{{ go render %q }}`, path))
	}
	files["index.txt"] = []byte(body.String())

	_, err := BuildTemplate(files, "index.txt", &BuildOptions{})
	if err == nil {
		t.Fatal("expected a build-time limit error for >256 distinct go-render targets, got nil (silent index truncation)")
	}
	if !strings.Contains(err.Error(), "Scriggo functions count exceeded") {
		t.Fatalf("expected a Scriggo-functions limit error, got: %v", err)
	}
}

// TestGoRenderBelowLimit confirms the guard does not disturb the normal path:
// distinct go-render targets below the limit build and each site renders its
// own target's body.
func TestGoRenderBelowLimit(t *testing.T) {
	const n = 10

	files := Files{}
	var body strings.Builder
	for i := 0; i < n; i++ {
		path := fmt.Sprintf("p%d.txt", i)
		files[path] = []byte(fmt.Sprintf("<%d>", i))
		body.WriteString(fmt.Sprintf(`{{ go render %q }}`, path))
	}
	files["index.txt"] = []byte(body.String())

	tmpl, err := BuildTemplate(files, "index.txt", &BuildOptions{})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	var out strings.Builder
	if err := tmpl.Run(&out, nil, nil); err != nil {
		t.Fatalf("run: %v", err)
	}
	for i := 0; i < n; i++ {
		token := fmt.Sprintf("<%d>", i)
		if got := strings.Count(out.String(), token); got != 1 {
			t.Fatalf("target %s rendered %d times, want 1 (output %q)", token, got, out.String())
		}
	}
}
