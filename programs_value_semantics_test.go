package scriggo

import (
	"fmt"
	"runtime"
	"strings"
	"testing"

	"github.com/haproxy-haptic/scriggo/native"
)

func TestValueLoadsSnapshotTheirStorage(t *testing.T) {
	values := []struct {
		name, typ, initial, zero, check string
	}{
		{"slice", "[]int", "[]int{7}", "nil", "len(saved) == 1 && saved[0] == 7"},
		{"map", "map[string]int", `map[string]int{"a": 7}`, "nil", `saved["a"] == 7`},
		{"pointer", "*int", "new(int)", "nil", "saved != nil"},
		{"channel", "chan int", "make(chan int, 2)", "nil", "saved != nil && cap(saved) == 2"},
		{"function", "func() int", "func() int { return 7 }", "nil", "saved != nil && saved() == 7"},
		{"complex64", "complex64", "3i", "0", "saved == 3i"},
		{"complex128", "complex128", "3i", "0", "saved == 3i"},
		{"array", "[1]int", "[1]int{7}", "[1]int{}", "saved[0] == 7"},
		{"struct", "struct { N int }", "struct { N int }{7}", "struct { N int }{}", "saved.N == 7"},
	}
	for _, value := range values {
		t.Run(value.name, func(t *testing.T) {
			initial := "var source " + value.typ + " = " + value.initial + "\n"
			check := "if !(" + value.check + ") { panic(\"saved value changed\") }"
			bodies := map[string]string{
				"global":      "package main\n" + initial + "func main() { saved := source; source = " + value.zero + "; " + check + " }",
				"closure":     "package main\nfunc main() { " + initial + "func() { saved := source; source = " + value.zero + "; " + check + " }() }",
				"field":       fmt.Sprintf("package main\nfunc main() { holder := struct { Value %s }{%s}; saved := holder.Value; holder.Value = %s; %s }", value.typ, value.initial, value.zero, check),
				"index":       fmt.Sprintf("package main\nfunc main() { values := []%s{%s}; saved := values[0]; values[0] = %s; %s }", value.typ, value.initial, value.zero, check),
				"dereference": "package main\nfunc main() { " + initial + "ptr := &source; saved := *ptr; source = " + value.zero + "; " + check + " }",
			}
			for name, source := range bodies {
				t.Run(name, func(t *testing.T) { runValueSemanticsProgram(t, source) })
			}
		})
	}
}

func TestValueSnapshotsKeepSharedContents(t *testing.T) {
	runValueSemanticsProgram(t, `package main
var slice = []int{1}
var mapping = map[string]int{"a": 1}
func main() {
    savedSlice := slice
    savedMap := mapping
    slice[0] = 2
    mapping["a"] = 2
    if savedSlice[0] != 2 || savedMap["a"] != 2 { panic("contents were deep copied") }
}`)
}

func TestNestedAssignmentsKeepTheirStorage(t *testing.T) {
	runValueSemanticsProgram(t, `package main
type Item struct { N int }
var global struct { Item Item }
func main() {
    items := []Item{{1}}
    items[0].N = 7
    if items[0].N != 7 { panic("slice field assignment") }
    var arrays [2][2]int
    arrays[1][1] = 7
    if arrays[1][1] != 7 { panic("nested array assignment") }
    global.Item.N = 7
    if global.Item.N != 7 { panic("nested global assignment") }
    pointer := &global
    pointer.Item.N = 8
    if global.Item.N != 8 { panic("nested pointer assignment") }
    n := 3
    p := &n
    *p += 2
    if n != 5 { panic("pointer compound assignment") }
}`)
	runValueSemanticsProgram(t, `package main
var global struct { A, B int; Items [2]struct { N int } }
func change() int { global.B = 9; return 7 }
func main() {
    global.A, global.B = 1, 2
    if global.A != 1 || global.B != 2 { panic("sibling writes lost") }
    global.A = change()
    if global.A != 7 || global.B != 9 { panic("right side write lost") }
    global.Items[1].N = 3
    global.Items[1].N += 2
    if global.Items[1].N != 5 { panic("nested indexed write lost") }
}`)
}

func TestExplicitStringConversionsKeepGoSemantics(t *testing.T) {
	cases := []struct{ name, body string }{
		{"signed", `var n int = 97; if string(n) != "a" { panic("integer conversion") }`},
		{"unsigned", `var n uint16 = 99; if string(n) != "c" { panic("integer conversion") }`},
		{"negative", `n := -1; if string(n) != "\ufffd" { panic("invalid rune") }`},
		{"out_of_range", `var n uint64 = 1 << 62; if string(n) != "\ufffd" { panic("invalid rune") }`},
		{"bytes", `s := []byte{97, 98, 99}; if string(s) != "abc" { panic("byte conversion") }`},
		{"runes", `s := []rune{97, 0x1f600}; if string(s) != "a😀" { panic("rune conversion") }`},
		{"named", `type Text string; var n byte = 97; if Text(n) != "a" { panic("named conversion") }`},
		{"boxed", `n := 97; var s interface{} = string(n); if s.(string) != "a" { panic("boxed conversion") }`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) { runValueSemanticsProgram(t, "package main\nfunc main() { "+tc.body+" }") })
	}
}

func TestInterfaceEqualityDoesNotCoerceOperands(t *testing.T) {
	runValueSemanticsProgram(t, `package main
type Number int
func main() {
    var truth interface{} = true
    var number interface{} = Number(1)
    if truth == 8 || 8 == truth || number == 1 || 1 == number { panic("coerced equality") }
    if truth != true || true != truth { panic("equal boolean") }
    p := new(int)
    var boxed interface{} = p
    if p != boxed || boxed != p { panic("pointer equality") }
}`)
}

func TestUncomparableInterfaceEqualityIsRecoverable(t *testing.T) {
	for _, tc := range []struct{ value, typ string }{
		{"[]int{1}", "[]int"},
		{"map[string]int{}", "map[string]int"},
		{"func() {}", "func()"},
		{"[1]interface{}{[]int{1}}", "[]int"},
		{"struct { Value interface{} }{[]int{1}}", "[]int"},
	} {
		t.Run(tc.value, func(t *testing.T) {
			var recovered any
			options := &BuildOptions{Packages: native.Packages{
				"probe": native.Package{Name: "probe", Declarations: native.Declarations{
					"Record": func(value any) { recovered = value },
				}},
			}}
			source := "package main\nimport \"probe\"\nfunc main() { defer func() { probe.Record(recover()) }(); var a interface{} = " + tc.value + "; _ = a == a }"
			program, err := Build(Files{"main.go": []byte(source)}, options)
			if err != nil {
				t.Fatal(err)
			}
			if err := program.Run(nil); err != nil {
				t.Fatal(err)
			}
			want := "runtime error: comparing uncomparable type " + tc.typ
			if err, ok := recovered.(runtime.Error); !ok || err.Error() != want {
				t.Fatalf("recovered %T %v, want runtime.Error %q", recovered, recovered, want)
			}
		})
	}
	runValueSemanticsProgram(t, `package main
func main() {
    var a interface{} = struct { N int; Value interface{} }{1, []int{1}}
    var b interface{} = struct { N int; Value interface{} }{2, []int{1}}
    if a == b { panic("unequal struct fields compared equal") }
    var c interface{} = []int{1}
    var d interface{} = []string{"1"}
    if c == d { panic("different dynamic types compared equal") }
}`)
}

func TestInterfaceMembershipKeepsElementTypes(t *testing.T) {
	source := `{% var items = []interface{}{true, 2, "3"} %}{{ items contains 2 }}|{{ items contains 3 }}|{{ items contains "3" }}|{{ items not contains false }}`
	tmpl, err := BuildTemplate(Files{"test.txt": []byte(source)}, "test.txt", nil)
	if err != nil {
		t.Fatal(err)
	}
	var output strings.Builder
	if err := tmpl.Run(&output, nil, nil); err != nil {
		t.Fatal(err)
	}
	if got, want := output.String(), "true|false|true|true"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func runValueSemanticsProgram(t *testing.T, source string) {
	t.Helper()
	defer func() {
		if value := recover(); value != nil {
			t.Fatalf("unexpected panic: %v", value)
		}
	}()
	program, err := Build(Files{"main.go": []byte(source)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := program.Run(nil); err != nil {
		t.Fatal(err)
	}
}
