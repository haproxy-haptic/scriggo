// Copyright 2025 The Scriggo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime

import (
	"reflect"
	"testing"
)

// TestPooledVMClearsEveryWrittenRegister is the guard for the partial clear in
// putPooledVM.
//
// putPooledVM only clears up to vm.high, which is sound exactly while every
// writer to the register banks maintains it. If a writer stops — or a new one
// is added — the bank keeps references past the mark, the VM goes back to the
// pool holding them, and nothing else in the suite notices: the next user
// overwrites the registers it needs, so the render still produces the right
// answer while the garbage stays reachable.
//
// So this asserts the *whole* bank is zero rather than sampling, and it writes
// through the real setters rather than poking the slices directly.
func TestPooledVMClearsEveryWrittenRegister(t *testing.T) {
	cases := []struct {
		name  string
		write func(vm *VM)
	}{
		{
			name: "setGeneral and setString at a low frame",
			write: func(vm *VM) {
				for r := int8(1); r < 40; r++ {
					vm.setGeneral(r, reflect.ValueOf(&struct{ X int }{int(r)}))
					vm.setString(r, "held")
				}
			},
		},
		{
			name: "writes from a shifted frame reach higher indices",
			write: func(vm *VM) {
				vm.fp[2], vm.fp[3] = 100, 100
				for r := int8(1); r < 40; r++ {
					vm.setGeneral(r, reflect.ValueOf(&struct{ X int }{int(r)}))
					vm.setString(r, "held")
				}
				vm.fp[2], vm.fp[3] = 0, 0
			},
		},
		{
			name: "swapStack shifts values above anything set directly",
			write: func(vm *VM) {
				for r := int8(1); r < 20; r++ {
					vm.setGeneral(r, reflect.ValueOf(&struct{ X int }{int(r)}))
					vm.setString(r, "held")
				}
				a := [4]Addr{0, 0, 0, 0}
				b := [4]Addr{0, 0, 10, 10}
				vm.swapStack(&a, &b, StackShift{0, 0, 12, 12})
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			vm := getPooledVM(nil, nil)
			tc.write(vm)
			putPooledVM(vm)

			for i, v := range vm.regs.general {
				if v.IsValid() {
					t.Fatalf("general register %d still holds %v after release — "+
						"a writer is not maintaining vm.high, so the pooled VM "+
						"retains references", i, v)
				}
			}
			for i, s := range vm.regs.string {
				if s != "" {
					t.Fatalf("string register %d still holds %q after release — "+
						"a writer is not maintaining vm.high", i, s)
				}
			}
			if vm.high != [4]Addr{0, 0, 0, 0} {
				t.Fatalf("high-water not reset on release: %v", vm.high)
			}
			if vm.nativeEnv != nil {
				t.Fatal("native environment retained after release")
			}
		})
	}
}

// TestPooledVMPartialClearLeavesBankZeroed pins the inductive half of the
// invariant: a bank is zero above the high-water mark, so a VM that writes
// little does not have to pay for a VM that previously wrote a lot.
func TestPooledVMPartialClearLeavesBankZeroed(t *testing.T) {
	vm := getPooledVM(nil, nil)
	vm.fp[3] = 300
	for r := int8(1); r < 50; r++ {
		vm.setGeneral(r, reflect.ValueOf(&struct{ X int }{int(r)}))
	}
	vm.fp[3] = 0
	putPooledVM(vm)

	// Now a shallow user of the same VM.
	vm2 := getPooledVM(nil, nil)
	vm2.setGeneral(1, reflect.ValueOf(&struct{ X int }{1}))
	putPooledVM(vm2)

	for i, v := range vm2.regs.general {
		if v.IsValid() {
			t.Fatalf("register %d not zero after a shallow release — the "+
				"invariant that the bank is zero above the mark does not hold", i)
		}
	}
}
