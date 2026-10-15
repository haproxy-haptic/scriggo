// Copyright 2025 The Scriggo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package scriggo_test

import (
	"io"
	"strings"
	"testing"

	"github.com/haproxy-haptic/scriggo"
	"github.com/haproxy-haptic/scriggo/native"
)

// TestEnvCallLine: a native function taking native.Env sees the 1-based line of
// its own call site via env.CallLine(), including through the reflect call path
// (a map argument forces it) — the seam the playground uses to jump a rendered
// status block to the exact statusPatch() call.
func TestEnvCallLine(t *testing.T) {
	var gotPath string
	var gotLine int
	probe := func(env native.Env, _ string, _ map[string]any) string {
		gotPath = env.CallPath()
		gotLine = env.CallLine()
		return ""
	}
	tmpl, err := scriggo.BuildTemplate(scriggo.Files{
		// probe() is called on line 3.
		"index.txt": []byte("L1\nL2\n{{ probe(\"x\", map[string]interface{}{}) }}\n"),
	}, "index.txt", &scriggo.BuildOptions{
		AllowGoStmt: true,
		Globals:     native.Declarations{"probe": probe},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := tmpl.Run(io.Discard, nil, nil); err != nil {
		t.Fatal(err)
	}
	if gotPath != "index.txt" {
		t.Errorf("CallPath = %q, want %q", gotPath, "index.txt")
	}
	if gotLine != 3 {
		t.Errorf("CallLine = %d, want 3 (the probe() call line)", gotLine)
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

// lineFrames walks the spans and returns, per output line, the include-stack
// frames of the first span that begins it, as "path:line" strings innermost
// first. Mirrors what the playground does to build breadcrumbs.
func lineFrames(out string, spans []scriggo.SourceSpan) [][]string {
	var lines [][]string
	var cur []string
	have := false
	pos := 0
	for _, sp := range spans {
		end := pos + sp.Length
		if end > len(out) {
			end = len(out)
		}
		chunk := out[pos:end]
		pos = end
		sub := 0
		for i := 0; i < len(chunk); i++ {
			if !have {
				cur = framesToStrings(sp.Frames, sp.IsText, sub)
				have = true
			}
			if chunk[i] == '\n' {
				lines = append(lines, cur)
				have = false
				if sp.IsText {
					sub++
				}
			}
		}
	}
	if have {
		lines = append(lines, cur)
	}
	return lines
}

func framesToStrings(frames []scriggo.SourceFrame, isText bool, sub int) []string {
	out := make([]string, len(frames))
	for i, f := range frames {
		ln := f.Line
		if isText && i == 0 {
			ln += sub
		}
		out[i] = f.Path + ":" + itoa(ln)
	}
	return out
}

func render(t *testing.T, fsys scriggo.Files, entry string) (string, []scriggo.SourceSpan) {
	t.Helper()
	tmpl, err := scriggo.BuildTemplate(fsys, entry, &scriggo.BuildOptions{AllowGoStmt: true})
	if err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	opts := &scriggo.RunOptions{CollectSourceMap: true}
	if err := tmpl.Run(&out, nil, opts); err != nil {
		t.Fatal(err)
	}
	// Self-check: span lengths reconstruct the output exactly.
	total := 0
	for _, s := range opts.SourceSpans {
		total += s.Length
	}
	if total != out.Len() {
		t.Fatalf("span length total = %d, want output length %d", total, out.Len())
	}
	return out.String(), opts.SourceSpans
}

// TestSourceMapBareRender: a bare {{ render "x" }} attributes to x's own source,
// with the caller as the outer breadcrumb frame.
func TestSourceMapBareRender(t *testing.T) {
	got, spans := render(t, scriggo.Files{
		"index.txt": []byte("A\n{{ render \"snip\" }}B\n"),
		"snip":      []byte("S1\nS2\n"),
	}, "index.txt")
	if want := "A\nS1\nS2\nB\n"; got != want {
		t.Fatalf("output = %q, want %q", got, want)
	}
	lines := lineFrames(got, spans)
	// innermost (Frames[0]) per output line
	wantInner := []string{"index.txt:1", "snip:1", "snip:2", "index.txt:2"}
	for i, w := range wantInner {
		if i >= len(lines) || len(lines[i]) == 0 || lines[i][0] != w {
			t.Errorf("line %d innermost = %v, want %q", i+1, at(lines, i), w)
		}
	}
	// S1 breadcrumb chain: snip -> index.txt (the render call line).
	if len(lines) > 1 && (len(lines[1]) < 2 || lines[1][1] != "index.txt:2") {
		t.Errorf("S1 chain = %v, want [snip:1 index.txt:2]", at(lines, 1))
	}
}

// TestSourceMapPipedRender: {{ render "x" }} used as a VALUE (assigned then
// shown, the same swapped-buffer path a "| indent" pipe takes) still resolves to
// x's own source per line, not the show line.
func TestSourceMapPipedRender(t *testing.T) {
	got, spans := render(t, scriggo.Files{
		"index.txt": []byte("A\n{% var s = render \"snip\" %}{{ s }}B\n"),
		"snip":      []byte("S1\nS2\n"),
	}, "index.txt")
	if want := "A\nS1\nS2\nB\n"; got != want {
		t.Fatalf("output = %q, want %q", got, want)
	}
	lines := lineFrames(got, spans)
	wantInner := []string{"index.txt:1", "snip:1", "snip:2", "index.txt:2"}
	for i, w := range wantInner {
		if i >= len(lines) || len(lines[i]) == 0 || lines[i][0] != w {
			t.Errorf("line %d innermost = %v, want %q (value-render must resolve into the snippet)", i+1, at(lines, i), w)
		}
	}
}

// TestSourceMapConcatValueRenders: several value-renders concatenated into one
// shown value (the shape a "render_glob \"x-*\" | filter" takes) each resolve to
// their own snippet's source, in order — the multi-source consume path.
func TestSourceMapConcatValueRenders(t *testing.T) {
	got, spans := render(t, scriggo.Files{
		"index.txt": []byte("{% var s = render \"a\" + render \"b\" %}{{ s }}"),
		"a":         []byte("A1\nA2\n"),
		"b":         []byte("B1\nB2\n"),
	}, "index.txt")
	if want := "A1\nA2\nB1\nB2\n"; got != want {
		t.Fatalf("output = %q, want %q", got, want)
	}
	lines := lineFrames(got, spans)
	wantInner := []string{"a:1", "a:2", "b:1", "b:2"}
	for i, w := range wantInner {
		if i >= len(lines) || len(lines[i]) == 0 || lines[i][0] != w {
			t.Errorf("line %d innermost = %v, want %q", i+1, at(lines, i), w)
		}
	}
}

// TestSourceMapCommentGap: text following a multi-line {# #} comment must
// attribute to its real source line, not to a line offset by coalescing across
// the comment. emitText only coalesces source-contiguous text runs, so the gap
// the comment introduces is preserved.
func TestSourceMapCommentGap(t *testing.T) {
	got, spans := render(t, scriggo.Files{
		// A at line 1; a 3-line comment spans lines 2-4; B at line 5, C at line 6.
		"index.txt": []byte("A\n{# line1\nline2\nline3 #}\nB\nC\n"),
	}, "index.txt")
	if want := "A\nB\nC\n"; got != want {
		t.Fatalf("output = %q, want %q", got, want)
	}
	lines := lineFrames(got, spans)
	wantInner := []string{"index.txt:1", "index.txt:5", "index.txt:6"}
	for i, w := range wantInner {
		if i >= len(lines) || len(lines[i]) == 0 || lines[i][0] != w {
			t.Errorf("line %d innermost = %v, want %q (comment gap must not shift attribution)", i+1, at(lines, i), w)
		}
	}
}

// TestSourceMapOrphanSuffix: a value-render whose result is never filtered leaves
// an orphaned source on the stack; a later "render a + render b | show" must still
// resolve into a and b by matching only the top suffix, ignoring the orphan below.
func TestSourceMapOrphanSuffix(t *testing.T) {
	got, spans := render(t, scriggo.Files{
		"index.txt": []byte("{% var junk = render \"j\" %}{% var s = render \"a\" + render \"b\" %}{{ s }}"),
		"j":         []byte("JUNK\n"),
		"a":         []byte("A1\nA2\n"),
		"b":         []byte("B1\nB2\n"),
	}, "index.txt")
	if want := "A1\nA2\nB1\nB2\n"; got != want {
		t.Fatalf("output = %q, want %q", got, want)
	}
	lines := lineFrames(got, spans)
	wantInner := []string{"a:1", "a:2", "b:1", "b:2"}
	for i, w := range wantInner {
		if i >= len(lines) || len(lines[i]) == 0 || lines[i][0] != w {
			t.Errorf("line %d innermost = %v, want %q (orphan source must not pollute the match)", i+1, at(lines, i), w)
		}
	}
}

func at(s [][]string, i int) []string {
	if i < len(s) {
		return s[i]
	}
	return nil
}
