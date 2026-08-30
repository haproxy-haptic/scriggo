// Copyright 2026 The Scriggo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build scriggo_native_frame_stats

package runtime

import "sync/atomic"

var nativeFrameCallCounters [5]atomic.Uint64

func recordNativeFrameCall(kind nativeFrameCallKind, authenticated bool) {
	index := int(kind) * 2
	if authenticated {
		index--
	}
	if index <= 0 || index >= len(nativeFrameCallCounters) {
		return
	}
	nativeFrameCallCounters[index].Add(1)
}

// ResetNativeFrameCallStatistics resets compiled frame dispatch counters.
func ResetNativeFrameCallStatistics() {
	for index := range nativeFrameCallCounters {
		nativeFrameCallCounters[index].Store(0)
	}
}

// ReadNativeFrameCallStatistics reads compiled frame dispatch counters.
func ReadNativeFrameCallStatistics() NativeFrameCallStatistics {
	return NativeFrameCallStatistics{
		FunctionFieldAuthenticated:   nativeFrameCallCounters[1].Load(),
		FunctionFieldFallback:        nativeFrameCallCounters[2].Load(),
		InterfaceMethodAuthenticated: nativeFrameCallCounters[3].Load(),
		InterfaceMethodFallback:      nativeFrameCallCounters[4].Load(),
	}
}
