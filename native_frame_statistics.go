// Copyright 2026 The Scriggo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package scriggo

import "gitlab.com/haproxy-haptic/scriggo/internal/runtime"

// NativeFrameCallStatistics reports compiled frame dispatch outcomes.
type NativeFrameCallStatistics = runtime.NativeFrameCallStatistics

// ResetNativeFrameCallStatistics resets compiled frame dispatch counters.
func ResetNativeFrameCallStatistics() {
	runtime.ResetNativeFrameCallStatistics()
}

// ReadNativeFrameCallStatistics reads compiled frame dispatch counters.
func ReadNativeFrameCallStatistics() NativeFrameCallStatistics {
	return runtime.ReadNativeFrameCallStatistics()
}
