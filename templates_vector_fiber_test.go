// Copyright 2026 The Scriggo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package scriggo

import (
	"context"
	"errors"
	"io"
	"strconv"
	"strings"
	"testing"

	"github.com/haproxy-haptic/scriggo/native"
)

type vectorFiberTestLifecycle struct {
	contexts   []context.Context
	calls      []string
	beginCheck func() error
	endCheck   func() error
	beginErr   error
	endErr     error
	beginPanic any
	endPanic   any
}

func (l *vectorFiberTestLifecycle) BeginChild(source, child int) (context.Context, error) {
	l.calls = append(l.calls, "begin:"+strconv.Itoa(source)+":"+strconv.Itoa(child))
	if l.beginPanic != nil {
		panic(l.beginPanic)
	}
	if l.beginErr != nil {
		return nil, l.beginErr
	}
	if l.beginCheck != nil {
		if err := l.beginCheck(); err != nil {
			return nil, err
		}
	}
	if child < 0 || child >= len(l.contexts) {
		return nil, ErrVectorInvalidConfiguration
	}
	return l.contexts[child], nil
}

func (l *vectorFiberTestLifecycle) EndChild(source, child int) error {
	l.calls = append(l.calls, "end:"+strconv.Itoa(source)+":"+strconv.Itoa(child))
	if l.endPanic != nil {
		panic(l.endPanic)
	}
	if l.endCheck != nil {
		if err := l.endCheck(); err != nil {
			return err
		}
	}
	return l.endErr
}

func TestTemplateVMNativeVectorFiberSelectsAndRevokesChildContexts(t *testing.T) {
	type contextKey struct{}
	boundary := native.NewVectorBoundary()
	fiber := native.NewVectorFiberBoundary()
	var retained context.Context
	read := func(env native.Env) string {
		value, _ := env.Context().Value(contextKey{}).(string)
		if retained == nil {
			retained = env.Context()
			return value
		}
		if !errors.Is(retained.Err(), ErrVectorGenerationRevoked) {
			env.Stop(errors.New("retained child context stayed live"))
		}
		return value
	}
	template, err := BuildTemplate(Files{"index.txt": []byte(
		`{% boundary.Begin(0) %}{% fiber.BeginChild(0, 0) %}{{ read() }}{% fiber.EndChild(0, 0) %}` +
			`{% fiber.BeginChild(0, 1) %}{{ read() }}{% fiber.EndChild(0, 1) %}{% boundary.End(0) %}`,
	)}, "index.txt", &BuildOptions{Globals: native.Declarations{
		"boundary": native.Synchronous((**native.VectorBoundary)(nil), "Begin", "End"),
		"fiber":    native.Synchronous((**native.VectorFiberBoundary)(nil), "BeginChild", "EndChild"),
		"read":     native.Synchronous(read),
	}})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	outer := &vectorVMNativeLifecycle{abortIndex: -1}
	children := &vectorFiberTestLifecycle{contexts: []context.Context{
		context.WithValue(context.Background(), contextKey{}, "first"),
		context.WithValue(context.Background(), contextKey{}, "second"),
	}}
	var output strings.Builder
	err = template.Run(&output, map[string]any{"boundary": boundary, "fiber": fiber}, &RunOptions{
		Vector: &VectorRunOptions{
			Authority: native.NewVectorAuthority(), Count: 1, Bindings: map[string]any{},
			Contexts: []context.Context{context.Background()}, VMNative: true,
			Boundary: boundary, Lifecycle: outer, FiberBoundary: fiber, FiberLifecycle: children,
		},
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if got, want := output.String(), "firstsecond"; got != want {
		t.Fatalf("output = %q, want %q", got, want)
	}
	if got, want := strings.Join(children.calls, ","), "begin:0:0,end:0:0,begin:0:1,end:0:1"; got != want {
		t.Fatalf("fiber calls = %q, want %q", got, want)
	}
	if got, want := strings.Join(outer.calls, ","), "begin:0,finish:0,commit:0"; got != want {
		t.Fatalf("outer calls = %q, want %q", got, want)
	}
}

func TestTemplateVMNativeVectorFiberDrainsContextsBeforeLifecycleBoundaries(t *testing.T) {
	boundary := native.NewVectorBoundary()
	fiber := native.NewVectorFiberBoundary()
	var rowContext context.Context
	var childContext context.Context
	capture := func(env native.Env) {
		if rowContext == nil {
			rowContext = env.Context()
			return
		}
		childContext = env.Context()
	}
	template, err := BuildTemplate(Files{"index.txt": []byte(
		`{% boundary.Begin(0) %}{% capture() %}{% fiber.BeginChild(0, 0) %}` +
			`{% capture() %}{% fiber.EndChild(0, 0) %}{% boundary.End(0) %}`,
	)}, "index.txt", &BuildOptions{Globals: native.Declarations{
		"boundary": native.Synchronous((**native.VectorBoundary)(nil), "Begin", "End"),
		"fiber":    native.Synchronous((**native.VectorFiberBoundary)(nil), "BeginChild", "EndChild"),
		"capture":  native.Synchronous(capture),
	}})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	children := &vectorFiberTestLifecycle{contexts: []context.Context{context.Background()}}
	children.beginCheck = func() error {
		if rowContext == nil || !errors.Is(rowContext.Err(), ErrVectorGenerationRevoked) {
			return errors.New("row context was not revoked before child begin")
		}
		return nil
	}
	children.endCheck = func() error {
		if childContext == nil || !errors.Is(childContext.Err(), ErrVectorGenerationRevoked) {
			return errors.New("child context was not revoked before child end")
		}
		return nil
	}
	outer := &vectorVMNativeLifecycle{abortIndex: -1}
	err = template.Run(io.Discard, map[string]any{"boundary": boundary, "fiber": fiber}, &RunOptions{
		Vector: &VectorRunOptions{
			Authority: native.NewVectorAuthority(), Count: 1, Bindings: map[string]any{},
			Contexts: []context.Context{context.Background()}, VMNative: true,
			Boundary: boundary, Lifecycle: outer, FiberBoundary: fiber, FiberLifecycle: children,
		},
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
}

func TestTemplateVMNativeVectorFiberRejectsPoisonedBoundariesAndSequence(t *testing.T) {
	tests := []struct {
		name   string
		source string
		value  func(*native.VectorFiberBoundary) *native.VectorFiberBoundary
	}{
		{name: "foreign", source: `{% boundary.Begin(0) %}{% other.BeginChild(0, 0) %}`,
			value: func(*native.VectorFiberBoundary) *native.VectorFiberBoundary { return native.NewVectorFiberBoundary() }},
		{name: "wrong source", source: `{% boundary.Begin(0) %}{% fiber.BeginChild(1, 0) %}`},
		{name: "wrong child end", source: `{% boundary.Begin(0) %}{% fiber.BeginChild(0, 0) %}{% fiber.EndChild(0, 1) %}`},
		{name: "outer end while child active", source: `{% boundary.Begin(0) %}{% fiber.BeginChild(0, 0) %}{% boundary.End(0) %}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			boundary := native.NewVectorBoundary()
			fiber := native.NewVectorFiberBoundary()
			other := fiber
			if test.value != nil {
				other = test.value(fiber)
			}
			template, err := BuildTemplate(Files{"index.txt": []byte(test.source)}, "index.txt", &BuildOptions{
				Globals: native.Declarations{
					"boundary": native.Synchronous((**native.VectorBoundary)(nil), "Begin", "End"),
					"fiber":    native.Synchronous((**native.VectorFiberBoundary)(nil), "BeginChild", "EndChild"),
					"other":    native.Synchronous((**native.VectorFiberBoundary)(nil), "BeginChild", "EndChild"),
				},
			})
			if err != nil {
				t.Fatalf("build: %v", err)
			}
			outer := &vectorVMNativeLifecycle{abortIndex: -2}
			children := &vectorFiberTestLifecycle{contexts: []context.Context{context.Background()}}
			err = template.Run(io.Discard, map[string]any{
				"boundary": boundary, "fiber": fiber, "other": other,
			}, &RunOptions{Vector: &VectorRunOptions{
				Authority: native.NewVectorAuthority(), Count: 1, Bindings: map[string]any{},
				Contexts: []context.Context{context.Background()}, VMNative: true,
				Boundary: boundary, Lifecycle: outer, FiberBoundary: fiber, FiberLifecycle: children,
			}})
			if !errors.Is(err, ErrVectorInvalidConfiguration) {
				t.Fatalf("error = %v, want %v", err, ErrVectorInvalidConfiguration)
			}
			if outer.abortErr == nil {
				t.Fatal("outer lifecycle was not aborted")
			}
		})
	}
}

func TestTemplateVMNativeVectorFiberRevokesOnPanicAndCancellation(t *testing.T) {
	tests := []struct {
		name       string
		body       string
		child      context.Context
		beginPanic any
		endErr     error
		endPanic   any
		want       error
	}{
		{name: "canceled child", body: `{% fiber.BeginChild(0, 0) %}`, child: canceledVectorFiberContext(), want: context.Canceled},
		{name: "body panic", body: `{% fiber.BeginChild(0, 0) %}{% capture() %}{% panicBody() %}`, child: context.Background()},
		{name: "begin panic", body: `{% fiber.BeginChild(0, 0) %}`, child: context.Background(), beginPanic: "fiber panic"},
		{name: "end error", body: `{% fiber.BeginChild(0, 0) %}{% capture() %}{% fiber.EndChild(0, 0) %}`, child: context.Background(), endErr: errVectorFiberTest},
		{name: "end panic", body: `{% fiber.BeginChild(0, 0) %}{% capture() %}{% fiber.EndChild(0, 0) %}`, child: context.Background(), endPanic: "fiber end panic"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			boundary := native.NewVectorBoundary()
			fiber := native.NewVectorFiberBoundary()
			var retained context.Context
			template, err := BuildTemplate(Files{"index.txt": []byte(`{% boundary.Begin(0) %}` + test.body)}, "index.txt", &BuildOptions{
				Globals: native.Declarations{
					"boundary": native.Synchronous((**native.VectorBoundary)(nil), "Begin", "End"),
					"fiber":    native.Synchronous((**native.VectorFiberBoundary)(nil), "BeginChild", "EndChild"),
					"capture": native.Synchronous(func(env native.Env) {
						retained = env.Context()
					}),
					"panicBody": native.Synchronous(func() { panic("body panic") }),
				},
			})
			if err != nil {
				t.Fatalf("build: %v", err)
			}
			outer := &vectorVMNativeLifecycle{abortIndex: -1}
			children := &vectorFiberTestLifecycle{
				contexts: []context.Context{test.child}, beginPanic: test.beginPanic,
				endErr: test.endErr, endPanic: test.endPanic,
			}
			err = template.Run(io.Discard, map[string]any{"boundary": boundary, "fiber": fiber}, &RunOptions{
				Vector: &VectorRunOptions{
					Authority: native.NewVectorAuthority(), Count: 1, Bindings: map[string]any{},
					Contexts: []context.Context{context.Background()}, VMNative: true,
					Boundary: boundary, Lifecycle: outer, FiberBoundary: fiber, FiberLifecycle: children,
				},
			})
			if err == nil {
				t.Fatal("run succeeded")
			}
			if test.want != nil && !errors.Is(err, test.want) {
				t.Fatalf("error = %v, want %v", err, test.want)
			}
			if outer.abortIndex != 0 || outer.abortErr == nil {
				t.Fatalf("abort = (%d, %v), want active row 0", outer.abortIndex, outer.abortErr)
			}
			if retained != nil && !errors.Is(retained.Err(), ErrVectorGenerationRevoked) {
				t.Fatalf("retained child context error = %v, want %v", retained.Err(), ErrVectorGenerationRevoked)
			}
		})
	}
}

func TestTemplateVMNativeVectorFiberStopsAfterActiveChildCancellation(t *testing.T) {
	boundary := native.NewVectorBoundary()
	fiber := native.NewVectorFiberBoundary()
	childContext, cancel := context.WithCancel(context.Background())
	template, err := BuildTemplate(Files{"index.txt": []byte(
		`{% boundary.Begin(0) %}{% fiber.BeginChild(0, 0) %}{% cancel() %}` +
			`unreachable{% fiber.EndChild(0, 0) %}{% boundary.End(0) %}`,
	)}, "index.txt", &BuildOptions{Globals: native.Declarations{
		"boundary": native.Synchronous((**native.VectorBoundary)(nil), "Begin", "End"),
		"fiber":    native.Synchronous((**native.VectorFiberBoundary)(nil), "BeginChild", "EndChild"),
		"cancel":   native.Synchronous(cancel),
	}})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	children := &vectorFiberTestLifecycle{contexts: []context.Context{childContext}}
	outer := &vectorVMNativeLifecycle{abortIndex: -1}
	var output strings.Builder
	err = template.Run(&output, map[string]any{"boundary": boundary, "fiber": fiber}, &RunOptions{
		Vector: &VectorRunOptions{
			Authority: native.NewVectorAuthority(), Count: 1, Bindings: map[string]any{},
			Contexts: []context.Context{context.Background()}, VMNative: true,
			Boundary: boundary, Lifecycle: outer, FiberBoundary: fiber, FiberLifecycle: children,
		},
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want %v", err, context.Canceled)
	}
	if output.Len() != 0 {
		t.Fatalf("output = %q, want empty", output.String())
	}
	if got, want := strings.Join(children.calls, ","), "begin:0:0"; got != want {
		t.Fatalf("fiber calls = %q, want %q", got, want)
	}
	if outer.abortIndex != 0 || outer.abortErr == nil {
		t.Fatalf("abort = (%d, %v), want active row 0", outer.abortIndex, outer.abortErr)
	}
}

var errVectorFiberTest = errors.New("vector fiber test error")

func canceledVectorFiberContext() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}
