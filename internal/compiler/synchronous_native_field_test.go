// Copyright 2026 The Scriggo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package compiler

import (
	"testing"

	"gitlab.com/haproxy-haptic/scriggo/internal/fstest"
	"gitlab.com/haproxy-haptic/scriggo/internal/runtime"
	"gitlab.com/haproxy-haptic/scriggo/native"
)

type synchronousNativeFieldStore struct {
	Reflective func(...any) *synchronousNativeFieldResult
	Direct     func(string) string
}

type synchronousNativeFieldResult struct {
	Name string
}

type synchronousNativeMethodSelector interface {
	Select(string) (string, bool)
}

func TestEmitterCertifiesOnlySynchronousReflectiveFieldCalls(t *testing.T) {
	tests := []struct {
		name          string
		source        string
		allowGo       bool
		wantCertified bool
	}{
		{
			name:          "immediate reflective call",
			source:        `{{ store.Reflective("value").Name }}`,
			wantCertified: true,
		},
		{
			name:   "fast signature",
			source: `{{ store.Direct("value") }}`,
		},
		{
			name:   "escaped function value",
			source: `{% call := store.Reflective %}{{ call("value").Name }}`,
		},
		{
			name:    "detached call",
			source:  `{% go store.Reflective("value") %}`,
			allowGo: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			code, err := BuildTemplate(fstest.Files{"index.txt": test.source}, "index.txt", Options{
				AllowGoStmt: test.allowGo,
				Globals: native.Declarations{
					"store": (*synchronousNativeFieldStore)(nil),
				},
			})
			if err != nil {
				t.Fatalf("build: %v", err)
			}
			var field, call bool
			for _, instruction := range code.Main.Body {
				field = field || instruction.Op == runtime.OpFieldNativeFunc
				call = call || instruction.Op == runtime.OpCallNativeValue
			}
			if field != test.wantCertified || call != test.wantCertified {
				t.Fatalf("certified field=%t call=%t, want %t", field, call, test.wantCertified)
			}
		})
	}
}

func TestEmitterUsesAuthenticatedFrameOnlyForImmediateInterfaceMethodCalls(t *testing.T) {
	tests := []struct {
		name          string
		source        string
		allowGo       bool
		wantCertified bool
	}{
		{
			name:          "immediate call",
			source:        `{% value, found := selector.Select("key") %}{{ value }}{{ found }}`,
			wantCertified: true,
		},
		{
			name:   "escaped method value",
			source: `{% selectValue := selector.Select %}{% value, found := selectValue("key") %}{{ value }}{{ found }}`,
		},
		{
			name:    "detached call",
			source:  `{% go selector.Select("key") %}`,
			allowGo: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			code, err := BuildTemplate(fstest.Files{"index.txt": test.source}, "index.txt", Options{
				AllowGoStmt: test.allowGo,
				Globals: native.Declarations{
					"selector": native.Synchronous((*synchronousNativeMethodSelector)(nil), "Select"),
				},
			})
			if err != nil {
				t.Fatalf("build: %v", err)
			}
			var method, call bool
			for _, instruction := range code.Main.Body {
				method = method || instruction.Op == runtime.OpMethodValueFrame
				call = call || instruction.Op == runtime.OpCallNativeFrameMethod
			}
			if method != test.wantCertified || call != test.wantCertified {
				t.Fatalf("certified method=%t call=%t, want %t", method, call, test.wantCertified)
			}
		})
	}
}
