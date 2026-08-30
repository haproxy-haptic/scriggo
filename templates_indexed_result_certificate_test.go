// Copyright 2026 The Scriggo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package scriggo

import (
	"errors"
	"strings"
	"testing"
	"time"

	"gitlab.com/haproxy-haptic/scriggo/native"
)

type indexedResultMethods struct{}

func (indexedResultMethods) Read() string  { return "read" }
func (indexedResultMethods) Write() string { return "write" }

type indexedResultStore struct {
	Call func() string
}

type indexedResultRegistrar interface {
	Profile() (string, error)
	Backend() (string, error)
}

type indexedResultRegistrarImpl struct{}

func (indexedResultRegistrarImpl) Profile() (string, error) {
	return "", errors.New("profile error")
}

func (indexedResultRegistrarImpl) Backend() (string, error) {
	return "", errors.New("backend error")
}

func TestTemplateBatchCertificateAcceptsIndexedFunctionResultMembers(t *testing.T) {
	makeValue := func() (indexedResultMethods, error) { return indexedResultMethods{}, nil }
	makeError := func() (string, error) { return "", errors.New("indexed error") }
	tests := []struct {
		name    string
		source  string
		globals native.Declarations
		want    string
	}{
		{
			name:   "duration result zero",
			source: `{% value, _ := parseDuration("1500ms") %}{{ value.Milliseconds() }}`,
			globals: native.Declarations{
				"parseDuration": native.Synchronous(time.ParseDuration, "[0].Milliseconds"),
			},
			want: "1500",
		},
		{
			name:   "error interface result one",
			source: `{% _, err := makeError() %}{{ err.Error() }}`,
			globals: native.Declarations{
				"makeError": native.Synchronous(makeError, "[1].Error"),
			},
			want: "indexed error",
		},
		{
			name:   "aliased result zero",
			source: `{% value, _ := makeValue() %}{% alias := value %}{{ alias.Read() }}`,
			globals: native.Declarations{
				"makeValue": native.Synchronous(makeValue, "[0].Read"),
			},
			want: "read",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			tmpl := buildIndexedResultTemplate(t, test.source, test.globals)
			if !tmpl.BatchSafe() {
				t.Fatalf("BatchSafe = false, callables = %#v", tmpl.UsedNativeCallables())
			}
			var output strings.Builder
			if err := tmpl.RunBatch([]BatchRun{{Out: &output}}); err != nil {
				t.Fatalf("run batch: %v", err)
			}
			if got := output.String(); got != test.want {
				t.Fatalf("output = %q, want %q", got, test.want)
			}
		})
	}
}

func TestTemplateBatchCertificateAcceptsIndexedMethodResultMember(t *testing.T) {
	tmpl := buildIndexedResultTemplate(t,
		`{% _, err := registry.Profile() %}{{ err.Error() }}`,
		native.Declarations{
			"registry": native.Synchronous(
				(*indexedResultRegistrar)(nil),
				"Profile",
				"Profile[1].Error",
			),
		},
	)
	if !tmpl.BatchSafe() {
		t.Fatalf("BatchSafe = false, callables = %#v", tmpl.UsedNativeCallables())
	}
	var output strings.Builder
	if err := tmpl.RunBatch([]BatchRun{{
		Out:  &output,
		Vars: map[string]any{"registry": indexedResultRegistrar(indexedResultRegistrarImpl{})},
	}}); err != nil {
		t.Fatalf("run batch: %v", err)
	}
	if got, want := output.String(), "profile error"; got != want {
		t.Fatalf("output = %q, want %q", got, want)
	}
}

func TestTemplateBatchCertificateBindsIndexedResultToExactMethod(t *testing.T) {
	tmpl := buildIndexedResultTemplate(t,
		`{% _, err := registry.Backend() %}{{ err.Error() }}`,
		native.Declarations{
			"registry": native.Synchronous(
				(*indexedResultRegistrar)(nil),
				"Backend",
				"Profile[1].Error",
			),
		},
	)
	assertIndexedResultBatchUnsafe(t, tmpl, "Backend[1].Error")
}

func TestTemplateBatchCertificateRejectsWrongIndexedFunctionResultMember(t *testing.T) {
	pair := func() (indexedResultMethods, indexedResultMethods) {
		return indexedResultMethods{}, indexedResultMethods{}
	}
	tests := []struct {
		name       string
		source     string
		member     string
		wantMember string
	}{
		{
			name:       "wrong result",
			source:     `{% _, second := pair() %}{{ second.Read() }}`,
			member:     "[0].Read",
			wantMember: "[1].Read",
		},
		{
			name:       "wrong member",
			source:     `{% first, _ := pair() %}{{ first.Write() }}`,
			member:     "[0].Read",
			wantMember: "[0].Write",
		},
		{
			name:       "unindexed multi result",
			source:     `{% first, _ := pair() %}{{ first.Read() }}`,
			member:     "Read",
			wantMember: "[0].Read",
		},
		{
			name:       "out of range certificate",
			source:     `{% first, _ := pair() %}{{ first.Read() }}`,
			member:     "[2].Read",
			wantMember: "[0].Read",
		},
		{
			name:       "leading zero index",
			source:     `{% first, _ := pair() %}{{ first.Read() }}`,
			member:     "[00].Read",
			wantMember: "[0].Read",
		},
		{
			name:       "signed index",
			source:     `{% first, _ := pair() %}{{ first.Read() }}`,
			member:     "[+0].Read",
			wantMember: "[0].Read",
		},
		{
			name:       "oversized index",
			source:     `{% first, _ := pair() %}{{ first.Read() }}`,
			member:     "[999999999999999999999999999999999999].Read",
			wantMember: "[0].Read",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			tmpl := buildIndexedResultTemplate(t, test.source, native.Declarations{
				"pair": native.Synchronous(pair, test.member),
			})
			assertIndexedResultBatchUnsafe(t, tmpl, test.wantMember)
		})
	}
}

func TestTemplateBatchCertificateBindsIndexedErrorToExactFunction(t *testing.T) {
	trusted := func() (string, error) { return "", errors.New("trusted") }
	ambient := func() (string, error) { return "", errors.New("ambient") }
	tmpl := buildIndexedResultTemplate(t,
		`{% _, trustedErr := trusted() %}{% _, ambientErr := ambient() %}`+
			`{{ trustedErr.Error() }}{{ ambientErr.Error() }}`,
		native.Declarations{
			"trusted": native.Synchronous(trusted, "[1].Error"),
			"ambient": native.Synchronous(ambient),
		},
	)
	if tmpl.BatchSafe() {
		t.Fatalf("BatchSafe = true, callables = %#v", tmpl.UsedNativeCallables())
	}
	trustedCertified := false
	ambientRejected := false
	for _, callable := range tmpl.UsedNativeCallables() {
		if callable.MemberPath != "[1].Error" {
			continue
		}
		switch callable.DeclarationName {
		case "trusted":
			trustedCertified = callable.Synchronous
		case "ambient":
			ambientRejected = !callable.Synchronous
		}
	}
	if !trustedCertified || !ambientRejected {
		t.Fatalf("callables = %#v, want trusted only", tmpl.UsedNativeCallables())
	}
}

func TestTemplateBatchCertificateRejectsIndexedResultReassignment(t *testing.T) {
	trusted := func() (indexedResultMethods, error) { return indexedResultMethods{}, nil }
	ambient := func() (indexedResultMethods, error) { return indexedResultMethods{}, nil }
	tmpl := buildIndexedResultTemplate(t,
		`{% selected, _ := trusted() %}{% replacement, _ := ambient() %}`+
			`{% selected = replacement %}{{ selected.Read() }}`,
		native.Declarations{
			"trusted": native.Synchronous(trusted, "[0].Read"),
			"ambient": native.Synchronous(ambient),
		},
	)
	assertIndexedResultBatchUnsafe(t, tmpl, "[0].Read")
}

func TestTemplateBatchCertificateRejectsIndexedResultMutationThroughAlias(t *testing.T) {
	makeStore := func() (*indexedResultStore, error) {
		return &indexedResultStore{Call: func() string { return "safe" }}, nil
	}
	ambient := func() string { return "ambient" }
	tmpl := buildIndexedResultTemplate(t,
		`{% store, _ := makeStore() %}{% alias := store %}`+
			`{% alias.Call = ambient %}{{ store.Call() }}`,
		native.Declarations{
			"makeStore": native.Synchronous(makeStore, "[0].Call"),
			"ambient":   native.Synchronous(ambient),
		},
	)
	if tmpl.BatchSafe() {
		t.Fatalf("BatchSafe = true, callables = %#v", tmpl.UsedNativeCallables())
	}
	if err := tmpl.RunBatch(nil); !errors.Is(err, ErrBatchUncertifiedNative) {
		t.Fatalf("empty batch error = %v, want %v", err, ErrBatchUncertifiedNative)
	}
}

func TestTemplateBatchCertificateTreatsRenderExpressionAsLocal(t *testing.T) {
	tmpl, err := BuildTemplate(Files{
		"index.txt":    []byte(`{% fragment := render "fragment.txt" %}{{ fragment }}`),
		"fragment.txt": []byte(`{{ store.Call() }}`),
	}, "index.txt", &BuildOptions{Globals: native.Declarations{
		"store": native.Synchronous((*batchCertificateFunctionStore)(nil), "Call"),
	}})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if !tmpl.BatchSafe() {
		t.Fatalf("BatchSafe = false, callables = %#v", tmpl.UsedNativeCallables())
	}
	for _, callable := range tmpl.UsedNativeCallables() {
		if callable.Path == `M"fragment.txt"()` {
			t.Fatalf("generated render macro reported as native: %#v", callable)
		}
	}
}

func TestTemplateBatchCertificateStillAuditsRenderedTemplateBody(t *testing.T) {
	tmpl, err := BuildTemplate(Files{
		"index.txt":    []byte(`{% fragment := render "fragment.txt" %}{{ fragment }}`),
		"fragment.txt": []byte(`{{ store.Call() }}`),
	}, "index.txt", &BuildOptions{Globals: native.Declarations{
		"store": (*batchCertificateFunctionStore)(nil),
	}})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if tmpl.BatchSafe() {
		t.Fatalf("BatchSafe = true, callables = %#v", tmpl.UsedNativeCallables())
	}
	foundStoreCall := false
	for _, callable := range tmpl.UsedNativeCallables() {
		if callable.Path == `M"fragment.txt"()` {
			t.Fatalf("generated render macro reported as native: %#v", callable)
		}
		if callable.Path == "store.Call" && !callable.Synchronous {
			foundStoreCall = true
		}
	}
	if !foundStoreCall {
		t.Fatalf("uncertified rendered body call missing: %#v", tmpl.UsedNativeCallables())
	}
	if err := tmpl.RunBatch(nil); !errors.Is(err, ErrBatchUncertifiedNative) {
		t.Fatalf("empty batch error = %v, want %v", err, ErrBatchUncertifiedNative)
	}
}

func buildIndexedResultTemplate(t *testing.T, source string, globals native.Declarations) *Template {
	t.Helper()
	tmpl, err := BuildTemplate(Files{"index.txt": []byte(source)}, "index.txt", &BuildOptions{Globals: globals})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	return tmpl
}

func assertIndexedResultBatchUnsafe(t *testing.T, tmpl *Template, member string) {
	t.Helper()
	if tmpl.BatchSafe() {
		t.Fatalf("BatchSafe = true, callables = %#v", tmpl.UsedNativeCallables())
	}
	found := false
	for _, callable := range tmpl.UsedNativeCallables() {
		if callable.MemberPath == member && !callable.Synchronous {
			found = true
		}
	}
	if !found {
		t.Fatalf("callables = %#v, want unsafe member %q", tmpl.UsedNativeCallables(), member)
	}
	if err := tmpl.RunBatch(nil); !errors.Is(err, ErrBatchUncertifiedNative) {
		t.Fatalf("empty batch error = %v, want %v", err, ErrBatchUncertifiedNative)
	}
}
