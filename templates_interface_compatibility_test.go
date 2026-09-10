package scriggo

import (
	"strings"
	"testing"
)

func TestTemplateInterfaceExpressionsKeepImplicitConversions(t *testing.T) {
	source := `{%%
		n := map[string]interface{}{
			"A": 1,
			"M": map[string]interface{}{"A": []int{0, 1, 2, 3, 4, 5}},
			"X": map[string]interface{}{"Y": map[string]interface{}{"Z": 7}},
		}
		m := map[string]int{}
		n.A += 1
		a := n.M.A.([]int)[5]
		m.X = n.X.Y.Z
	%%}{{ n.A }}|{{ a }}|{{ m.X }}`
	tmpl, err := BuildTemplate(Files{"index.txt": []byte(source)}, "index.txt", nil)
	if err != nil {
		t.Fatal(err)
	}
	var output strings.Builder
	if err := tmpl.Run(&output, nil, nil); err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(output.String()); got != "2|5|7" {
		t.Fatalf("output = %q, want 2|5|7", got)
	}
}

func TestInterfaceCompoundAssignments(t *testing.T) {
	for _, tc := range []struct{ name, source, want string }{
		{"local", `{%% var value interface{} = 4; value += 3 %%}{{ value }}`, "7"},
		{"closure", `{%% var value interface{} = 4; f := func() { value *= 3 }; f() %%}{{ value }}`, "12"},
		{"field", `{%% value := struct { N interface{} }{4}; value.N -= 3 %%}{{ value.N }}`, "1"},
		{"index", `{%% values := []interface{}{8}; values[0] /= 2 %%}{{ values[0] }}`, "4"},
		{"pointer", `{%% var value interface{} = 4; p := &value; *p += 3 %%}{{ value }}`, "7"},
		{"string", `{%% var value interface{} = "a"; value += "b" %%}{{ value }}`, "ab"},
		{"complex", `{%% var value interface{} = 2i; value += 3i %%}{{ value }}`, "5i"},
		{"receiver_once", `{%% calls := 0; values := map[string]interface{}{"a": 1}; get := func() map[string]interface{} { calls++; return values }; get()["a"] += 2 %%}{{ calls }}|{{ values.a }}`, "1|3"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tmpl, err := BuildTemplate(Files{"index.txt": []byte(tc.source)}, "index.txt", nil)
			if err != nil {
				t.Fatal(err)
			}
			var output strings.Builder
			if err := tmpl.Run(&output, nil, nil); err != nil {
				t.Fatal(err)
			}
			if got := output.String(); got != tc.want {
				t.Fatalf("output = %q, want %q", got, tc.want)
			}
		})
	}
}
