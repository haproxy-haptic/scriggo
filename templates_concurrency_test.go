// Copyright 2026 The Scriggo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package scriggo

import (
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/haproxy-haptic/scriggo/internal/fstest"
	"github.com/haproxy-haptic/scriggo/native"
)

type compileBool bool

func TestConcurrentTemplateBuilds(t *testing.T) {
	for worker := range 16 {
		t.Run(strconv.Itoa(worker), func(t *testing.T) {
			t.Parallel()
			for range 16 {
				testConstantConversions(t)
			}
		})
	}
}

func TestTemplateConstantConversions(t *testing.T) {
	testConstantConversions(t)
}

func testConstantConversions(t *testing.T) {
	t.Helper()
	tests := []struct {
		name   string
		source string
		want   string
	}{
		{
			name:   "predeclared",
			source: `{{ named(true) }}|{{ describe(true) }}|{{ named(false) }}|{{ describe(false) }}`,
			want:   "scriggo.compileBool:true|bool:true|scriggo.compileBool:false|bool:false",
		},
		{
			name:   "predeclared-reverse",
			source: `{{ describe(true) }}|{{ named(true) }}|{{ describe(false) }}|{{ named(false) }}`,
			want:   "bool:true|scriggo.compileBool:true|bool:false|scriggo.compileBool:false",
		},
		{
			name:   "declared",
			source: `{% const yes = true %}{% const no = false %}{{ named(yes) }}|{{ describe(yes) }}|{{ named(no) }}|{{ describe(no) }}`,
			want:   "scriggo.compileBool:true|bool:true|scriggo.compileBool:false|bool:false",
		},
		{
			name:   "default",
			source: `{% var yes Named = true default false %}{% var plainYes any = true default false %}{% var no Named = false default true %}{% var plainNo any = false default true %}{{ describe(yes) }}|{{ describe(plainYes) }}|{{ describe(no) }}|{{ describe(plainNo) }}`,
			want:   "scriggo.compileBool:true|bool:true|scriggo.compileBool:false|bool:false",
		},
		{
			name:   "imported",
			source: `{% import "constants" %}{{ describe(constants.Yes) }}|{{ named(constants.Yes) }}|{{ describe(constants.No) }}|{{ named(constants.No) }}`,
			want:   "bool:true|scriggo.compileBool:true|bool:false|scriggo.compileBool:false",
		},
		{
			name:   "comparison",
			source: `{{ named(number() == 1) }}|{{ describe(number() == 1) }}|{{ named(number() != 1) }}|{{ describe(number() != 1) }}`,
			want:   "scriggo.compileBool:true|bool:true|scriggo.compileBool:false|bool:false",
		},
	}
	for _, tt := range tests {
		options := BuildOptions{Globals: native.Declarations{
			"Named":    reflect.TypeFor[compileBool](),
			"named":    func(value compileBool) string { return fmt.Sprintf("%T:%t", value, value) },
			"describe": func(value any) string { return fmt.Sprintf("%T:%v", value, value) },
			"number":   func() int { return 1 },
		}}
		options.Packages = native.Packages{
			"constants": native.Package{Name: "constants", Declarations: native.Declarations{
				"Yes": native.UntypedBooleanConst(true),
				"No":  native.UntypedBooleanConst(false),
			}},
		}
		template, err := BuildTemplate(fstest.Files{"index.txt": tt.source}, "index.txt", &options)
		if err != nil {
			t.Fatalf("%s: build: %v", tt.name, err)
		}
		var out strings.Builder
		if err := template.Run(&out, nil, nil); err != nil {
			t.Fatalf("%s: run: %v", tt.name, err)
		}
		if out.String() != tt.want {
			t.Fatalf("%s: got %q, want %q", tt.name, out.String(), tt.want)
		}
	}
}
