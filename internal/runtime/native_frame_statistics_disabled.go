// Copyright 2026 The Scriggo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build !scriggo_native_frame_stats

package runtime

func recordNativeFrameCall(nativeFrameCallKind, bool) {}

// ResetNativeFrameCallStatistics resets compiled frame dispatch counters.
func ResetNativeFrameCallStatistics() {}

// ReadNativeFrameCallStatistics reads compiled frame dispatch counters.
func ReadNativeFrameCallStatistics() NativeFrameCallStatistics {
	return NativeFrameCallStatistics{}
}
