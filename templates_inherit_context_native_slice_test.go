// Copyright 2026 The Scriggo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package scriggo

import (
	"errors"
	"strconv"
	"strings"
	"testing"

	"gitlab.com/haproxy-haptic/scriggo/native"
)

type inheritContextNativeSliceCapability struct {
	name  string
	calls []string
}

func (capability *inheritContextNativeSliceCapability) Begin(_ native.Env, index int) {
	capability.calls = append(capability.calls, "begin:"+strconv.Itoa(index))
}

func (capability *inheritContextNativeSliceCapability) End(_ native.Env, index int) {
	capability.calls = append(capability.calls, "end:"+strconv.Itoa(index))
}

func (capability *inheritContextNativeSliceCapability) Name() string {
	return capability.name
}

func TestTemplateBatchCertificateFollowsInheritedNativeSliceElement(t *testing.T) {
	template, err := BuildTemplate(Files{
		"wrapper.txt": []byte(
			`{% for index, _ := range capabilities %}` +
				`{% capability := capabilities[index] %}` +
				`{{ render "entry.txt" inherit_context }}` +
				`{% end %}`,
		),
		"entry.txt": []byte(
			`{% capability.Begin(index) %}` +
				`{{ capability.Name() }}` +
				`{% capability.End(index) %}`,
		),
	}, "wrapper.txt", &BuildOptions{Globals: native.Declarations{
		"capabilities": native.Synchronous(
			(*[]*inheritContextNativeSliceCapability)(nil),
			"*.Begin",
			"*.End",
			"*.Name",
		),
	}})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if !template.BatchSafe() {
		t.Fatalf("BatchSafe = false, callables = %#v", template.UsedNativeCallables())
	}
	for _, callable := range template.UsedNativeCallables() {
		if callable.DeclarationName != "capabilities" || !callable.Synchronous {
			t.Fatalf("callable = %#v, want certified capabilities origin", callable)
		}
	}

	first := &inheritContextNativeSliceCapability{name: "first"}
	second := &inheritContextNativeSliceCapability{name: "second"}
	var output strings.Builder
	if err := template.RunBatch([]BatchRun{{
		Out:  &output,
		Vars: map[string]any{"capabilities": []*inheritContextNativeSliceCapability{first, second}},
	}}); err != nil {
		t.Fatalf("run batch: %v", err)
	}
	if got, want := output.String(), "firstsecond"; got != want {
		t.Fatalf("output = %q, want %q", got, want)
	}
	if got, want := strings.Join(first.calls, ","), "begin:0,end:0"; got != want {
		t.Fatalf("first calls = %q, want %q", got, want)
	}
	if got, want := strings.Join(second.calls, ","), "begin:1,end:1"; got != want {
		t.Fatalf("second calls = %q, want %q", got, want)
	}
}

func TestTemplateBatchCertificateRejectsUncertifiedInheritedNativeSliceElement(t *testing.T) {
	tests := []struct {
		name    string
		wrapper string
		globals native.Declarations
	}{
		{
			name: "missing element wildcard",
			wrapper: `{% for index, _ := range capabilities %}` +
				`{% capability := capabilities[index] %}` +
				`{{ render "entry.txt" inherit_context }}` +
				`{% end %}`,
			globals: native.Declarations{
				"capabilities": native.Synchronous(
					(*[]*inheritContextNativeSliceCapability)(nil),
					"Begin",
				),
			},
		},
		{
			name: "reassigned ambient origin",
			wrapper: `{% for index, _ := range capabilities %}` +
				`{% capability := capabilities[index] %}` +
				`{% capability = ambient %}` +
				`{{ render "entry.txt" inherit_context }}` +
				`{% end %}`,
			globals: native.Declarations{
				"ambient": (**inheritContextNativeSliceCapability)(nil),
				"capabilities": native.Synchronous(
					(*[]*inheritContextNativeSliceCapability)(nil),
					"*.Begin",
				),
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			template, err := BuildTemplate(Files{
				"wrapper.txt": []byte(test.wrapper),
				"entry.txt":   []byte(`{% capability.Begin(index) %}`),
			}, "wrapper.txt", &BuildOptions{Globals: test.globals})
			if err != nil {
				t.Fatalf("build: %v", err)
			}
			if template.BatchSafe() {
				t.Fatalf("BatchSafe = true, callables = %#v", template.UsedNativeCallables())
			}
			if err := template.RunBatch(nil); !errors.Is(err, ErrBatchUncertifiedNative) {
				t.Fatalf("RunBatch error = %v, want %v", err, ErrBatchUncertifiedNative)
			}
		})
	}
}
