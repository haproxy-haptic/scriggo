package scriggo

import (
	"errors"
	"fmt"
	"reflect"
	"runtime"
	"runtime/debug"
	"strings"
	"testing"

	"gitlab.com/haproxy-haptic/scriggo/native"
)

func TestNativePanicBoundariesKeepOriginalValues(t *testing.T) {
	cases := []struct{ name, body string }{
		{"native_recovers_callback", `probe.Recover(func() { panic(probe.Token()) })`},
		{"interpreted_recovers_callback", `defer func() { probe.Record(recover()) }(); probe.Call(func() { panic(probe.Token()) })`},
		{"interpreted_recovers_native", `defer func() { probe.Record(recover()) }(); probe.Panic()`},
		{"interpreted_recovers_deferred_native", `defer func() { probe.Record(recover()) }(); defer probe.Panic()`},
		{"interpreted_recovers_deferred_builtin", `defer func() { probe.Record(recover()) }(); defer panic(probe.Token())`},
		{"native_recovers_deferred_builtin", `probe.Recover(func() { defer panic(probe.Token()) })`},
		{"pending_native_defer", `defer func() { probe.Record(recover()) }(); defer probe.Noop(); probe.Panic()`},
		{"multiple_native_defers", `defer func() { probe.Record(recover()) }(); defer probe.Noop(); defer probe.Panic(); defer probe.Noop(); probe.Panic()`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			defer func() {
				if value := recover(); value != nil {
					t.Fatalf("unexpected host panic: %v\n%s", value, debug.Stack())
				}
			}()
			token := errors.New("panic token")
			var values []any
			noops := 0
			record := func(value any) { values = append(values, value) }
			options := &BuildOptions{Packages: native.Packages{
				"probe": native.Package{Name: "probe", Declarations: native.Declarations{
					"Token":  func() any { return token },
					"Record": record,
					"Call":   func(callback func()) { callback() },
					"Panic":  func() { panic(token) },
					"Noop":   func() { noops++ },
					"Recover": func(callback func()) {
						defer func() { record(recover()) }()
						callback()
					},
				}},
			}}
			source := "package main\nimport \"probe\"\nfunc main() { " + tc.body + " }"
			program, err := Build(Files{"main.go": []byte(source)}, options)
			if err != nil {
				t.Fatal(err)
			}
			if err := program.Run(nil); err != nil {
				t.Fatal(err)
			}
			if len(values) != 1 || values[0] != token {
				t.Fatalf("recovered %#v, want original token %p", values, token)
			}
			wantNoops := strings.Count(tc.body, "probe.Noop()")
			if noops != wantNoops {
				t.Fatalf("native defers ran %d times, want %d", noops, wantNoops)
			}
		})
	}
}

func TestNativeRuntimePanicsRunDeferredRecovery(t *testing.T) {
	for _, call := range []string{"probe.Crash()", "probe.Call(func() { probe.Crash() })", "probe.Call(func() { n := 0; _ = 1 / n })"} {
		t.Run(call, func(t *testing.T) {
			var values []any
			token := errors.New("replacement panic")
			options := &BuildOptions{Packages: native.Packages{
				"probe": native.Package{Name: "probe", Declarations: native.Declarations{
					"Record": func(value any) { values = append(values, value) },
					"Token":  func() any { return token },
					"Call":   func(callback func()) { callback() },
					"Crash":  func() { zero := 0; _ = 1 / zero },
				}},
			}}
			source := `package main
import "probe"
func main() {
    defer func() { probe.Record(recover()) }()
    defer func() { probe.Record(recover()); panic(probe.Token()) }()
    ` + call + `
    probe.Record("unreachable")
}`
			program, err := Build(Files{"main.go": []byte(source)}, options)
			if err != nil {
				t.Fatal(err)
			}
			if err := program.Run(nil); err != nil {
				t.Fatal(err)
			}
			if len(values) != 2 || values[1] != token {
				t.Fatalf("recovered %#v, want runtime error then replacement token", values)
			}
			if err, ok := values[0].(runtime.Error); !ok || err.Error() != "runtime error: integer divide by zero" {
				t.Fatalf("first panic = %T %v, want divide-by-zero runtime.Error", values[0], values[0])
			}
		})
	}
}

func TestNativeDefersPreserveNamedResults(t *testing.T) {
	options := &BuildOptions{Packages: native.Packages{
		"probe": native.Package{Name: "probe", Declarations: native.Declarations{
			"Increment": func(n *int) { *n++ },
			"Noop":      func() {},
			"Panic":     func() { panic("token") },
		}},
	}}
	source := `package main
import "probe"
func normal() (n int) { defer probe.Increment(&n); return 4 }
func recovered() (n int) {
    local := 7
    defer func() { if recover() != "token" { panic("lost panic") }; n = local }()
    defer probe.Noop()
    probe.Panic()
    return 9
}
func main() {
    if normal() != 5 { panic("lost named result write") }
    if recovered() != 7 { panic("lost recovery result") }
}`
	program, err := Build(Files{"main.go": []byte(source)}, options)
	if err != nil {
		t.Fatal(err)
	}
	if err := program.Run(nil); err != nil {
		t.Fatal(err)
	}
}

func TestNativeCallbackRecoveryPreservesNamedResults(t *testing.T) {
	for _, tc := range []struct {
		name     string
		function any
	}{
		{"typed", func(items []int, compare func(int, int) int) ([]int, error) {
			compare(items[0], items[1])
			return items, nil
		}},
		{"boxed", func(_ native.Env, items, compare any) (any, error) {
			values := reflect.ValueOf(items)
			reflect.ValueOf(compare).Call([]reflect.Value{values.Index(0), values.Index(1)})
			return items, nil
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			options := &BuildOptions{Packages: native.Packages{
				"probe": native.Package{Name: "probe", Declarations: native.Declarations{"Call": tc.function}},
			}}
			source := `package main
import "probe"
func f() (value string) {
    defer func() { value = recover().(string) }()
    probe.Call([]int{2, 1}, func(a, b int) int { panic("callback token") })
    return "unreachable"
}
func main() { if f() != "callback token" { panic("lost recovery result") } }
`
			program, err := Build(Files{"main.go": []byte(source)}, options)
			if err != nil {
				t.Fatal(err)
			}
			if err := program.Run(nil); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestTemplateAdaptiveCallbackPanicRecovery(t *testing.T) {
	options := &BuildOptions{Globals: native.Declarations{
		"sort_by": native.AdaptiveFunc{
			Impl: func(items, compare any) (any, error) {
				values := reflect.ValueOf(items)
				reflect.ValueOf(compare).Call([]reflect.Value{values.Index(0), values.Index(1)})
				return items, nil
			},
			ReturnType: func(args []reflect.Type) (reflect.Type, error) { return args[0], nil },
		},
		"tostring": func(value any) string { return fmt.Sprint(value) },
	}}
	source := `{%% f := func() (value string) {
    defer func() { value = tostring(recover()) }()
    sort_by([]int{2, 1}, func(a, b int) int { panic("callback token") })
    return "unreachable"
} %%}{{ f() }}`
	tmpl, err := BuildTemplate(Files{"index.txt": []byte(source)}, "index.txt", options)
	if err != nil {
		t.Fatal(err)
	}
	var output strings.Builder
	if err := tmpl.Run(&output, nil, nil); err != nil {
		t.Fatal(err)
	}
	if got := output.String(); got != "callback token" {
		t.Fatalf("output = %q, want callback token", got)
	}
}
