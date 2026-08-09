// Copyright 2025 The Scriggo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package scriggo

import (
	"reflect"
	"strings"
	"testing"

	"gitlab.com/haproxy-haptic/scriggo/native"
)

// elemOf types a lambda argument from the element type of the slice argument
// to its left — the shape every collection helper wants.
func elemOf(argIndex int, resolved []reflect.Type) []reflect.Type {
	if argIndex != 1 || len(resolved) == 0 || resolved[0] == nil {
		return nil
	}
	if resolved[0].Kind() != reflect.Slice {
		return nil
	}
	return []reflect.Type{resolved[0].Elem()}
}

func lambdaDecls() native.Declarations {
	return native.Declarations{
		"filter": native.AdaptiveFunc{
			Impl: func(slice, pred any) any {
				rv := reflect.ValueOf(slice)
				pv := reflect.ValueOf(pred)
				out := reflect.MakeSlice(rv.Type(), 0, rv.Len())
				for i := range rv.Len() {
					if pv.Call([]reflect.Value{rv.Index(i)})[0].Bool() {
						out = reflect.Append(out, rv.Index(i))
					}
				}
				return out.Interface()
			},
			ReturnType:   func(a []reflect.Type) (reflect.Type, error) { return a[0], nil },
			LambdaParams: elemOf,
		},
		"mapped": native.AdaptiveFunc{
			Impl: func(slice, fn any) any {
				rv := reflect.ValueOf(slice)
				fv := reflect.ValueOf(fn)
				out := reflect.MakeSlice(reflect.SliceOf(fv.Type().Out(0)), 0, rv.Len())
				for i := range rv.Len() {
					out = reflect.Append(out, fv.Call([]reflect.Value{rv.Index(i)})[0])
				}
				return out.Interface()
			},
			ReturnType: func(a []reflect.Type) (reflect.Type, error) {
				return reflect.SliceOf(a[1].Out(0)), nil
			},
			LambdaParams: elemOf,
		},
		"flat_map": native.AdaptiveFunc{
			Impl: func(slice, fn any) any {
				rv := reflect.ValueOf(slice)
				fv := reflect.ValueOf(fn)
				out := reflect.MakeSlice(fv.Type().Out(0), 0, rv.Len())
				for i := range rv.Len() {
					out = reflect.AppendSlice(out, fv.Call([]reflect.Value{rv.Index(i)})[0])
				}
				return out.Interface()
			},
			ReturnType:   func(a []reflect.Type) (reflect.Type, error) { return a[1].Out(0), nil },
			LambdaParams: elemOf,
		},
		"join": strings.Join,
	}
}

type lambdaGroup struct {
	Items []lambdaItem
}

type lambdaItem struct {
	Name  string
	Ready bool
}

// TestLambda covers `x => expr`: the parameter type comes from the call, the
// result type from the returned expression, and neither is written down.
func TestLambda(t *testing.T) {
	tests := []struct {
		name   string
		source string
		want   string
	}{
		{
			name:   "parameter typed from the slice, result inferred",
			source: `{% var xs = []int{1,2,3,4} %}{{ len(filter(xs, v => v > 2)) }}`,
			want:   "2",
		},
		{
			name:   "result type follows the expression, not the input",
			source: `{% var xs = []int{1,2,3} %}{{ join(mapped(xs, v => "x"), ",") }}`,
			want:   "x,x,x",
		},
		{
			name: "typed field access on an inferred struct parameter",
			source: `{% var xs = []Item{Item{Name:"a", Ready:true}, Item{Name:"b"}} %}` +
				`{{ join(mapped(filter(xs, i => i.Ready), i => i.Name), ",") }}`,
			want: "a",
		},
		{
			name:   "lambda in a pipe stage",
			source: `{% var xs = []int{1,2,3,4} %}{{ len(xs | filter(v => v > 1)) }}`,
			want:   "3",
		},
		{
			// A lambda reading an outer variable is a closure, and the trial
			// scope that types its body must not disturb the capture.
			name:   "captures an outer variable",
			source: `{% var xs = []int{1,2,3,4} %}{% var lo = 2 %}{{ len(filter(xs, v => v > lo)) }}`,
			want:   "2",
		},
		{
			name:   "captures an outer variable through a lowered chain",
			source: `{% var xs = []int{1,2,3,4} %}{% var lo = 2 %}{{ len(xs | filter(v => v > lo) | filter(v => v < 4)) }}`,
			want:   "1",
		},
		{
			// Lowering hoists every stage closure into its own variable, so
			// each lambda past the first must take its parameter from the
			// stage before it rather than from the chain's source.
			name: "every stage of a lowered chain",
			source: `{% var gs = []Group{Group{Items: []Item{Item{Name:"a", Ready:true}, Item{Name:"b"}}}} %}` +
				`{{ join(gs | flat_map(g => g.Items) | filter(i => i.Ready) | mapped(i => i.Name), ",") }}`,
			want: "a",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			decls := lambdaDecls()
			decls["Item"] = reflect.TypeOf(lambdaItem{})
			decls["Group"] = reflect.TypeOf(lambdaGroup{})
			tmpl, err := BuildTemplate(Files{"index.html": []byte(tc.source)}, "index.html",
				&BuildOptions{Globals: decls})
			if err != nil {
				t.Fatalf("build: %v", err)
			}
			var out strings.Builder
			if err := tmpl.Run(&out, nil, nil); err != nil {
				t.Fatalf("run: %v", err)
			}
			if got := out.String(); got != tc.want {
				t.Fatalf("output: got %q, want %q", got, tc.want)
			}
		})
	}
}

// TestLambdaWithoutContextIsRejected pins the diagnostic. A lambda has nothing
// to infer from outside a call that supplies parameter types, and the error
// must point at the lambda rather than at something confusing inside its body.
func TestLambdaWithoutContextIsRejected(t *testing.T) {
	_, err := BuildTemplate(Files{"index.html": []byte(`{% var f = v => v + 1 %}{{ f(1) }}`)},
		"index.html", &BuildOptions{Globals: lambdaDecls()})
	if err == nil {
		t.Fatal("expected an error for a lambda with no context")
	}
	if !strings.Contains(err.Error(), "cannot infer") {
		t.Fatalf("diagnostic must say the types cannot be inferred, got: %v", err)
	}
}

// TestArrowDoesNotChangeAssignment pins that adding => left = and >= alone.
func TestArrowDoesNotChangeAssignment(t *testing.T) {
	src := `{% var a = 1 %}{% a = 2 %}{% var b = a >= 2 %}{{ a }}{{ b }}`
	tmpl, err := BuildTemplate(Files{"index.html": []byte(src)}, "index.html", nil)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	var out strings.Builder
	if err := tmpl.Run(&out, nil, nil); err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := out.String(); got != "2true" {
		t.Fatalf("output: got %q, want %q", got, "2true")
	}
}
