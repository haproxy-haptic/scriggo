// Copyright 2019 The Scriggo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestComparisonReportsRecoveredPanic(t *testing.T) {
	if err := buildCmd(); err != nil {
		t.Fatal(err)
	}
	src := []byte("package main\nfunc main() {\n_ = missing // ERROR \"[\"\n}\n")
	err := test(src, "errorcheck", "main.go", nil)
	if err == nil || !strings.HasPrefix(err.Error(), "test panicked: regexp:") {
		t.Fatalf("got %v, want regexp panic reported as failure", err)
	}
}

func TestRunGcUsesCheckout(t *testing.T) {
	path := filepath.Join(t.TempDir(), "main.go")
	src := []byte(`package main
import "github.com/haproxy-haptic/scriggo/native"
func main() { var env native.Env; println(env == nil) }
`)
	if err := os.WriteFile(path, src, 0600); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr, err := runGc(path)
	if err != nil || code != 0 || len(stdout) != 0 || string(stderr) != "true\n" {
		t.Fatalf("gc: code=%d stdout=%q stderr=%q err=%v", code, stdout, stderr, err)
	}
}

func Test_errorcheck(t *testing.T) {

	// Build the executable 'cmd/cmd', that is going to be used in this test.
	err := buildCmd()
	if err != nil {
		t.Fatalf("cannot build cmd: %s", err)
	}

	mustPass := []struct {
		src string
		ext string
	}{
		{
			src: joinLines([]string{
				`package main`,
				`func main() {`,
				`	_ = a // ERROR "undefined: a"`,
				`	_ = b // ERROR "undefined: b"`,
				`	_ = c // ERROR "undefined: c"`,
				`}`,
			}),
			ext: ".go",
		},
		{
			src: joinLines([]string{
				`package main`,
				`func main() {`,
				`	x := 3 // ERROR "x declared but not used"`,
				`}`,
			}),
			ext: ".go",
		},
	}

	for _, test := range mustPass {
		err := errorcheck([]byte(test.src), test.ext, nil)
		if err != nil {
			t.Fatalf("unexpected error %v", err)
		}
	}

	mustFail := []struct {
		src   string
		ext   string
		panic string
	}{
		{
			src: joinLines([]string{
				`package main`,
				`func main() {`,
				`	_ = a // ERROR "undefined: A"`,
				`}`,
			}),
			ext:   ".go",
			panic: "output does not match with the given regular expression:\n\n\tregexp:    undefined: A\n\toutput:    undefined: a\n",
		},
	}

	for _, test := range mustFail {
		func() {
			err := errorcheck([]byte(test.src), test.ext, nil)
			if err == nil {
				t.Fatal("should return a not nil error")
			}
			got := err.Error()
			if test.panic != got {
				t.Fatalf("expecting %q, got %q", test.panic, got)
			}
		}()
	}

}

func Test_readMode(t *testing.T) {
	cases := []struct {
		src          string
		ext          string
		expectedMode string
		expectedArgs []string
	}{
		{
			src:          `// run`,
			ext:          `.go`,
			expectedMode: `run`,
		},
		{
			src:          `// run -option`,
			ext:          `.go`,
			expectedMode: `run`,
			expectedArgs: []string{"-option"},
		},
		{
			src:          `// compile -time=3s`,
			ext:          `.go`,
			expectedMode: `compile`,
			expectedArgs: []string{"-time=3s"},
		},
		{
			src: `{# render -time=5ms #}
text`,
			ext:          `.html`,
			expectedMode: `render`,
			expectedArgs: []string{"-time=5ms"},
		},
	}
	for _, c := range cases {
		t.Run(c.src, func(t *testing.T) {
			mode, args, err := readMode([]byte(c.src), c.ext)
			if err != nil {
				t.Errorf("expecting mode '%s', got error '%s'", c.expectedMode, err)
			}
			if mode != c.expectedMode {
				t.Errorf("expecting mode '%s', got '%s'", c.expectedMode, mode)
			}
			if len(args) == 0 && len(c.expectedArgs) == 0 {
				// Ok.
			} else if !reflect.DeepEqual(args, c.expectedArgs) {
				t.Errorf("expecting args %#v, got %#v", c.expectedArgs, args)
			}
		})
	}
}

func Test_goBaseVersionHyphenatedExperiment(t *testing.T) {
	if got, want := goBaseVersion("go1.27.0-X:nodwarf5"), "go1.27"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}
