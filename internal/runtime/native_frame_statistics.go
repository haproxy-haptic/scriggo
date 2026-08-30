// Copyright 2026 The Scriggo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime

type nativeFrameCallKind uint8

const (
	nativeFrameCallNone nativeFrameCallKind = iota
	nativeFrameCallFunctionField
	nativeFrameCallInterfaceMethod
)

// NativeFrameCallStatistics reports compiled frame dispatch outcomes.
type NativeFrameCallStatistics struct {
	FunctionFieldAuthenticated   uint64
	FunctionFieldFallback        uint64
	InterfaceMethodAuthenticated uint64
	InterfaceMethodFallback      uint64
}
