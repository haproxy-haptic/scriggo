// Copyright 2026 The Scriggo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"sync/atomic"
	"time"

	"github.com/haproxy-haptic/scriggo/native"
)

var (
	ErrVectorInvalidConfiguration = errors.New("invalid vector configuration")
	ErrVectorInactive             = errors.New("vector execution is inactive")
	ErrVectorGenerationRevoked    = errors.New("vector invocation generation is revoked")
)

type VectorBinding struct {
	Name          string
	Type          reflect.Type
	Column        reflect.Value
	Slots         []reflect.Value
	GlobalIndexes []int
}

type vectorLoadedRange struct {
	start    int
	end      int
	columns  []reflect.Value
	contexts []context.Context
}

type VectorState struct {
	mu             sync.Mutex
	drained        *sync.Cond
	authority      *native.VectorAuthority
	count          int
	bindings       []VectorBinding
	contexts       []context.Context
	deferred       bool
	loaded         int
	ranges         []vectorLoadedRange
	active         int
	activeCtx      context.Context
	cancel         context.CancelFunc
	generation     uint64
	inflight       int
	revoking       bool
	closed         bool
	switchCtx      func(context.Context)
	vmNative       bool
	globalRefs     []int16
	values         []reflect.Value
	valuesLive     atomic.Bool
	boundary       *native.VectorBoundary
	lifecycle      native.VectorLifecycle
	fiberBoundary  *native.VectorFiberBoundary
	fiberLifecycle native.VectorFiberLifecycle
	fiberActive    int
	rowContext     context.Context
	next           int
	failure        int
	aborted        bool
}

type vectorInvocationLease struct {
	state      *VectorState
	generation uint64
	ctx        context.Context

	mu        sync.Mutex
	drained   *sync.Cond
	users     int
	closing   bool
	closeOnce sync.Once
}

func NewVectorState(
	authority *native.VectorAuthority,
	count int,
	bindings []VectorBinding,
	contexts []context.Context,
) (*VectorState, error) {
	if authority == nil || count <= 0 || len(contexts) != count {
		return nil, ErrVectorInvalidConfiguration
	}
	for index, ctx := range contexts {
		if ctx == nil {
			return nil, fmt.Errorf("%w: context %d is nil", ErrVectorInvalidConfiguration, index)
		}
	}
	for _, binding := range bindings {
		if binding.Name == "" || binding.Column.Kind() != reflect.Slice ||
			binding.Column.Len() != count || len(binding.Slots) == 0 {
			return nil, fmt.Errorf("%w: binding %q is invalid", ErrVectorInvalidConfiguration, binding.Name)
		}
		elementType := binding.Column.Type().Elem()
		if binding.Type != nil && binding.Type != elementType {
			return nil, fmt.Errorf("%w: binding %q has an incompatible type", ErrVectorInvalidConfiguration, binding.Name)
		}
		for _, slot := range binding.Slots {
			if !slot.IsValid() || !slot.CanSet() || !elementType.AssignableTo(slot.Type()) {
				return nil, fmt.Errorf("%w: binding %q has an incompatible slot", ErrVectorInvalidConfiguration, binding.Name)
			}
		}
	}
	state := &VectorState{
		authority:   authority,
		count:       count,
		bindings:    bindings,
		contexts:    append([]context.Context(nil), contexts...),
		loaded:      count,
		active:      -1,
		fiberActive: -1,
	}
	state.drained = sync.NewCond(&state.mu)
	return state, nil
}

func NewDeferredVectorState(
	authority *native.VectorAuthority,
	count int,
	bindings []VectorBinding,
) (*VectorState, error) {
	if authority == nil || count <= 0 || len(bindings) == 0 {
		return nil, ErrVectorInvalidConfiguration
	}
	seen := make(map[string]struct{}, len(bindings))
	for _, binding := range bindings {
		if binding.Name == "" || binding.Type == nil || binding.Column.IsValid() || len(binding.Slots) == 0 {
			return nil, fmt.Errorf("%w: deferred binding %q is invalid", ErrVectorInvalidConfiguration, binding.Name)
		}
		if _, duplicate := seen[binding.Name]; duplicate {
			return nil, fmt.Errorf("%w: deferred binding %q is duplicated", ErrVectorInvalidConfiguration, binding.Name)
		}
		seen[binding.Name] = struct{}{}
		for _, slot := range binding.Slots {
			if !slot.IsValid() || !slot.CanSet() || slot.Type() != binding.Type {
				return nil, fmt.Errorf("%w: deferred binding %q has an incompatible slot", ErrVectorInvalidConfiguration, binding.Name)
			}
		}
	}
	state := &VectorState{
		authority:   authority,
		count:       count,
		bindings:    bindings,
		deferred:    true,
		active:      -1,
		fiberActive: -1,
	}
	state.drained = sync.NewCond(&state.mu)
	return state, nil
}

func NewVMNativeVectorState(
	authority *native.VectorAuthority,
	count int,
	bindings []VectorBinding,
	contexts []context.Context,
) (*VectorState, error) {
	state, err := NewVectorState(authority, count, bindings, contexts)
	if err != nil {
		return nil, err
	}
	if err := state.enableVMNative(); err != nil {
		state.Close()
		return nil, err
	}
	return state, nil
}

func NewVMNativeDeferredVectorState(
	authority *native.VectorAuthority,
	count int,
	bindings []VectorBinding,
) (*VectorState, error) {
	state, err := NewDeferredVectorState(authority, count, bindings)
	if err != nil {
		return nil, err
	}
	if err := state.enableVMNative(); err != nil {
		state.Close()
		return nil, err
	}
	return state, nil
}

func (state *VectorState) enableVMNative() error {
	maxIndex := -1
	for _, binding := range state.bindings {
		if len(binding.GlobalIndexes) == 0 {
			return fmt.Errorf("%w: binding %q has no global indexes", ErrVectorInvalidConfiguration, binding.Name)
		}
		for _, index := range binding.GlobalIndexes {
			if index < 0 {
				return fmt.Errorf("%w: binding %q has an invalid global index", ErrVectorInvalidConfiguration, binding.Name)
			}
			maxIndex = max(maxIndex, index)
		}
	}
	state.globalRefs = make([]int16, maxIndex+1)
	for index := range state.globalRefs {
		state.globalRefs[index] = -1
	}
	for bindingIndex, binding := range state.bindings {
		if bindingIndex > int(^uint16(0)>>1) {
			return ErrVectorInvalidConfiguration
		}
		for _, globalIndex := range binding.GlobalIndexes {
			if state.globalRefs[globalIndex] >= 0 {
				return fmt.Errorf("%w: global %d belongs to more than one binding", ErrVectorInvalidConfiguration, globalIndex)
			}
			state.globalRefs[globalIndex] = int16(bindingIndex)
		}
	}
	state.values = make([]reflect.Value, len(state.bindings))
	state.vmNative = true
	return nil
}

func (state *VectorState) SetVMNativeBoundary(
	boundary *native.VectorBoundary,
	lifecycle native.VectorLifecycle,
) error {
	state.mu.Lock()
	defer state.mu.Unlock()
	if !state.vmNative || state.closed || state.active >= 0 || state.boundary != nil ||
		boundary == nil || !boundary.Valid() || nilVectorLifecycle(lifecycle) {
		return ErrVectorInvalidConfiguration
	}
	state.boundary = boundary
	state.lifecycle = lifecycle
	state.failure = -1
	return nil
}

func (state *VectorState) VMNativeBoundary() *native.VectorBoundary {
	state.mu.Lock()
	defer state.mu.Unlock()
	return state.boundary
}

func (state *VectorState) SetVMNativeFiberBoundary(
	boundary *native.VectorFiberBoundary,
	lifecycle native.VectorFiberLifecycle,
) error {
	state.mu.Lock()
	defer state.mu.Unlock()
	if !state.vmNative || state.closed || state.active >= 0 || state.fiberBoundary != nil ||
		boundary == nil || !boundary.Valid() || nilVectorFiberLifecycle(lifecycle) {
		return ErrVectorInvalidConfiguration
	}
	state.fiberBoundary = boundary
	state.fiberLifecycle = lifecycle
	return nil
}

func (state *VectorState) VMNativeFiberBoundary() *native.VectorFiberBoundary {
	state.mu.Lock()
	defer state.mu.Unlock()
	return state.fiberBoundary
}

// releaseFailedFiberTransition clears the flags a fiber begin or end sets before
// it hands control to the lifecycle, so an error return does not leave the
// state mid-transition.
//
// It deliberately does NOT mark the state aborted: CloseWithError only notifies
// the outer lifecycle while !aborted, so setting it here would swallow the
// Abort for the active row. Revoking is what has to be undone — it is why every
// later call answered ErrVectorInvalidConfiguration, which only looked harmless
// because the VM turns these errors into a panic that Template.run converts
// into CloseWithError. A direct caller got no such rescue.
func (state *VectorState) releaseFailedFiberTransition() {
	state.mu.Lock()
	state.revoking = false
	state.fiberActive = -1
	state.mu.Unlock()
}

func (state *VectorState) BeginFiberVMNative(
	boundary *native.VectorFiberBoundary,
	source, child int,
) error {
	state.mu.Lock()
	if state.fiberBoundary == nil || boundary != state.fiberBoundary || state.fiberLifecycle == nil ||
		state.aborted || state.active != source || state.fiberActive >= 0 || child < 0 {
		state.mu.Unlock()
		return ErrVectorInvalidConfiguration
	}
	state.fiberActive = child
	state.revoking = true
	for state.inflight != 0 {
		state.drained.Wait()
	}
	state.generation++
	cancel := state.cancel
	state.cancel = nil
	state.activeCtx = nil
	lifecycle := state.fiberLifecycle
	switchCtx := state.switchCtx
	state.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if switchCtx != nil {
		switchCtx(nil)
	}

	childContext, err := lifecycle.BeginChild(source, child)
	if err != nil {
		state.releaseFailedFiberTransition()
		return err
	}
	if childContext == nil {
		state.releaseFailedFiberTransition()
		return ErrVectorInvalidConfiguration
	}
	if err := childContext.Err(); err != nil {
		state.releaseFailedFiberTransition()
		return err
	}

	state.mu.Lock()
	if state.closed || state.aborted || state.active != source || state.fiberActive != child ||
		state.rowContext == nil || !state.revoking || state.activeCtx != nil || state.cancel != nil {
		state.mu.Unlock()
		return ErrVectorInvalidConfiguration
	}
	rawCtx, nextCancel := context.WithCancel(childContext)
	state.cancel = nextCancel
	state.activeCtx = &vectorContext{Context: rawCtx, state: state, generation: state.generation}
	state.revoking = false
	activeCtx := state.activeCtx
	switchCtx = state.switchCtx
	state.mu.Unlock()
	if switchCtx != nil {
		switchCtx(activeCtx)
	}
	return nil
}

func (state *VectorState) EndFiberVMNative(
	boundary *native.VectorFiberBoundary,
	source, child int,
) error {
	state.mu.Lock()
	if state.fiberBoundary == nil || boundary != state.fiberBoundary || state.fiberLifecycle == nil ||
		state.aborted || state.active != source || state.fiberActive != child {
		state.mu.Unlock()
		return ErrVectorInvalidConfiguration
	}
	lifecycle := state.fiberLifecycle
	state.revoking = true
	for state.inflight != 0 {
		state.drained.Wait()
	}
	state.generation++
	cancel := state.cancel
	state.cancel = nil
	state.activeCtx = nil
	switchCtx := state.switchCtx
	state.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if switchCtx != nil {
		switchCtx(nil)
	}
	if err := lifecycle.EndChild(source, child); err != nil {
		state.releaseFailedFiberTransition()
		return err
	}

	state.mu.Lock()
	if state.closed || state.aborted || state.active != source || state.fiberActive != child ||
		state.rowContext == nil || !state.revoking || state.activeCtx != nil || state.cancel != nil {
		state.mu.Unlock()
		return ErrVectorInvalidConfiguration
	}
	rawCtx, nextCancel := context.WithCancel(state.rowContext)
	state.cancel = nextCancel
	state.activeCtx = &vectorContext{Context: rawCtx, state: state, generation: state.generation}
	state.fiberActive = -1
	state.revoking = false
	activeCtx := state.activeCtx
	switchCtx = state.switchCtx
	state.mu.Unlock()
	if switchCtx != nil {
		switchCtx(activeCtx)
	}
	return nil
}

func (state *VectorState) BeginVMNative(boundary *native.VectorBoundary, index int) error {
	state.mu.Lock()
	if state.boundary == nil || boundary != state.boundary || state.lifecycle == nil ||
		state.aborted || state.active >= 0 || index != state.next {
		state.mu.Unlock()
		return ErrVectorInvalidConfiguration
	}
	state.failure = index
	authority := state.authority
	lifecycle := state.lifecycle
	state.mu.Unlock()
	if err := state.Activate(authority, index); err != nil {
		return err
	}
	_, ctx := state.captureContext()
	if ctx == nil {
		return ErrVectorInvalidConfiguration
	}
	return lifecycle.Begin(ctx, index)
}

func (state *VectorState) EndVMNative(boundary *native.VectorBoundary, index int) error {
	state.mu.Lock()
	if state.boundary == nil || boundary != state.boundary || state.lifecycle == nil ||
		state.aborted || index != state.next || state.active != index || state.fiberActive >= 0 {
		state.mu.Unlock()
		return ErrVectorInvalidConfiguration
	}
	authority := state.authority
	lifecycle := state.lifecycle
	state.mu.Unlock()
	if err := lifecycle.Finish(index); err != nil {
		return err
	}
	if err := state.Revoke(authority); err != nil {
		return err
	}
	if err := lifecycle.Commit(index); err != nil {
		return err
	}
	state.mu.Lock()
	state.next++
	state.failure = -1
	state.mu.Unlock()
	return nil
}

func nilVectorLifecycle(lifecycle native.VectorLifecycle) bool {
	if lifecycle == nil {
		return true
	}
	value := reflect.ValueOf(lifecycle)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}

func nilVectorFiberLifecycle(lifecycle native.VectorFiberLifecycle) bool {
	if lifecycle == nil {
		return true
	}
	value := reflect.ValueOf(lifecycle)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}

func (state *VectorState) VMNativeCompletionError() error {
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.boundary == nil || state.aborted {
		return nil
	}
	if state.active >= 0 || state.fiberActive >= 0 || state.next != state.count {
		return errors.New("vector VM-native lifecycle did not complete")
	}
	return nil
}

func (state *VectorState) valueForGlobal(global int16) (reflect.Value, bool, error) {
	if !state.vmNative || global < 0 || int(global) >= len(state.globalRefs) {
		return reflect.Value{}, false, nil
	}
	binding := state.globalRefs[global]
	if binding < 0 {
		return reflect.Value{}, false, nil
	}
	if !state.valuesLive.Load() {
		return reflect.Value{}, true, ErrVectorInactive
	}
	return state.values[binding], true, nil
}

func (state *VectorState) hasGlobal(global int16) bool {
	return state.vmNative && global >= 0 && int(global) < len(state.globalRefs) && state.globalRefs[global] >= 0
}

func (state *VectorState) LoadRange(
	authority *native.VectorAuthority,
	start int,
	bindings map[string]any,
	contexts []context.Context,
) error {
	return state.loadRange(authority, start, bindings, contexts, false)
}

func (state *VectorState) LoadOwnedRange(
	authority *native.VectorAuthority,
	start int,
	bindings map[string]any,
	contexts []context.Context,
) error {
	return state.loadRange(authority, start, bindings, contexts, true)
}

func (state *VectorState) loadRange(
	authority *native.VectorAuthority,
	start int,
	bindings map[string]any,
	contexts []context.Context,
	owned bool,
) error {
	state.mu.Lock()
	defer state.mu.Unlock()
	if !state.deferred || state.closed || state.revoking || state.active >= 0 || state.inflight != 0 ||
		authority == nil || authority != state.authority || start != state.loaded || len(contexts) == 0 ||
		start < 0 || start > state.count || len(contexts) > state.count-start {
		return ErrVectorInvalidConfiguration
	}
	if len(bindings) != len(state.bindings) {
		return fmt.Errorf("%w: deferred range has %d bindings, want %d", ErrVectorInvalidConfiguration, len(bindings), len(state.bindings))
	}
	ownedContexts := contexts
	if !owned {
		ownedContexts = append([]context.Context(nil), contexts...)
	}
	for index, ctx := range ownedContexts {
		if ctx == nil {
			return fmt.Errorf("%w: deferred context %d is nil", ErrVectorInvalidConfiguration, start+index)
		}
	}
	columns := make([]reflect.Value, len(state.bindings))
	for index, binding := range state.bindings {
		column, exists := bindings[binding.Name]
		if !exists {
			return fmt.Errorf("%w: deferred binding %q is missing", ErrVectorInvalidConfiguration, binding.Name)
		}
		value := reflect.ValueOf(column)
		if !value.IsValid() || value.Kind() != reflect.Slice || value.Len() != len(contexts) {
			return fmt.Errorf("%w: deferred binding %q must be a concrete slice of length %d", ErrVectorInvalidConfiguration, binding.Name, len(contexts))
		}
		if owned {
			if value.Type().Elem() != binding.Type {
				return fmt.Errorf(
					"%w: owned deferred binding %q has element type %s, want %s",
					ErrVectorInvalidConfiguration,
					binding.Name,
					value.Type().Elem(),
					binding.Type,
				)
			}
			columns[index] = value
			continue
		}
		cloned, err := ownVectorColumn(binding.Name, value, binding.Type, len(contexts))
		if err != nil {
			return err
		}
		columns[index] = cloned
	}
	for name := range bindings {
		if _, exists := state.binding(name); !exists {
			return fmt.Errorf("%w: deferred binding %q is unexpected", ErrVectorInvalidConfiguration, name)
		}
	}
	end := start + len(contexts)
	state.ranges = append(state.ranges, vectorLoadedRange{
		start: start, end: end, columns: columns, contexts: ownedContexts,
	})
	state.loaded = end
	return nil
}

func (state *VectorState) binding(name string) (VectorBinding, bool) {
	for _, binding := range state.bindings {
		if binding.Name == name {
			return binding, true
		}
	}
	return VectorBinding{}, false
}

func ownVectorColumn(name string, column reflect.Value, variableType reflect.Type, count int) (reflect.Value, error) {
	elementType := column.Type().Elem()
	direct := elementType == variableType
	indirect := elementType.Kind() == reflect.Pointer && elementType.Elem().AssignableTo(variableType)
	if !direct && !indirect {
		return reflect.Value{}, fmt.Errorf(
			"%w: deferred binding %q has element type %s, want %s or a pointer to it",
			ErrVectorInvalidConfiguration,
			name,
			elementType,
			variableType,
		)
	}
	owned := reflect.MakeSlice(reflect.SliceOf(variableType), count, count)
	for index := range count {
		value := column.Index(index)
		if indirect {
			if value.IsNil() {
				return reflect.Value{}, fmt.Errorf(
					"%w: deferred binding %q item %d is a nil pointer",
					ErrVectorInvalidConfiguration,
					name,
					index,
				)
			}
			value = value.Elem()
		}
		owned.Index(index).Set(value)
	}
	return owned, nil
}

func (state *VectorState) Activate(authority *native.VectorAuthority, index int) error {
	state.mu.Lock()
	if state.closed || state.revoking || state.inflight != 0 || authority == nil || authority != state.authority {
		state.mu.Unlock()
		return ErrVectorInvalidConfiguration
	}
	if state.active >= 0 {
		state.mu.Unlock()
		return fmt.Errorf("vector index %d is already active", state.active)
	}
	if index < 0 || index >= state.count || state.deferred && index >= state.loaded {
		state.mu.Unlock()
		return fmt.Errorf("vector index %d is out of range", index)
	}
	var itemContext context.Context
	columns := make([]reflect.Value, len(state.bindings))
	if state.deferred {
		var loaded *vectorLoadedRange
		for rangeIndex := range state.ranges {
			candidate := &state.ranges[rangeIndex]
			if index >= candidate.start && index < candidate.end {
				loaded = candidate
				break
			}
		}
		if loaded == nil {
			state.mu.Unlock()
			return fmt.Errorf("vector index %d is not loaded", index)
		}
		itemContext = loaded.contexts[index-loaded.start]
		for bindingIndex := range state.bindings {
			columns[bindingIndex] = loaded.columns[bindingIndex].Index(index - loaded.start)
		}
	} else {
		itemContext = state.contexts[index]
	}
	if err := itemContext.Err(); err != nil {
		state.mu.Unlock()
		return err
	}
	for bindingIndex, binding := range state.bindings {
		value := columns[bindingIndex]
		if !state.deferred {
			value = binding.Column.Index(index)
		}
		if state.vmNative {
			state.values[bindingIndex] = value
		} else {
			for _, slot := range binding.Slots {
				slot.Set(value)
			}
		}
	}
	if state.vmNative {
		state.valuesLive.Store(true)
	}
	state.generation++
	state.active = index
	state.rowContext = itemContext
	rawCtx, cancel := context.WithCancel(itemContext)
	state.cancel = cancel
	state.activeCtx = &vectorContext{
		Context:    rawCtx,
		state:      state,
		generation: state.generation,
	}
	activeCtx := state.activeCtx
	switchCtx := state.switchCtx
	state.mu.Unlock()
	if switchCtx != nil {
		switchCtx(activeCtx)
	}
	return nil
}

func (state *VectorState) Revoke(authority *native.VectorAuthority) error {
	state.mu.Lock()
	if state.closed || authority == nil || authority != state.authority {
		state.mu.Unlock()
		return ErrVectorInvalidConfiguration
	}
	if state.active < 0 || state.fiberActive >= 0 {
		state.mu.Unlock()
		return ErrVectorInactive
	}
	state.revoking = true
	for state.inflight != 0 {
		state.drained.Wait()
	}
	state.clearBindings()
	state.active = -1
	state.fiberActive = -1
	state.rowContext = nil
	state.generation++
	state.revoking = false
	cancel := state.cancel
	state.cancel = nil
	state.activeCtx = nil
	switchCtx := state.switchCtx
	state.mu.Unlock()
	cancel()
	if switchCtx != nil {
		switchCtx(nil)
	}
	return nil
}

func (state *VectorState) Close() {
	state.close(nil)
}

func (state *VectorState) CloseWithError(cause error) {
	state.close(cause)
}

func (state *VectorState) close(cause error) {
	state.mu.Lock()
	state.revoking = true
	for state.inflight != 0 {
		state.drained.Wait()
	}
	state.clearBindings()
	state.active = -1
	state.fiberActive = -1
	state.rowContext = nil
	state.generation++
	cancel := state.cancel
	state.cancel = nil
	state.activeCtx = nil
	state.closed = true
	state.revoking = false
	state.bindings = nil
	state.contexts = nil
	state.ranges = nil
	state.globalRefs = nil
	state.values = nil
	state.fiberBoundary = nil
	state.fiberLifecycle = nil
	state.authority = nil
	lifecycle := state.lifecycle
	failure := state.failure
	abort := cause != nil && lifecycle != nil && !state.aborted
	state.aborted = state.aborted || abort
	state.boundary = nil
	state.lifecycle = nil
	switchCtx := state.switchCtx
	state.switchCtx = nil
	state.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if switchCtx != nil {
		switchCtx(nil)
	}
	if abort {
		lifecycle.Abort(failure, cause)
	}
}

func (state *VectorState) clearSlots() {
	for _, binding := range state.bindings {
		for _, slot := range binding.Slots {
			slot.SetZero()
		}
	}
}

func (state *VectorState) clearBindings() {
	if !state.vmNative {
		state.clearSlots()
		return
	}
	state.valuesLive.Store(false)
	clear(state.values)
}

func (state *VectorState) capture() uint64 {
	state.mu.Lock()
	generation := state.generation
	active := state.active
	closed := state.closed
	state.mu.Unlock()
	if closed || active < 0 {
		return 0
	}
	return generation
}

func (state *VectorState) captureContext() (uint64, context.Context) {
	state.mu.Lock()
	generation := state.generation
	active := state.active
	activeCtx := state.activeCtx
	closed := state.closed
	state.mu.Unlock()
	if closed || active < 0 {
		return 0, nil
	}
	return generation, activeCtx
}

func (state *VectorState) setContextSwitch(switchCtx func(context.Context)) {
	state.mu.Lock()
	state.switchCtx = switchCtx
	state.mu.Unlock()
}

func (state *VectorState) acquire(generation uint64) error {
	state.mu.Lock()
	if generation == 0 || state.closed || state.revoking || state.active < 0 ||
		state.generation != generation {
		state.mu.Unlock()
		return ErrVectorGenerationRevoked
	}
	state.inflight++
	state.mu.Unlock()
	return nil
}

func (state *VectorState) acquireInvocation(generation uint64) (*vectorInvocationLease, error) {
	state.mu.Lock()
	if generation == 0 || state.closed || state.revoking || state.active < 0 ||
		state.generation != generation {
		state.mu.Unlock()
		return nil, ErrVectorGenerationRevoked
	}
	activeCtx, ok := state.activeCtx.(*vectorContext)
	if !ok || activeCtx.generation != generation {
		state.mu.Unlock()
		return nil, ErrVectorInvalidConfiguration
	}
	state.inflight++
	lease := &vectorInvocationLease{
		state:      state,
		generation: generation,
		ctx:        activeCtx.Context,
	}
	lease.drained = sync.NewCond(&lease.mu)
	state.mu.Unlock()
	return lease, nil
}

func (state *VectorState) release() {
	state.mu.Lock()
	state.inflight--
	if state.inflight < 0 {
		state.mu.Unlock()
		panic("negative vector invocation count")
	}
	if state.inflight == 0 {
		state.drained.Broadcast()
	}
	state.mu.Unlock()
}

func (lease *vectorInvocationLease) acquireUse() error {
	lease.mu.Lock()
	if lease.closing {
		lease.mu.Unlock()
		return ErrVectorGenerationRevoked
	}
	lease.users++
	lease.mu.Unlock()
	return nil
}

func (lease *vectorInvocationLease) releaseUse() {
	lease.mu.Lock()
	lease.users--
	if lease.users < 0 {
		lease.mu.Unlock()
		panic("negative vector invocation lease count")
	}
	if lease.users == 0 {
		lease.drained.Broadcast()
	}
	lease.mu.Unlock()
}

func (lease *vectorInvocationLease) close() {
	lease.closeOnce.Do(func() {
		lease.mu.Lock()
		lease.closing = true
		for lease.users != 0 {
			lease.drained.Wait()
		}
		lease.mu.Unlock()
		lease.state.release()
	})
}

func acquireVectorInvocation(
	state *VectorState,
	generation uint64,
	parent context.Context,
) (*vectorInvocationLease, context.Context, func(), error) {
	if invocationCtx, ok := parent.(*vectorInvocationContext); ok &&
		invocationCtx.lease.state == state && invocationCtx.lease.generation == generation {
		if err := invocationCtx.lease.acquireUse(); err == nil {
			return invocationCtx.lease, invocationCtx.Context, invocationCtx.lease.releaseUse, nil
		}
	}
	lease, err := state.acquireInvocation(generation)
	if err != nil {
		return nil, nil, nil, err
	}
	base := lease.ctx
	switch parentCtx := parent.(type) {
	case *vectorContext:
		if parentCtx.state == state && parentCtx.generation == generation {
			base = parentCtx.Context
		}
	case *vectorInvocationContext:
		if parentCtx.lease.state == state && parentCtx.lease.generation == generation {
			base = parentCtx.Context
		}
	}
	return lease, base, lease.close, nil
}

type vectorContext struct {
	context.Context
	state      *VectorState
	generation uint64
}

type vectorInvocationContext struct {
	context.Context
	lease *vectorInvocationLease
}

func (ctx *vectorContext) Deadline() (time.Time, bool) {
	if err := ctx.state.acquire(ctx.generation); err != nil {
		return time.Time{}, false
	}
	defer ctx.state.release()
	return ctx.Context.Deadline()
}

func (ctx *vectorContext) Err() error {
	if err := ctx.state.acquire(ctx.generation); err != nil {
		return err
	}
	defer ctx.state.release()
	return ctx.Context.Err()
}

func (ctx *vectorContext) Value(key any) any {
	if err := ctx.state.acquire(ctx.generation); err != nil {
		return nil
	}
	defer ctx.state.release()
	return ctx.Context.Value(key)
}

func (ctx *vectorInvocationContext) Deadline() (time.Time, bool) {
	if err := ctx.lease.acquireUse(); err != nil {
		return time.Time{}, false
	}
	defer ctx.lease.releaseUse()
	return ctx.Context.Deadline()
}

func (ctx *vectorInvocationContext) Err() error {
	if err := ctx.lease.acquireUse(); err != nil {
		return err
	}
	defer ctx.lease.releaseUse()
	return ctx.Context.Err()
}

func (ctx *vectorInvocationContext) Value(key any) any {
	if err := ctx.lease.acquireUse(); err != nil {
		return nil
	}
	defer ctx.lease.releaseUse()
	return ctx.Context.Value(key)
}
