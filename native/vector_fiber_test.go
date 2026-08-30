// Copyright 2026 The Scriggo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package native

import "testing"

func TestVectorFiberBoundaryCopyIsInvalid(t *testing.T) {
	original := NewVectorFiberBoundary()
	copy := *original
	if !original.Valid() {
		t.Fatal("new boundary is invalid")
	}
	if copy.Valid() {
		t.Fatal("copied boundary is valid")
	}
}
