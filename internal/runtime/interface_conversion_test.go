package runtime_test

import (
	"strings"
	"testing"

	"gitlab.com/haproxy-haptic/scriggo"
)

func TestImplicitInterfaceConversion(t *testing.T) {
	tests := []struct {
		name     string
		template string
		wantOut  string
		wantErr  bool
		errMsg   string
	}{
		{
			name: "string conversion from map[string]interface{}",
			template: `{% var data = map[string]interface{}{"name": "Alice"} %}
{% var name string = data["name"] %}
{{ name }}`,
			wantOut: "Alice",
			wantErr: false,
		},
		{
			name: "int conversion from map[string]interface{}",
			template: `{% var data = map[string]interface{}{"age": 30} %}
{% var age int = data["age"] %}
{{ age }}`,
			wantOut: "30",
			wantErr: false,
		},
		{
			name: "multiple conversions",
			template: `{% var data = map[string]interface{}{"name": "Bob", "age": 25} %}
{% var name string = data["name"] %}
{% var age int = data["age"] %}
{{ name }}-{{ age }}`,
			wantOut: "Bob-25",
			wantErr: false,
		},
		{
			name: "float64 conversion",
			template: `{% var data = map[string]interface{}{"price": 19.99} %}
{% var price float64 = data["price"] %}
{{ price }}`,
			wantOut: "19.99",
			wantErr: false,
		},
		{
			name: "type mismatch - string to int",
			template: `{% var data = map[string]interface{}{"name": "Alice"} %}
{% var age int = data["name"] %}`,
			wantOut: "",
			wantErr: true,
			errMsg:  "is string",
		},
		{
			name: "type mismatch - float to int (should truncate)",
			template: `{% var data = map[string]interface{}{"pi": 3.14} %}
{% var n int = data["pi"] %}
{{ n }}`,
			wantOut: "3",
			wantErr: false,
		},
		{
			name: "nil interface to string",
			template: `{% var i interface{} %}
{% var s string = i %}
{{ s }}`,
			wantOut: "",
			wantErr: false,
		},
		{
			name: "nil interface to int",
			template: `{% var i interface{} %}
{% var n int = i %}
{{ n }}`,
			wantOut: "0",
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fsys := scriggo.Files{"test.html": []byte(tt.template)}
			opts := &scriggo.BuildOptions{}

			tmpl, err := scriggo.BuildTemplate(fsys, "test.html", opts)
			if err != nil {
				t.Fatalf("Build error: %v", err)
			}

			var out strings.Builder
			err = tmpl.Run(&out, nil, nil)

			if tt.wantErr {
				if err == nil {
					t.Fatalf("Expected error containing %q, got nil", tt.errMsg)
				}
				if !strings.Contains(err.Error(), tt.errMsg) {
					t.Fatalf("Expected error containing %q, got: %v", tt.errMsg, err)
				}
				return
			}

			if err != nil {
				t.Fatalf("Unexpected error: %v", err)
			}

			got := strings.TrimSpace(out.String())
			want := strings.TrimSpace(tt.wantOut)
			if got != want {
				t.Errorf("Output mismatch:\ngot:  %q\nwant: %q", got, want)
			}
		})
	}
}

func TestImplicitConversionWithExpressions(t *testing.T) {
	// Test conversion in various expression contexts
	template := `{% var data = map[string]interface{}{"x": 10, "y": 20} %}
{% var sum int = data["x"].(int) + data["y"].(int) %}
{{ sum }}`

	fsys := scriggo.Files{"test.html": []byte(template)}
	opts := &scriggo.BuildOptions{}

	tmpl, err := scriggo.BuildTemplate(fsys, "test.html", opts)
	if err != nil {
		t.Fatalf("Build error: %v", err)
	}

	var out strings.Builder
	err = tmpl.Run(&out, nil, nil)
	if err != nil {
		t.Fatalf("Runtime error: %v", err)
	}

	got := strings.TrimSpace(out.String())
	if got != "30" {
		t.Errorf("Expected 30, got %s", got)
	}
}

func TestImplicitConversionErrorMessage(t *testing.T) {
	// Test that error messages are clear and helpful
	template := `{% var data = map[string]interface{}{"name": "Alice"} %}
{% var age int = data["name"] %}
{{ age }}`

	fsys := scriggo.Files{"test.html": []byte(template)}
	opts := &scriggo.BuildOptions{}

	tmpl, err := scriggo.BuildTemplate(fsys, "test.html", opts)
	if err != nil {
		t.Fatalf("Build error: %v", err)
	}

	var out strings.Builder
	err = tmpl.Run(&out, nil, nil)
	if err == nil {
		t.Fatal("Expected error, got nil")
	}

	errStr := err.Error()

	// Check that error message contains expected information
	expectedParts := []string{
		"string",
		"Alice",
		"int",
	}

	for _, part := range expectedParts {
		if !strings.Contains(errStr, part) {
			t.Errorf("Error message missing %q\nGot: %s", part, errStr)
		}
	}
}

// TestVariableTypePreservationAfterBinaryOp verifies that using an interface{} variable
// in a binary operation does not corrupt the type information for other uses of related
// variables. This is a regression test for a bug where binary operator type conversion
// modified shared typeInfo, causing subsequent map operations to fail with
// "reflect: Key of non-map type".
func TestVariableTypePreservationAfterBinaryOp(t *testing.T) {
	tests := []struct {
		name     string
		template string
		wantOut  string
	}{
		{
			name: "map iteration after binary op with interface value",
			template: `{% var d = map[string]any{"x": 5, "y": 10} %}{% var v = d["x"] %}{{ v + 1 }}{% for k, _ := range d %}{{ k }}{% end %}`,
			wantOut: "6xy", // or "6yx" depending on map order
		},
		{
			name: "multiple binary ops with interface values then iteration",
			template: `{% var m = map[string]any{"a": "hello", "b": "world"} %}{% var a = m["a"] %}{% var b = m["b"] %}{{ a + " " + b }}{% for k := range m %}{{ k }}{% end %}`,
			wantOut: "hello worldab", // or "hello worldba"
		},
		{
			name: "string concat then map index",
			template: `{% var data = map[string]any{"prefix": "Hello"} %}{% var p = data["prefix"] %}{{ p + " World" }}-{{ data["prefix"] }}`,
			wantOut: "Hello World-Hello",
		},
		{
			name: "arithmetic then map keys",
			template: `{% var nums = map[string]any{"n": 42} %}{% var n = nums["n"] %}{{ n * 2 }}{% for k := range nums %}:{{ k }}{% end %}`,
			wantOut: "84:n",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fsys := scriggo.Files{"test.html": []byte(tt.template)}
			opts := &scriggo.BuildOptions{}

			tmpl, err := scriggo.BuildTemplate(fsys, "test.html", opts)
			if err != nil {
				t.Fatalf("Build error: %v", err)
			}

			var out strings.Builder
			err = tmpl.Run(&out, nil, nil)
			if err != nil {
				t.Fatalf("Unexpected runtime error: %v", err)
			}

			got := out.String()
			// For maps, order is not guaranteed, so we check for both possible orders
			if !matchesAnyOrder(got, tt.wantOut) {
				t.Errorf("Output mismatch:\ngot:  %q\nwant: %q (or reordered)", got, tt.wantOut)
			}
		})
	}
}

// TestStringConcatWithConcreteTypes tests that string concatenation with concrete
// non-string types (int, float64, bool) works using fmt.Sprint-style conversion.
// This is a template-friendly extension: "count: " + 42 → "count: 42"
func TestStringConcatWithConcreteTypes(t *testing.T) {
	tests := []struct {
		name     string
		template string
		wantOut  string
	}{
		{
			name:     "string + int literal",
			template: `{{ "count: " + 42 }}`,
			wantOut:  "count: 42",
		},
		{
			name:     "int literal + string",
			template: `{{ 100 + " items" }}`,
			wantOut:  "100 items",
		},
		{
			name:     "string + float64 literal",
			template: `{{ "pi: " + 3.14 }}`,
			wantOut:  "pi: 3.14",
		},
		{
			name:     "string + bool true",
			template: `{{ "flag: " + true }}`,
			wantOut:  "flag: true",
		},
		{
			name:     "string + bool false",
			template: `{{ "flag: " + false }}`,
			wantOut:  "flag: false",
		},
		{
			name:     "string + negative int",
			template: `{{ "neg: " + (-5) }}`,
			wantOut:  "neg: -5",
		},
		{
			name:     "string + int variable",
			template: `{% var n = 42 %}{{ "n=" + n }}`,
			wantOut:  "n=42",
		},
		{
			name:     "int variable + string",
			template: `{% var n = 42 %}{{ n + " is the answer" }}`,
			wantOut:  "42 is the answer",
		},
		{
			name:     "string + float64 variable",
			template: `{% var f = 3.14 %}{{ "value: " + f }}`,
			wantOut:  "value: 3.14",
		},
		{
			name:     "chained string concat with mixed types",
			template: `{{ "a" + 1 + "b" + 2 }}`,
			wantOut:  "a1b2",
		},
		{
			name:     "whole number float64 formatting",
			template: `{{ "port: " + 8080.0 }}`,
			wantOut:  "port: 8080",
		},
		// Chained concatenation with int-first (regression tests for panic fix)
		{
			name:     "chained int-first: 1 + a + 2",
			template: `{{ 1 + "a" + 2 }}`,
			wantOut:  "1a2",
		},
		{
			name:     "chained int-first: 1 + a + 2 + b",
			template: `{{ 1 + "a" + 2 + "b" }}`,
			wantOut:  "1a2b",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fsys := scriggo.Files{"test.html": []byte(tt.template)}
			opts := &scriggo.BuildOptions{}

			tmpl, err := scriggo.BuildTemplate(fsys, "test.html", opts)
			if err != nil {
				t.Fatalf("Build error: %v", err)
			}

			var out strings.Builder
			err = tmpl.Run(&out, nil, nil)
			if err != nil {
				t.Fatalf("Unexpected runtime error: %v", err)
			}

			got := out.String()
			if got != tt.wantOut {
				t.Errorf("Output mismatch:\ngot:  %q\nwant: %q", got, tt.wantOut)
			}
		})
	}
}

// TestStringConcatWithNonStringInterface tests that string concatenation with interface{}
// containing non-string types works using fmt.Sprint-style conversion.
// This is the expected behavior for template string concatenation.
func TestStringConcatWithNonStringInterface(t *testing.T) {
	tests := []struct {
		name     string
		template string
		wantOut  string
	}{
		{
			name:     "float64 in interface to string concat",
			template: `{% var data = map[string]any{"num": 100.5} %}{{ "value: " + data["num"] }}`,
			wantOut:  "value: 100.5",
		},
		{
			name:     "int in interface to string concat",
			template: `{% var data = map[string]any{"num": 42} %}{{ "value: " + data["num"] }}`,
			wantOut:  "value: 42",
		},
		{
			name:     "bool true in interface to string concat",
			template: `{% var data = map[string]any{"flag": true} %}{{ "flag: " + data["flag"] }}`,
			wantOut:  "flag: true",
		},
		{
			name:     "bool false in interface to string concat",
			template: `{% var data = map[string]any{"flag": false} %}{{ "flag: " + data["flag"] }}`,
			wantOut:  "flag: false",
		},
		{
			name:     "string in interface to string concat (should still work)",
			template: `{% var data = map[string]any{"name": "Alice"} %}{{ "Hello, " + data["name"] }}`,
			wantOut:  "Hello, Alice",
		},
		{
			name:     "interface on left side of concat",
			template: `{% var data = map[string]any{"num": 100} %}{{ data["num"] + " items" }}`,
			wantOut:  "100 items",
		},
		{
			name:     "JSON-style float64 (common case from dig)",
			template: `{% var opts = map[string]any{"maxconn": 100.0} %}{{ " maxconn " + opts["maxconn"] }}`,
			wantOut:  " maxconn 100",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fsys := scriggo.Files{"test.html": []byte(tt.template)}
			opts := &scriggo.BuildOptions{}

			tmpl, err := scriggo.BuildTemplate(fsys, "test.html", opts)
			if err != nil {
				t.Fatalf("Build error: %v", err)
			}

			var out strings.Builder
			err = tmpl.Run(&out, nil, nil)
			if err != nil {
				t.Fatalf("Unexpected runtime error: %v", err)
			}

			got := out.String()
			if got != tt.wantOut {
				t.Errorf("Output mismatch:\ngot:  %q\nwant: %q", got, tt.wantOut)
			}
		})
	}
}

// TestInterfaceIndexing tests that indexing on interface{} types works when
// the underlying value is a map with string keys.
func TestInterfaceIndexing(t *testing.T) {
	tests := []struct {
		name     string
		template string
		wantOut  string
	}{
		{
			name:     "direct map indexing on interface{}",
			template: `{% var data any = map[string]any{"name": "test"} %}{{ data["name"] }}`,
			wantOut:  "test",
		},
		{
			name: "range loop with direct indexing",
			template: `{% var items = []any{map[string]any{"k": "v1"}, map[string]any{"k": "v2"}} %}` +
				`{% for _, item := range items %}{{ item["k"] }}{% end %}`,
			wantOut: "v1v2",
		},
		{
			name:     "nested map access",
			template: `{% var data any = map[string]any{"nested": map[string]any{"key": "value"}} %}{{ data["nested"]["key"] }}`,
			wantOut:  "value",
		},
		{
			name:     "interface{} alias any",
			template: `{% var data interface{} = map[string]interface{}{"x": "y"} %}{{ data["x"] }}`,
			wantOut:  "y",
		},
		{
			name: "real-world JSON-like iteration pattern",
			template: `{% var all_routes = []any{
				map[string]any{"name": "route1", "path": "/a"},
				map[string]any{"name": "route2", "path": "/b"},
			} %}` +
				`{%- for _, route_info := range all_routes %}{{ route_info["name"] }}:{{ route_info["path"] }} {% end %}`,
			wantOut: "route1:/a route2:/b ",
		},
		{
			name:     "indexing result used in string concat",
			template: `{% var data any = map[string]any{"prefix": "Hello"} %}{{ data["prefix"] + " World" }}`,
			wantOut:  "Hello World",
		},
		{
			name:     "indexing result used in comparison",
			template: `{% var data any = map[string]any{"flag": "yes"} %}{% if data["flag"] == "yes" %}OK{% end %}`,
			wantOut:  "OK",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fsys := scriggo.Files{"test.html": []byte(tt.template)}
			opts := &scriggo.BuildOptions{}

			tmpl, err := scriggo.BuildTemplate(fsys, "test.html", opts)
			if err != nil {
				t.Fatalf("Build error: %v", err)
			}

			var out strings.Builder
			err = tmpl.Run(&out, nil, nil)
			if err != nil {
				t.Fatalf("Runtime error: %v", err)
			}

			got := out.String()
			if got != tt.wantOut {
				t.Errorf("Output mismatch:\ngot:  %q\nwant: %q", got, tt.wantOut)
			}
		})
	}
}

// TestInterfaceIndexingErrors tests error cases for interface{} indexing.
func TestInterfaceIndexingErrors(t *testing.T) {
	// Test runtime error when interface{} contains non-map value
	template := `{% var data any = "not a map" %}{{ data["key"] }}`

	fsys := scriggo.Files{"test.html": []byte(template)}
	opts := &scriggo.BuildOptions{}

	tmpl, err := scriggo.BuildTemplate(fsys, "test.html", opts)
	if err != nil {
		t.Fatalf("Build error: %v", err)
	}

	var out strings.Builder
	err = tmpl.Run(&out, nil, nil)
	if err == nil {
		t.Fatal("Expected runtime error, got nil")
	}

	// Should mention the type doesn't support indexing (error includes type name)
	errStr := err.Error()
	if !strings.Contains(errStr, "non-map") && !strings.Contains(errStr, "string") {
		t.Errorf("Expected error about non-map type, got: %v", err)
	}
}

// matchesAnyOrder checks if got matches want accounting for map iteration order variations.
// It finds the longest common prefix and suffix, then checks if the middle parts
// are permutations of each other (for map key iteration order differences).
func matchesAnyOrder(got, want string) bool {
	if got == want {
		return true
	}
	if len(got) != len(want) {
		return false
	}

	// Find common prefix
	prefixLen := 0
	for i := 0; i < len(got) && i < len(want); i++ {
		if got[i] == want[i] {
			prefixLen++
		} else {
			break
		}
	}

	// Find common suffix (from the end, not overlapping with prefix)
	suffixLen := 0
	for i := 0; i < len(got)-prefixLen && i < len(want)-prefixLen; i++ {
		gi := len(got) - 1 - i
		wi := len(want) - 1 - i
		if got[gi] == want[wi] {
			suffixLen++
		} else {
			break
		}
	}

	// Guard against invalid slice bounds (prefix and suffix overlap)
	if prefixLen+suffixLen > len(got) {
		return false
	}

	// Extract the middle parts that differ
	gotMiddle := got[prefixLen : len(got)-suffixLen]
	wantMiddle := want[prefixLen : len(want)-suffixLen]

	// Check if middle parts are character permutations
	if len(gotMiddle) != len(wantMiddle) {
		return false
	}
	if len(gotMiddle) == 0 {
		return true
	}

	gotCounts := make(map[rune]int)
	wantCounts := make(map[rune]int)
	for _, r := range gotMiddle {
		gotCounts[r]++
	}
	for _, r := range wantMiddle {
		wantCounts[r]++
	}
	for r, c := range wantCounts {
		if gotCounts[r] != c {
			return false
		}
	}
	return true
}
