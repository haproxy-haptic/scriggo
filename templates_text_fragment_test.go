// Copyright 2026 The Scriggo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package scriggo

import (
	"errors"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"gitlab.com/haproxy-haptic/scriggo/native"
)

type templateTextFragment struct {
	text   string
	writes atomic.Int64
}

type templateTextFragmentFunc func(io.Writer) (int64, error)

func (f templateTextFragmentFunc) WriteTo(writer io.Writer) (int64, error) {
	return f(writer)
}

type templateFixedWriter struct {
	count int
	err   error
}

func (w templateFixedWriter) Write([]byte) (int, error) {
	return w.count, w.err
}

func (f *templateTextFragment) WriteTo(writer io.Writer) (int64, error) {
	f.writes.Add(1)
	n, err := io.WriteString(writer, f.text)
	return int64(n), err
}

func (*templateTextFragment) String() string { return "stringer output" }

type retainedTemplatePart struct {
	text     string
	fragment native.TextFragment
}

type retainingTemplateWriter struct {
	mu          sync.Mutex
	parts       []retainedTemplatePart
	fragmentErr error
}

func (w *retainingTemplateWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	w.parts = append(w.parts, retainedTemplatePart{text: string(p)})
	w.mu.Unlock()
	return len(p), nil
}

func (w *retainingTemplateWriter) WriteString(s string) (int, error) {
	w.mu.Lock()
	w.parts = append(w.parts, retainedTemplatePart{text: s})
	w.mu.Unlock()
	return len(s), nil
}

func (w *retainingTemplateWriter) WriteTextFragment(fragment native.TextFragment) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.fragmentErr != nil {
		return w.fragmentErr
	}
	w.parts = append(w.parts, retainedTemplatePart{fragment: fragment})
	return nil
}

func (w *retainingTemplateWriter) snapshot() []retainedTemplatePart {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]retainedTemplatePart(nil), w.parts...)
}

func retainedTemplateText(parts []retainedTemplatePart) string {
	var output strings.Builder
	for _, part := range parts {
		if part.fragment == nil {
			output.WriteString(part.text)
			continue
		}
		fragment, ok := part.fragment.(*templateTextFragment)
		if !ok {
			panic("unexpected retained text fragment type")
		}
		output.WriteString(fragment.text)
	}
	return output.String()
}

func retainedTemplateFragments(parts []retainedTemplatePart) []native.TextFragment {
	fragments := make([]native.TextFragment, 0, len(parts))
	for _, part := range parts {
		if part.fragment != nil {
			fragments = append(fragments, part.fragment)
		}
	}
	return fragments
}

func buildTextFragmentTemplate(t *testing.T, files Files, globals native.Declarations) *Template {
	t.Helper()
	tmpl, err := BuildTemplate(files, "index.txt", &BuildOptions{Globals: globals})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	return tmpl
}

func TestTemplateTextFragmentWriterProtocol(t *testing.T) {
	tmpl := buildTextFragmentTemplate(t, Files{
		"index.txt": []byte(`A{{ fragment }}B`),
	}, native.Declarations{"fragment": (*native.TextFragment)(nil)})

	retainedFragment := &templateTextFragment{text: "fragment"}
	var retained retainingTemplateWriter
	if err := tmpl.Run(&retained, map[string]any{"fragment": retainedFragment}, nil); err != nil {
		t.Fatalf("run retaining writer: %v", err)
	}
	parts := retained.snapshot()
	if got, want := retainedTemplateText(parts), "AfragmentB"; got != want {
		t.Fatalf("retained output = %q, want %q", got, want)
	}
	if got := len(retainedTemplateFragments(parts)); got != 1 {
		t.Fatalf("retained fragments = %d, want 1", got)
	}
	if got := retainedFragment.writes.Load(); got != 0 {
		t.Fatalf("retained fragment writes = %d, want 0", got)
	}

	fallbackFragment := &templateTextFragment{text: "fragment"}
	var fallback strings.Builder
	if err := tmpl.Run(&fallback, map[string]any{"fragment": fallbackFragment}, nil); err != nil {
		t.Fatalf("run fallback writer: %v", err)
	}
	if got, want := fallback.String(), "AfragmentB"; got != want {
		t.Fatalf("fallback output = %q, want %q", got, want)
	}
	if got := fallbackFragment.writes.Load(); got != 1 {
		t.Fatalf("fallback fragment writes = %d, want 1", got)
	}
}

func TestTemplateTextFragmentRejectedOutsideText(t *testing.T) {
	tests := []struct {
		name   string
		path   string
		source string
	}{
		{name: "HTML", path: "index.html", source: `<p>{{ fragment }}</p>`},
		{name: "JavaScript", path: "index.html", source: `<script>{{ fragment }}</script>`},
		{name: "JSON", path: "index.html", source: `<script type="application/ld+json">{{ fragment }}</script>`},
		{name: "Markdown", path: "index.md", source: `{{ fragment }}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := BuildTemplate(Files{test.path: []byte(test.source)}, test.path, &BuildOptions{
				Globals: native.Declarations{"fragment": (*native.TextFragment)(nil)},
			})
			if err == nil || !strings.Contains(err.Error(), "cannot show") {
				t.Fatalf("build error = %v, want cannot show", err)
			}
		})
	}
}

func TestTemplateDynamicTextFragmentRejectedOutsideText(t *testing.T) {
	tmpl, err := BuildTemplate(Files{
		"index.html": []byte(`<script>{{ value }}</script>`),
	}, "index.html", &BuildOptions{Globals: native.Declarations{"value": (*any)(nil)}})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	err = tmpl.Run(io.Discard, map[string]any{
		"value": &templateTextFragment{text: "fragment"},
	}, nil)
	if err == nil || !strings.Contains(err.Error(), "cannot show") {
		t.Fatalf("run error = %v, want cannot show", err)
	}
}

func TestTemplateTextFragmentNonTextRootFallsBackToBytes(t *testing.T) {
	tmpl, err := BuildTemplate(Files{
		"index.html": []byte(`A{{ render "child.txt" }}B`),
		"child.txt":  []byte(`{{ fragment }}`),
	}, "index.html", &BuildOptions{
		Globals: native.Declarations{"fragment": (*native.TextFragment)(nil)},
	})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	fragment := &templateTextFragment{text: "fragment"}
	var output retainingTemplateWriter
	if err := tmpl.Run(&output, map[string]any{"fragment": fragment}, nil); err != nil {
		t.Fatalf("run: %v", err)
	}
	parts := output.snapshot()
	if got, want := retainedTemplateText(parts), "AfragmentB"; got != want {
		t.Fatalf("output = %q, want %q", got, want)
	}
	if got := len(retainedTemplateFragments(parts)); got != 0 {
		t.Fatalf("retained fragments = %d, want 0", got)
	}
	if got := fragment.writes.Load(); got != 1 {
		t.Fatalf("fragment writes = %d, want 1", got)
	}
}

func TestTemplateTextFragmentInvalidWriteToFails(t *testing.T) {
	tmpl := buildTextFragmentTemplate(t, Files{
		"index.txt": []byte(`{{ fragment }}`),
	}, native.Declarations{"fragment": (*native.TextFragment)(nil)})
	tests := []struct {
		name     string
		fragment native.TextFragment
		writer   io.Writer
		want     string
	}{
		{
			name: "negative count",
			fragment: templateTextFragmentFunc(func(io.Writer) (int64, error) {
				return -1, nil
			}),
			writer: &strings.Builder{},
			want:   "invalid write count",
		},
		{
			name: "underreported count",
			fragment: templateTextFragmentFunc(func(writer io.Writer) (int64, error) {
				_, err := io.WriteString(writer, "abc")
				return 2, err
			}),
			writer: &strings.Builder{},
			want:   "invalid write count",
		},
		{
			name: "short underlying write",
			fragment: templateTextFragmentFunc(func(writer io.Writer) (int64, error) {
				n, err := io.WriteString(writer, "abc")
				return int64(n), err
			}),
			writer: templateFixedWriter{count: 2},
			want:   io.ErrShortWrite.Error(),
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := tmpl.Run(test.writer, map[string]any{"fragment": test.fragment}, nil)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("run error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestTemplateTextFragmentSequentialRenderPreservesFragments(t *testing.T) {
	tmpl := buildTextFragmentTemplate(t, Files{
		"index.txt":   []byte(`A{{ render "parts/a.txt" }}B{{ render_glob "parts/[bc].txt" }}D`),
		"parts/a.txt": []byte(`{{ a }}`),
		"parts/b.txt": []byte(`{{ b }}`),
		"parts/c.txt": []byte(`{{ c }}`),
	}, native.Declarations{
		"a": (*native.TextFragment)(nil),
		"b": (*native.TextFragment)(nil),
		"c": (*native.TextFragment)(nil),
	})
	fragments := map[string]any{
		"a": &templateTextFragment{text: "a"},
		"b": &templateTextFragment{text: "b"},
		"c": &templateTextFragment{text: "c"},
	}
	var output retainingTemplateWriter
	if err := tmpl.Run(&output, fragments, nil); err != nil {
		t.Fatalf("run: %v", err)
	}
	parts := output.snapshot()
	if got, want := retainedTemplateText(parts), "AaBbcD"; got != want {
		t.Fatalf("output = %q, want %q", got, want)
	}
	if got := len(retainedTemplateFragments(parts)); got != 3 {
		t.Fatalf("retained fragments = %d, want 3", got)
	}
	for name, value := range fragments {
		if got := value.(*templateTextFragment).writes.Load(); got != 0 {
			t.Errorf("fragment %s writes = %d, want 0", name, got)
		}
	}
}

func TestTemplateTextFragmentValueRenderFallsBackToString(t *testing.T) {
	decorate := func(value string) string { return "[" + value + "]" }
	tmpl := buildTextFragmentTemplate(t, Files{
		"index.txt": []byte(`A{{ decorate(render "child.txt") }}B`),
		"child.txt": []byte(`{{ fragment }}`),
	}, native.Declarations{
		"decorate": decorate,
		"fragment": (*native.TextFragment)(nil),
	})
	fragment := &templateTextFragment{text: "child"}
	var output retainingTemplateWriter
	if err := tmpl.Run(&output, map[string]any{"fragment": fragment}, nil); err != nil {
		t.Fatalf("run: %v", err)
	}
	parts := output.snapshot()
	if got, want := retainedTemplateText(parts), "A[child]B"; got != want {
		t.Fatalf("output = %q, want %q", got, want)
	}
	if got := len(retainedTemplateFragments(parts)); got != 0 {
		t.Fatalf("retained fragments = %d, want 0", got)
	}
	if got := fragment.writes.Load(); got != 1 {
		t.Fatalf("fragment writes = %d, want 1", got)
	}
}

func TestTemplateTextFragmentOrderedWithParallelRenders(t *testing.T) {
	tmpl := buildTextFragmentTemplate(t, Files{
		"index.txt": []byte(`A{{ go render "left.txt" }}B{{ fragment }}C{{ go render "right.txt" }}D`),
		"left.txt":  []byte(`L`),
		"right.txt": []byte(`R`),
	}, native.Declarations{"fragment": (*native.TextFragment)(nil)})
	fragment := &templateTextFragment{text: "fragment"}
	var output retainingTemplateWriter
	if err := tmpl.Run(&output, map[string]any{"fragment": fragment}, nil); err != nil {
		t.Fatalf("run: %v", err)
	}
	parts := output.snapshot()
	if got, want := retainedTemplateText(parts), "ALBfragmentCRD"; got != want {
		t.Fatalf("output = %q, want %q", got, want)
	}
	fragments := retainedTemplateFragments(parts)
	if len(fragments) != 1 || fragments[0] != fragment {
		t.Fatalf("retained fragments = %#v, want the parent fragment", fragments)
	}
	if got := fragment.writes.Load(); got != 0 {
		t.Fatalf("fragment writes = %d, want 0", got)
	}
}

func TestTemplateTextFragmentParallelChildFallsBackToString(t *testing.T) {
	tmpl := buildTextFragmentTemplate(t, Files{
		"index.txt": []byte(`A{{ go render "child.txt" }}B`),
		"child.txt": []byte(`{{ fragment }}`),
	}, native.Declarations{"fragment": (*native.TextFragment)(nil)})
	fragment := &templateTextFragment{text: "child"}
	var output retainingTemplateWriter
	if err := tmpl.Run(&output, map[string]any{"fragment": fragment}, nil); err != nil {
		t.Fatalf("run: %v", err)
	}
	parts := output.snapshot()
	if got, want := retainedTemplateText(parts), "AchildB"; got != want {
		t.Fatalf("output = %q, want %q", got, want)
	}
	if got := len(retainedTemplateFragments(parts)); got != 0 {
		t.Fatalf("retained fragments = %d, want 0", got)
	}
	if got := fragment.writes.Load(); got != 1 {
		t.Fatalf("fragment writes = %d, want 1", got)
	}
}

func TestTemplateTextFragmentSourceMapFallsBackToBytes(t *testing.T) {
	tmpl := buildTextFragmentTemplate(t, Files{
		"index.txt": []byte(`A{{ go render "child.txt" }}B{{ fragment }}C`),
		"child.txt": []byte(`child`),
	}, native.Declarations{"fragment": (*native.TextFragment)(nil)})
	fragment := &templateTextFragment{text: "fragment"}
	var output retainingTemplateWriter
	options := &RunOptions{CollectSourceMap: true}
	if err := tmpl.Run(&output, map[string]any{"fragment": fragment}, options); err != nil {
		t.Fatalf("run: %v", err)
	}
	parts := output.snapshot()
	if got, want := retainedTemplateText(parts), "AchildBfragmentC"; got != want {
		t.Fatalf("output = %q, want %q", got, want)
	}
	if got := len(retainedTemplateFragments(parts)); got != 0 {
		t.Fatalf("retained fragments = %d, want 0", got)
	}
	if got := fragment.writes.Load(); got != 1 {
		t.Fatalf("fragment writes = %d, want 1", got)
	}
	var spanBytes int
	for _, span := range options.SourceSpans {
		spanBytes += span.Length
	}
	if want := len("AchildBfragmentC"); spanBytes != want {
		t.Fatalf("source span bytes = %d, want %d", spanBytes, want)
	}
}

func TestTemplateTextFragmentWriterError(t *testing.T) {
	tmpl := buildTextFragmentTemplate(t, Files{
		"index.txt": []byte(`{{ go render "child.txt" }}{{ fragment }}`),
		"child.txt": []byte(`child`),
	}, native.Declarations{"fragment": (*native.TextFragment)(nil)})
	sentinel := errors.New("fragment rejected")
	output := &retainingTemplateWriter{fragmentErr: sentinel}
	err := tmpl.Run(output, map[string]any{
		"fragment": &templateTextFragment{text: "fragment"},
	}, nil)
	if err == nil || !strings.Contains(err.Error(), sentinel.Error()) {
		t.Fatalf("error = %v, want %v", err, sentinel)
	}
}

func TestTemplateRunBatchTextFragments(t *testing.T) {
	tmpl := buildTextFragmentTemplate(t, Files{
		"index.txt": []byte(`A{{ fragment }}B`),
	}, native.Declarations{"fragment": (*native.TextFragment)(nil)})
	firstFragment := &templateTextFragment{text: "first"}
	secondFragment := &templateTextFragment{text: "second"}
	var first, second retainingTemplateWriter
	err := tmpl.RunBatch([]BatchRun{
		{Out: &first, Vars: map[string]any{"fragment": firstFragment}},
		{Out: &second, Vars: map[string]any{"fragment": secondFragment}},
	})
	if err != nil {
		t.Fatalf("run batch: %v", err)
	}
	if got, want := retainedTemplateText(first.snapshot()), "AfirstB"; got != want {
		t.Fatalf("first output = %q, want %q", got, want)
	}
	if got, want := retainedTemplateText(second.snapshot()), "AsecondB"; got != want {
		t.Fatalf("second output = %q, want %q", got, want)
	}
	if got := firstFragment.writes.Load(); got != 0 {
		t.Fatalf("first fragment writes = %d, want 0", got)
	}
	if got := secondFragment.writes.Load(); got != 0 {
		t.Fatalf("second fragment writes = %d, want 0", got)
	}
}

func TestTemplateTextFragmentConcurrentRuns(t *testing.T) {
	tmpl := buildTextFragmentTemplate(t, Files{
		"index.txt": []byte(`A{{ go render "left.txt" }}B{{ fragment }}C{{ go render "right.txt" }}D`),
		"left.txt":  []byte(`L`),
		"right.txt": []byte(`R`),
	}, native.Declarations{"fragment": (*native.TextFragment)(nil)})

	const runs = 32
	errorsCh := make(chan error, runs)
	var wg sync.WaitGroup
	for range runs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			fragment := &templateTextFragment{text: "fragment"}
			var output retainingTemplateWriter
			if err := tmpl.Run(&output, map[string]any{"fragment": fragment}, nil); err != nil {
				errorsCh <- err
				return
			}
			if got, want := retainedTemplateText(output.snapshot()), "ALBfragmentCRD"; got != want {
				errorsCh <- errors.New("unexpected concurrent output: " + got)
				return
			}
			if got := fragment.writes.Load(); got != 0 {
				errorsCh <- errors.New("concurrent fragment was flattened")
			}
		}()
	}
	wg.Wait()
	close(errorsCh)
	for err := range errorsCh {
		t.Error(err)
	}
}
