// Copyright 2026 The Scriggo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package scriggo

import (
	"reflect"
	"slices"
	"testing"

	"gitlab.com/haproxy-haptic/scriggo/native"
)

type nativeValueAccessItem struct {
	Name string
}

type nativeValueAccessStore struct {
	T          nativeValueAccessItem
	List       func() []*nativeValueAccessItem
	Fetch      func(...any) []*nativeValueAccessItem
	GetSingle  func(...any) *nativeValueAccessItem
	APIVersion func() string
}

type nativeValueAccessResources struct {
	Routes  *nativeValueAccessStore `json:"routes"`
	Other   *nativeValueAccessStore `json:"other"`
	Items   []string                `json:"items"`
	Enabled bool                    `json:"enabled"`
}

type nativeValueAccessShared struct{}

func (*nativeValueAccessShared) Get(string) any { return nil }

func TestTemplateUsedNativeValueAccessesKeepsPureCallableChainsExact(t *testing.T) {
	declaration := (*nativeValueAccessResources)(nil)
	tests := map[string]struct {
		files Files
		paths []string
	}{
		"direct": {
			files: Files{
				"index.txt": []byte(`{{ len(resources.routes.List()) }}{{ resources.routes.APIVersion() }}`),
			},
			paths: []string{"routes.APIVersion", "routes.List"},
		},
		"function alias": {
			files: Files{
				"index.txt": []byte(`{% list := resources.routes.List %}{{ len(list()) }}`),
			},
			paths: []string{"routes.List"},
		},
		"macro": {
			files: Files{
				"index.txt": []byte(`{% macro Read() string %}{{ len(resources.routes.List()) }}{% end %}{{ Read() }}`),
			},
			paths: []string{"routes.List"},
		},
		"rendered template": {
			files: Files{
				"index.txt": []byte(`{{ render "body.txt" }}`),
				"body.txt":  []byte(`{{ len(resources.routes.List()) }}`),
			},
			paths: []string{"routes.List"},
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			tmpl := buildNativeValueAccessTemplate(t, test.files, declaration)
			if accesses := tmpl.UsedNativeValueAccesses(); len(accesses) != 0 {
				t.Fatalf("value accesses = %#v, want none", accesses)
			}
			assertNativeValueCallablePaths(t, tmpl, test.paths...)
		})
	}
}

func TestTemplateUsedNativeValueAccessesSeparatesMethodResultsFromReceivers(t *testing.T) {
	declaration := (*nativeValueAccessShared)(nil)
	tests := map[string]struct {
		files      Files
		wantAccess bool
	}{
		"direct method": {
			files: Files{"index.txt": []byte(`{{ consume(shared.Get("value")) }}`)},
		},
		"method alias": {
			files: Files{"index.txt": []byte(`{% get := shared.Get %}{{ consume(get("value")) }}`)},
		},
		"imported macro": {
			files: Files{
				"index.txt":   []byte(`{% import "library.txt" for Read %}{{ Read() }}`),
				"library.txt": []byte(`{% macro Read() string %}{{ consume(shared.Get("value")) }}{% end %}`),
			},
		},
		"receiver escape": {
			files:      Files{"index.txt": []byte(`{{ consume(shared.Get("value")) }}{{ consume(shared) }}`)},
			wantAccess: true,
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			tmpl := buildNativeValueAccessTemplateWithDeclarations(t, test.files, native.Declarations{
				"consume": func(any) string { return "" },
				"shared":  declaration,
			})
			accesses := tmpl.UsedNativeValueAccesses()
			if !test.wantAccess {
				if len(accesses) != 0 {
					t.Fatalf("value accesses = %#v, want none", accesses)
				}
				return
			}
			assertNativeValueAccessNamed(t, tmpl, "shared", declaration, "")
		})
	}
}

func TestTemplateUsedNativeValueAccessesReportsEveryWholeValueUse(t *testing.T) {
	declaration := (*nativeValueAccessResources)(nil)
	tests := map[string]struct {
		source string
		path   string
	}{
		"root stored": {
			source: `{% all := resources %}{{ len(all.routes.List()) }}`,
			path:   "",
		},
		"member stored": {
			source: `{% routes := resources.routes %}{{ len(routes.List()) }}`,
			path:   "routes",
		},
		"member boxed": {
			source: `{% var boxed interface{} = resources.routes %}{{ consume(boxed) }}`,
			path:   "routes",
		},
		"member passed": {
			source: `{{ consume(resources.routes) }}`,
			path:   "routes",
		},
		"member returned": {
			source: `{% leak := func() interface{} { return resources.routes } %}{{ consume(leak()) }}`,
			path:   "routes",
		},
		"member ranged": {
			source: `{% for _, item := range resources.items %}{{ item }}{% end %}`,
			path:   "items",
		},
		"member compared": {
			source: `{% if resources.enabled == true %}yes{% end %}`,
			path:   "enabled",
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			tmpl := buildNativeValueAccessTemplate(t, Files{
				"index.txt": []byte(test.source),
			}, declaration)
			assertNativeValueAccess(t, tmpl, declaration, test.path)
		})
	}
}

func TestTemplateUsedNativeValueAccessesCoversTransitiveAndUnreachableBodies(t *testing.T) {
	declaration := (*nativeValueAccessResources)(nil)
	tests := map[string]Files{
		"unreachable macro": {
			"index.txt": []byte(`{% macro Leak() string %}{{ consume(resources.routes) }}{% end %}safe`),
		},
		"imported macro": {
			"index.txt":   []byte(`{% import "library.txt" for Leak %}{{ Leak() }}`),
			"library.txt": []byte(`{% macro Leak() string %}{{ consume(resources.routes) }}{% end %}`),
		},
		"rendered body": {
			"index.txt": []byte(`{{ render "body.txt" }}`),
			"body.txt":  []byte(`{{ consume(resources.routes) }}`),
		},
	}
	for name, files := range tests {
		t.Run(name, func(t *testing.T) {
			tmpl := buildNativeValueAccessTemplate(t, files, declaration)
			assertNativeValueAccess(t, tmpl, declaration, "routes")
		})
	}
}

func TestTemplateUsedNativeValueAccessesIgnoresTypeOnlySelectorChains(t *testing.T) {
	declaration := (*nativeValueAccessResources)(nil)
	tmpl := buildNativeValueAccessTemplate(t, Files{
		"index.txt": []byte(`{% macro value(item *resources.routes.T) string %}{{ item.Name }}{% end %}`),
	}, declaration)
	if accesses := tmpl.UsedNativeValueAccesses(); len(accesses) != 0 {
		t.Fatalf("value accesses = %#v, want none for type-only chain", accesses)
	}
	assertNativeValueCallablePaths(t, tmpl)
}

func TestTemplateUsedNativeValueAccessesBindsExactDeclarationIdentity(t *testing.T) {
	first := (*nativeValueAccessResources)(nil)
	secondValue := nativeValueAccessResources{}
	second := &secondValue
	tmpl, err := BuildTemplate(Files{
		"index.txt": []byte(`{{ consume(first.routes) }}{{ len(second.routes.List()) }}`),
	}, "index.txt", &BuildOptions{Globals: native.Declarations{
		"consume": native.Synchronous(func(any) int { return 0 }),
		"first":   native.Synchronous(first, "*.List"),
		"second":  native.Synchronous(second, "*.List"),
	}})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	accesses := tmpl.UsedNativeValueAccesses()
	if len(accesses) != 1 || accesses[0].Declaration != first ||
		accesses[0].DeclarationName != "first" || accesses[0].MemberPath != "routes" {
		t.Fatalf("value accesses = %#v, want only exact first.routes declaration", accesses)
	}
}

func buildNativeValueAccessTemplate(
	t *testing.T,
	files Files,
	declaration native.Declaration,
) *Template {
	t.Helper()
	tmpl, err := BuildTemplate(files, "index.txt", &BuildOptions{Globals: native.Declarations{
		"consume": native.Synchronous(func(any) int { return 0 }),
		"resources": native.Synchronous(
			declaration,
			"*.List", "*.Fetch", "*.GetSingle", "*.APIVersion",
		),
	}})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	return tmpl
}

func buildNativeValueAccessTemplateWithDeclarations(
	t *testing.T,
	files Files,
	declarations native.Declarations,
) *Template {
	t.Helper()
	tmpl, err := BuildTemplate(files, "index.txt", &BuildOptions{Globals: declarations})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	return tmpl
}

func assertNativeValueAccess(
	t *testing.T,
	tmpl *Template,
	declaration native.Declaration,
	path string,
) {
	assertNativeValueAccessNamed(t, tmpl, "resources", declaration, path)
}

func assertNativeValueAccessNamed(
	t *testing.T,
	tmpl *Template,
	name string,
	declaration native.Declaration,
	path string,
) {
	t.Helper()
	for _, access := range tmpl.UsedNativeValueAccesses() {
		if access.Package == "main" && access.DeclarationName == name &&
			access.Declaration == declaration && access.MemberPath == path {
			return
		}
	}
	t.Fatalf("value accesses = %#v, want %s path %q", tmpl.UsedNativeValueAccesses(), name, path)
}

func assertNativeValueCallablePaths(t *testing.T, tmpl *Template, want ...string) {
	t.Helper()
	var got []string
	for _, callable := range tmpl.UsedNativeCallables() {
		if callable.Package == "main" && callable.DeclarationName == "resources" {
			got = append(got, callable.MemberPath)
		}
	}
	slices.Sort(got)
	slices.Sort(want)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("callable paths = %#v, want %#v", got, want)
	}
}
