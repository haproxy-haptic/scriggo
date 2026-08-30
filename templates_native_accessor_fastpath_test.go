// Copyright 2026 The Scriggo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package scriggo

import (
	"strconv"
	"strings"
	"testing"

	"gitlab.com/haproxy-haptic/scriggo/native"
)

// The accessor-helper signatures reach the VM through a direct-call case whose
// register indexes are written by hand, and through an allowlist entry that
// makes that case reachable at all. Either half can be wrong on its own: a
// missing allowlist entry leaves the case dead (it measures as no speedup), and
// a wrong index reads the wrong register (it measures as a wrong value). These
// run each signature through a real template, with zero and with several
// variadic arguments, so both halves are pinned.
func TestNativeEnvAccessorHelpersTakeTheDirectCallPath(t *testing.T) {
	globals := native.Declarations{
		"envString": func(_ native.Env, v interface{}) string { return "S:" + v.(string) },
		"envInt":    func(_ native.Env, v interface{}) int { return len(v.(string)) },
		"envDig": func(_ native.Env, v interface{}, keys ...string) interface{} {
			return v.(string) + "|" + strings.Join(keys, ",")
		},
		"envDigStr": func(_ native.Env, v interface{}, first string, rest ...string) string {
			return v.(string) + "/" + first + "/" + strconv.Itoa(len(rest)) + ":" + strings.Join(rest, ",")
		},
	}

	tests := []struct {
		name   string
		source string
		want   string
	}{
		{"env+iface->string", `{{ envString("a") }}`, "S:a"},
		{"env+iface->int", `{{ envInt("abcd") }}`, "4"},
		{"env+iface+novariadic->iface", `{{ envDig("root") }}`, "root|"},
		{"env+iface+onevariadic->iface", `{{ envDig("root", "a") }}`, "root|a"},
		{"env+iface+manyvariadic->iface", `{{ envDig("root", "a", "b", "c") }}`, "root|a,b,c"},
		{"env+iface+str+novariadic->string", `{{ envDigStr("root", "k") }}`, "root/k/0:"},
		{"env+iface+str+onevariadic->string", `{{ envDigStr("root", "k", "x") }}`, "root/k/1:x"},
		{"env+iface+str+manyvariadic->string", `{{ envDigStr("root", "k", "x", "y", "z") }}`, "root/k/3:x,y,z"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			template, err := BuildTemplate(Files{"index.txt": []byte(tc.source)}, "index.txt",
				&BuildOptions{Globals: globals})
			if err != nil {
				t.Fatalf("build: %v", err)
			}
			var out strings.Builder
			if err := template.Run(&out, nil, nil); err != nil {
				t.Fatalf("run: %v", err)
			}
			if got := out.String(); got != tc.want {
				t.Fatalf("got %q, want %q — a direct-call case is reading the wrong register", got, tc.want)
			}
		})
	}
}
