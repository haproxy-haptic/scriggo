// Copyright 2026 The Scriggo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package scriggo

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"testing/fstest"
	"time"

	"gitlab.com/haproxy-haptic/scriggo/native"
)

type childContextTestKey struct{}

func childContextTestValue(env native.Env) string {
	value, _ := env.Context().Value(childContextTestKey{}).(string)
	return value
}

func numberedChildContexts() func(context.Context) context.Context {
	var next atomic.Int64
	return func(parent context.Context) context.Context {
		value, _ := parent.Value(childContextTestKey{}).(string)
		return context.WithValue(parent, childContextTestKey{}, fmt.Sprintf("%s/%d", value, next.Add(1)))
	}
}

func TestChildContextNativeCallback(t *testing.T) {
	invoke := func(fn func() string) string { return fn() }
	tmpl, err := BuildTemplate(Files{
		"index.txt": []byte(`{{ invoke(func() string { return prefix + ":" + contextValue() }) }}|{{ invoke(func() string { return prefix + ":" + contextValue() }) }}`),
	}, "index.txt", &BuildOptions{Globals: native.Declarations{
		"contextValue": childContextTestValue,
		"invoke":       invoke,
		"prefix":       (*string)(nil),
	}})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	root := context.WithValue(context.Background(), childContextTestKey{}, "root")
	var out strings.Builder
	err = tmpl.Run(&out, map[string]any{"prefix": "global"}, &RunOptions{
		Context:      root,
		ChildContext: numberedChildContexts(),
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if got, want := out.String(), "global:root/1|global:root/2"; got != want {
		t.Fatalf("output = %q, want %q", got, want)
	}
}

func TestChildContextGoRender(t *testing.T) {
	tmpl, err := BuildTemplate(Files{
		"index.txt": []byte(`{{ go render "child.txt" }}|{{ go render "child.txt" }}`),
		"child.txt": []byte(`{{ prefix }}:{{ contextValue() }}`),
	}, "index.txt", &BuildOptions{
		Globals: native.Declarations{
			"contextValue": childContextTestValue,
			"prefix":       (*string)(nil),
		},
	})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	root := context.WithValue(context.Background(), childContextTestKey{}, "root")
	var out strings.Builder
	err = tmpl.Run(&out, map[string]any{"prefix": "global"}, &RunOptions{
		Context:      root,
		ChildContext: numberedChildContexts(),
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if got, want := out.String(), "global:root/1|global:root/2"; got != want {
		t.Fatalf("output = %q, want %q", got, want)
	}
}

func TestChildContextGoRenderGlob(t *testing.T) {
	tmpl, err := BuildTemplate(fstest.MapFS{
		"index.txt":   {Data: []byte(`{{ go render_glob "parts/*.txt" }}`)},
		"parts/a.txt": {Data: []byte(`A:{{ contextValue() }}`)},
		"parts/b.txt": {Data: []byte(`B:{{ contextValue() }}`)},
	}, "index.txt", &BuildOptions{Globals: native.Declarations{
		"contextValue": childContextTestValue,
	}})
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	root := context.WithValue(context.Background(), childContextTestKey{}, "root")
	var out strings.Builder
	if err := tmpl.Run(&out, nil, &RunOptions{
		Context:      root,
		ChildContext: numberedChildContexts(),
	}); err != nil {
		t.Fatalf("run: %v", err)
	}
	if got, want := out.String(), "A:root/1B:root/2"; got != want {
		t.Fatalf("output = %q, want %q", got, want)
	}
}

func TestChildContextNestedGoRender(t *testing.T) {
	tmpl, err := BuildTemplate(Files{
		"index.txt":  []byte(`{{ go render "parent.txt" }}`),
		"parent.txt": []byte(`[{{ contextValue() }}{{ go render "child.txt" }}]`),
		"child.txt":  []byte(`({{ contextValue() }})`),
	}, "index.txt", &BuildOptions{
		Globals: native.Declarations{
			"contextValue": childContextTestValue,
		},
	})
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	root := context.WithValue(context.Background(), childContextTestKey{}, "root")
	derive := func(parent context.Context) context.Context {
		value, _ := parent.Value(childContextTestKey{}).(string)
		return context.WithValue(parent, childContextTestKey{}, value+"/child")
	}
	var out strings.Builder
	if err := tmpl.Run(&out, nil, &RunOptions{Context: root, ChildContext: derive}); err != nil {
		t.Fatalf("run: %v", err)
	}
	if got, want := out.String(), "[root/child(root/child/child)]"; got != want {
		t.Fatalf("output = %q, want %q", got, want)
	}
}

func TestChildContextGoStatement(t *testing.T) {
	tmpl, err := BuildTemplate(Files{
		"index.txt": []byte(`{% var done = make(chan string, 1) %}{% go func() { done <- prefix + ":" + contextValue() }() %}{% var got = <-done %}{{ got }}`),
	}, "index.txt", &BuildOptions{
		AllowGoStmt: true,
		Globals: native.Declarations{
			"contextValue": childContextTestValue,
			"prefix":       (*string)(nil),
		},
	})
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	root := context.WithValue(context.Background(), childContextTestKey{}, "root")
	var out strings.Builder
	if err := tmpl.Run(&out, map[string]any{"prefix": "global"}, &RunOptions{
		Context:      root,
		ChildContext: numberedChildContexts(),
	}); err != nil {
		t.Fatalf("run: %v", err)
	}
	if got, want := out.String(), "global:root/1"; got != want {
		t.Fatalf("output = %q, want %q", got, want)
	}
}

func TestChildContextGoNativeCallbackLineage(t *testing.T) {
	done := make(chan string, 1)
	doneDeclaration := done
	invoke := func(env native.Env, fn func() string, result chan string) {
		result <- childContextTestValue(env) + "|" + fn()
	}
	tmpl, err := BuildTemplate(Files{
		"index.txt": []byte(`{% go invoke(func() string { return contextValue() }, done) %}{{ <-done }}`),
	}, "index.txt", &BuildOptions{
		AllowGoStmt: true,
		Globals: native.Declarations{
			"contextValue": childContextTestValue,
			"done":         &doneDeclaration,
			"invoke":       invoke,
		},
	})
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	root := context.WithValue(context.Background(), childContextTestKey{}, "root")
	derive := func(parent context.Context) context.Context {
		value, _ := parent.Value(childContextTestKey{}).(string)
		return context.WithValue(parent, childContextTestKey{}, value+"/child")
	}
	var out strings.Builder
	if err := tmpl.Run(&out, nil, &RunOptions{Context: root, ChildContext: derive}); err != nil {
		t.Fatalf("run: %v", err)
	}
	if got, want := out.String(), "root/child|root/child/child"; got != want {
		t.Fatalf("output = %q, want %q", got, want)
	}
}

func TestChildContextCancellationStopsChildBytecode(t *testing.T) {
	started := make(chan struct{})
	signal := func() { close(started) }
	invoke := func(fn func() string) string { return fn() }
	tmpl, err := BuildTemplate(Files{
		"index.txt": []byte(`{{ invoke(func() string { signal(); for {} }) }}`),
	}, "index.txt", &BuildOptions{Globals: native.Declarations{
		"invoke": invoke,
		"signal": signal,
	}})
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	root, cancelRoot := context.WithCancel(context.Background())
	defer cancelRoot()
	childCancel := make(chan context.CancelFunc, 1)
	done := make(chan error, 1)
	go func() {
		done <- tmpl.Run(&strings.Builder{}, nil, &RunOptions{
			Context: root,
			ChildContext: func(parent context.Context) context.Context {
				ctx, cancel := context.WithCancel(parent)
				childCancel <- cancel
				return ctx
			},
		})
	}()

	cancel := <-childCancel
	<-started
	cancel()
	select {
	case err := <-done:
		if err != context.Canceled {
			t.Fatalf("run error = %v, want %v", err, context.Canceled)
		}
		if root.Err() != nil {
			t.Fatalf("root context was canceled: %v", root.Err())
		}
	case <-time.After(2 * time.Second):
		cancelRoot()
		t.Fatal("child bytecode did not stop after child context cancellation")
	}
}

func TestChildContextCancellationStopsParallelChildBytecode(t *testing.T) {
	for _, test := range []struct {
		name        string
		cancelChild bool
	}{
		{name: "child", cancelChild: true},
		{name: "root"},
	} {
		t.Run(test.name, func(t *testing.T) {
			started := make(chan struct{})
			signal := func() { close(started) }
			tmpl, err := BuildTemplate(Files{
				"index.txt": []byte(`{{ go render "child.txt" }}`),
				"child.txt": []byte(`{% signal() %}{% for %}{% end %}`),
			}, "index.txt", &BuildOptions{Globals: native.Declarations{"signal": signal}})
			if err != nil {
				t.Fatalf("build: %v", err)
			}

			root, cancelRoot := context.WithCancel(context.Background())
			defer cancelRoot()
			childCancel := make(chan context.CancelFunc, 1)
			done := make(chan error, 1)
			go func() {
				done <- tmpl.Run(&strings.Builder{}, nil, &RunOptions{
					Context: root,
					ChildContext: func(parent context.Context) context.Context {
						ctx, cancel := context.WithCancel(parent)
						childCancel <- cancel
						return ctx
					},
				})
			}()

			cancel := <-childCancel
			<-started
			if test.cancelChild {
				cancel()
			} else {
				cancelRoot()
			}
			select {
			case err := <-done:
				if err != context.Canceled {
					t.Fatalf("run error = %v, want %v", err, context.Canceled)
				}
				if test.cancelChild && root.Err() != nil {
					t.Fatalf("root context was canceled: %v", root.Err())
				}
			case <-time.After(2 * time.Second):
				cancelRoot()
				t.Fatal("parallel child bytecode did not stop after context cancellation")
			}
		})
	}
}

func TestAlreadyCanceledEmptyTemplate(t *testing.T) {
	tmpl, err := BuildTemplate(Files{"index.txt": nil}, "index.txt", nil)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	for range 1000 {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if err := tmpl.Run(&strings.Builder{}, nil, &RunOptions{Context: ctx}); err != context.Canceled {
			t.Fatalf("run error = %v, want %v", err, context.Canceled)
		}
	}
}

func TestChildContextPreservesCancellation(t *testing.T) {
	started := make(chan struct{})
	wait := func(env native.Env) string {
		close(started)
		<-env.Context().Done()
		return ""
	}
	invoke := func(fn func() string) string { return fn() }
	tmpl, err := BuildTemplate(Files{
		"index.txt": []byte(`{{ invoke(func() string { wait(); for {} }) }}`),
	}, "index.txt", &BuildOptions{Globals: native.Declarations{
		"invoke": invoke,
		"wait":   wait,
	}})
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- tmpl.Run(&strings.Builder{}, nil, &RunOptions{
			Context: ctx,
			ChildContext: func(parent context.Context) context.Context {
				return context.WithValue(parent, childContextTestKey{}, "child")
			},
		})
	}()
	<-started
	cancel()
	select {
	case err := <-done:
		if err != context.Canceled {
			t.Fatalf("run error = %v, want %v", err, context.Canceled)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("run did not stop after context cancellation")
	}
}

func TestNativeEnvFunctionField(t *testing.T) {
	functionType := reflect.TypeFor[func(native.Env, string, map[string]any) string]()
	storeType := reflect.StructOf([]reflect.StructField{{Name: "Fetch", Type: functionType}})
	storeDeclaration := reflect.Zero(reflect.PointerTo(storeType)).Interface()
	store := reflect.New(storeType).Elem()
	store.Field(0).Set(reflect.ValueOf(func(env native.Env, key string, _ map[string]any) string {
		return childContextTestValue(env) + ":" + key
	}))

	tmpl, err := BuildTemplate(Files{
		"index.txt": []byte(`{{ store.Fetch("key", map[string]interface{}{}) }}`),
	}, "index.txt", &BuildOptions{Globals: native.Declarations{"store": storeDeclaration}})
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	ctx := context.WithValue(context.Background(), childContextTestKey{}, "root")
	var out strings.Builder
	if err := tmpl.Run(&out, map[string]any{"store": store.Interface()}, &RunOptions{Context: ctx}); err != nil {
		t.Fatalf("run: %v", err)
	}
	if got, want := out.String(), "root:key"; got != want {
		t.Fatalf("output = %q, want %q", got, want)
	}
}
