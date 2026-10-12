// Copyright 2026 The Scriggo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package scriggo

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"

	"gitlab.com/haproxy-haptic/scriggo/native"
)

func TestTemplateRunIsolatesRetainedValues(t *testing.T) {
	type key struct{}
	var environments []native.Env
	var callbacks []func() string
	retain := func(env native.Env, callback func() string) {
		environments = append(environments, env)
		callbacks = append(callbacks, callback)
	}
	tmpl, err := BuildTemplate(Files{"index.txt": []byte(`{% var local = value %}{% retain(func() string { return local }) %}{{ value }}`)}, "index.txt", &BuildOptions{
		Globals: native.Declarations{
			"value":  (*string)(nil),
			"retain": native.Synchronous(retain),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !tmpl.BatchSafe() {
		t.Fatal("template is not certified for reuse")
	}
	for index := range 30 {
		want := fmt.Sprint(index)
		ctx := context.WithValue(context.Background(), key{}, want)
		var output strings.Builder
		if err := tmpl.Run(&output, map[string]any{"value": want}, &RunOptions{Context: ctx}); err != nil {
			t.Fatal(err)
		}
		if output.String() != want {
			t.Fatalf("output = %q, want %q", output.String(), want)
		}
	}
	for index, callback := range callbacks {
		want := fmt.Sprint(index)
		if got := callback(); got != want {
			t.Errorf("callback %d = %q, want %q", index, got, want)
		}
		if got := environments[index].Context().Value(key{}); got != want {
			t.Errorf("context %d = %v, want %q", index, got, want)
		}
	}
}

func TestTemplateRunRecoversBetweenExecutions(t *testing.T) {
	stop := errors.New("stop requested")
	tmpl, err := BuildTemplate(Files{"index.txt": []byte(`{% if fail %}{{ halt() }}{% end %}{{ value }}`)}, "index.txt", &BuildOptions{
		Globals: native.Declarations{
			"value": (*string)(nil),
			"fail":  (*bool)(nil),
			"halt":  native.Synchronous(func(env native.Env) string { env.Fatal(stop); return "" }),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !tmpl.BatchSafe() {
		t.Fatal("template is not certified for reuse")
	}
	for range 10 {
		func() {
			defer func() {
				if got := recover(); got != stop {
					t.Errorf("panic = %v, want %v", got, stop)
				}
			}()
			_ = tmpl.Run(io.Discard, map[string]any{"fail": true, "value": "failed"}, nil)
		}()
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if err := tmpl.Run(io.Discard, map[string]any{"fail": false, "value": "cancelled"}, &RunOptions{Context: ctx}); !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled run = %v", err)
		}
		var output strings.Builder
		if err := tmpl.Run(&output, map[string]any{"fail": false, "value": "valid"}, nil); err != nil {
			t.Fatal(err)
		}
		if output.String() != "valid" {
			t.Fatalf("output = %q", output.String())
		}
	}
}

func TestTemplateRunConcurrentIsolation(t *testing.T) {
	tmpl, err := BuildTemplate(Files{"index.txt": []byte(`{% var local = value %}{{ local }}`)}, "index.txt", &BuildOptions{
		Globals: native.Declarations{"value": (*string)(nil)},
	})
	if err != nil {
		t.Fatal(err)
	}
	var workers sync.WaitGroup
	for worker := range 16 {
		workers.Go(func() {
			for iteration := range 40 {
				want := fmt.Sprintf("%d/%d", worker, iteration)
				var output strings.Builder
				if err := tmpl.Run(&output, map[string]any{"value": want}, nil); err != nil {
					t.Error(err)
					return
				}
				if output.String() != want {
					t.Errorf("output = %q, want %q", output.String(), want)
					return
				}
			}
		})
	}
	workers.Wait()
}

func BenchmarkTemplateRunRegisters(b *testing.B) {
	tmpl, err := BuildTemplate(Files{"index.txt": []byte(`{{ value }}`)}, "index.txt", &BuildOptions{
		Globals: native.Declarations{"value": (*string)(nil)},
	})
	if err != nil {
		b.Fatal(err)
	}
	vars := map[string]any{"value": "value"}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if err := tmpl.Run(io.Discard, vars, nil); err != nil {
			b.Fatal(err)
		}
	}
}
