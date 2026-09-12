// Copyright 2026 The Scriggo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package misc

import (
	"strings"
	"testing"

	"gitlab.com/haproxy-haptic/scriggo"
	"gitlab.com/haproxy-haptic/scriggo/native"
)

func TestAppendNilNativeResult(t *testing.T) {
	for name, missing := range map[string]any{
		"reflective":  func() any { return nil },
		"synchronous": native.Synchronous(func() any { return nil }),
	} {
		t.Run(name, func(t *testing.T) {
			const source = `{% var a = []any{"before"} %}{% a = append(a, missing()) %}{{ len(a) }}:{{ a[0] }}:{{ a[1] == nil }}`
			template, err := scriggo.BuildTemplate(scriggo.Files{"index.txt": []byte(source)}, "index.txt", &scriggo.BuildOptions{
				Globals: native.Declarations{"missing": missing},
			})
			if err != nil {
				t.Fatal(err)
			}
			for range 3 {
				var output strings.Builder
				if err := template.Run(&output, nil, nil); err != nil {
					t.Fatal(err)
				}
				if got := output.String(); got != "2:before:true" {
					t.Fatalf("output = %q, want 2:before:true", got)
				}
			}
		})
	}
}
