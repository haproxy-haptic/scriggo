package scriggo

import (
	"fmt"
	"testing"
	"time"

	"github.com/haproxy-haptic/scriggo/native"
)

func TestNativePackedVariadicArguments(t *testing.T) {
	cases := []struct {
		name, args string
		function   func(chan string) any
		want       string
	}{
		{"nil_slice", `[]interface{}(nil)...`, func(result chan string) any {
			return func(args ...any) string { result <- fmt.Sprint(args == nil); return "" }
		}, "true"},
		{"empty_slice", `[]interface{}{}...`, func(result chan string) any {
			return func(args ...any) string { result <- fmt.Sprint(args == nil, len(args)); return "" }
		}, "false 0"},
		{"any", `[]interface{}{"a", 2}...`, func(result chan string) any {
			return func(args ...any) string { result <- fmt.Sprint(args); return "" }
		}, "[a 2]"},
		{"string_any", `"prefix", []interface{}{"a", 2}...`, func(result chan string) any {
			return func(prefix string, args ...any) string { result <- prefix + fmt.Sprint(args); return "" }
		}, "prefix[a 2]"},
		{"any_string", `"object", []string{"a", "b"}...`, func(result chan string) any {
			return func(object any, args ...string) any { result <- fmt.Sprint(object, args); return nil }
		}, "object[a b]"},
		{"any_string_any", `"object", "key", []interface{}{"a", 2}...`, func(result chan string) any {
			return func(object any, key string, args ...any) []any { result <- fmt.Sprint(object, key, args); return nil }
		}, "objectkey[a 2]"},
		{"any_any_error", `"object", []interface{}{"a", 2}...`, func(result chan string) any {
			return func(object any, args ...any) (string, error) { result <- fmt.Sprint(object, args); return "", nil }
		}, "object[a 2]"},
		{"env_any", `[]interface{}{"a", 2}...`, func(result chan string) any {
			return func(_ native.Env, args ...any) bool { result <- fmt.Sprint(args); return true }
		}, "[a 2]"},
		{"env_any_string", `"object", []string{"a", "b"}...`, func(result chan string) any {
			return func(_ native.Env, object any, args ...string) any { result <- fmt.Sprint(object, args); return nil }
		}, "object[a b]"},
		{"env_any_string_string", `"object", "key", []string{"a", "b"}...`, func(result chan string) any {
			return func(_ native.Env, object any, key string, args ...string) string {
				result <- fmt.Sprint(object, key, args)
				return ""
			}
		}, "objectkey[a b]"},
	}
	for _, tc := range cases {
		for _, mode := range []string{"direct", "indirect", "deferred", "goroutine"} {
			t.Run(tc.name+"/"+mode, func(t *testing.T) {
				result := make(chan string, 1)
				call := "probe.Call(" + tc.args + ")"
				switch mode {
				case "indirect":
					call = "f := probe.Call; f(" + tc.args + ")"
				case "deferred":
					call = "defer " + call
				case "goroutine":
					call = "go " + call
				}
				options := &BuildOptions{AllowGoStmt: true, Packages: native.Packages{
					"probe": native.Package{Name: "probe", Declarations: native.Declarations{"Call": tc.function(result)}},
				}}
				source := "package main\nimport \"probe\"\nfunc main() { " + call + " }"
				program, err := Build(Files{"main.go": []byte(source)}, options)
				if err != nil {
					t.Fatal(err)
				}
				if err := program.Run(nil); err != nil {
					t.Fatal(err)
				}
				select {
				case got := <-result:
					if got != tc.want {
						t.Fatalf("got %q, want %q", got, tc.want)
					}
				case <-time.After(5 * time.Second):
					t.Fatal("native call did not finish")
				}
			})
		}
	}
}
