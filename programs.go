// Copyright 2019 The Scriggo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package scriggo

import (
	"context"
	"errors"
	"io/fs"
	"reflect"

	"gitlab.com/haproxy-haptic/scriggo/ast"
	"gitlab.com/haproxy-haptic/scriggo/internal/compiler"
	"gitlab.com/haproxy-haptic/scriggo/internal/runtime"
	"gitlab.com/haproxy-haptic/scriggo/native"
)

// BuildOptions contains options for building programs and templates.
type BuildOptions struct {

	// AllowGoStmt, when true, allows the use of the go statement.
	AllowGoStmt bool

	// Packages is a package importer that makes native packages available
	// in programs and templates through the import statement.
	Packages native.Importer

	// UnexpandedTransformer transforms an unexpanded AST.
	// If non-nil, it is invoked on each parsed file before expansion.
	//
	// Used for templates only.
	UnexpandedTransformer func(tree *ast.Tree) error

	// ExpandedTransformer transforms the expanded AST.
	// If non-nil, it is invoked on the expanded tree before type checking.
	//
	// Used for templates only.
	ExpandedTransformer func(tree *ast.Tree) error

	// NoParseShortShowStmt, when true, don't parse the short show statements.
	//
	// Used for templates only.
	NoParseShortShowStmt bool

	// MarkdownConverter converts a Markdown source code to HTML.
	//
	// Used for templates only.
	MarkdownConverter Converter

	// Globals declares constants, types, variables, functions and packages
	// that are accessible from the code in the template.
	//
	// Used for templates only.
	Globals native.Declarations

	// EnableProfiling enables profiling instrumentation in the compiled template.
	// When true, the runtime will collect timing data for function calls,
	// macros, and includes during execution.
	//
	// Used for templates only.
	EnableProfiling bool

	// IsolateRootRenderState makes each render expression in the root template
	// reinitialize package variables reachable through the rendered file.
	// Nested renders share the state of their enclosing root render.
	//
	// Used for templates only.
	IsolateRootRenderState bool
}

// PrintFunc represents a function that prints the arguments of the print and
// println builtins.
type PrintFunc func(any)

// MutationOperation identifies a template-language mutation.
type MutationOperation = runtime.MutationOperation

const (
	MutationSetVariable    = runtime.MutationSetVariable
	MutationSetPointer     = runtime.MutationSetPointer
	MutationSetField       = runtime.MutationSetField
	MutationSetMapIndex    = runtime.MutationSetMapIndex
	MutationSetIndex       = runtime.MutationSetIndex
	MutationDelete         = runtime.MutationDelete
	MutationClear          = runtime.MutationClear
	MutationAppend         = runtime.MutationAppend
	MutationCopy           = runtime.MutationCopy
	MutationCloseChannel   = runtime.MutationCloseChannel
	MutationSendChannel    = runtime.MutationSendChannel
	MutationReceiveChannel = runtime.MutationReceiveChannel
	MutationSelectChannel  = runtime.MutationSelectChannel
)

// Mutation describes storage immediately before a template mutates it.
// Target is the variable, pointer, struct, map, slice, array, or channel whose
// reachable storage will change. An observer must treat Target as read-only.
type Mutation = runtime.Mutation

// NativeCall describes a native method immediately before Scriggo calls it or
// exposes it as a method value. Receiver must be treated as read-only by an
// observer. MethodValue reports the latter case.
type NativeCall = runtime.NativeCall

// ErrMutationObserverDetachedGo is returned before a detached Scriggo
// goroutine starts while ObserveMutation is set. Waited go-render operations
// remain available.
var ErrMutationObserverDetachedGo = runtime.ErrMutationObserverDetachedGo

// ErrMutationObserverChannelOperation is returned before a channel close,
// send, receive, or select while ObserveMutation is set.
var ErrMutationObserverChannelOperation = runtime.ErrMutationObserverChannelOperation

// ErrDeterministicPrint is returned before execution when deterministic mode
// finds a language-level print or println call.
var ErrDeterministicPrint = runtime.ErrDeterministicPrint

// ErrDeterministicMapKey is returned when deterministic mode encounters a map
// key whose identity has no stable, value-based order.
var ErrDeterministicMapKey = runtime.ErrDeterministicMapKey

// ErrDeterministicGo is returned before deterministic execution starts when
// the program contains a goroutine or parallel render operation.
var ErrDeterministicGo = runtime.ErrDeterministicGo

// ErrBatchDetachedWork is returned before batch execution starts when the
// program contains a goroutine or parallel render operation.
var ErrBatchDetachedWork = runtime.ErrBatchDetachedWork

// ErrBatchUncertifiedNative is returned before batch execution starts when a
// referenced native capability lacks a synchronous declaration certificate.
var ErrBatchUncertifiedNative = runtime.ErrBatchUncertifiedNative

// ErrVectorInvalidConfiguration reports a malformed or unauthenticated vector run.
var ErrVectorInvalidConfiguration = runtime.ErrVectorInvalidConfiguration

// ErrVectorInactive reports an activation lifecycle violation.
var ErrVectorInactive = runtime.ErrVectorInactive

// ErrVectorGenerationRevoked reports a callable invoked outside its vector item.
var ErrVectorGenerationRevoked = runtime.ErrVectorGenerationRevoked

// VectorRunOptions configure columnar globals for one template execution.
type VectorRunOptions struct {
	Authority        *native.VectorAuthority
	Count            int
	Bindings         map[string]any
	Contexts         []context.Context
	DeferredBindings []string
	VMNative         bool
	Boundary         *native.VectorBoundary
	Lifecycle        native.VectorLifecycle
	FiberBoundary    *native.VectorFiberBoundary
	FiberLifecycle   native.VectorFiberLifecycle
}

// RunOptions are the run options.
type RunOptions struct {
	// Deterministic orders map ranges by key and disables print, println,
	// goroutines, and parallel rendering. Functions containing those operations
	// and statically unsupported map key types are rejected before execution.
	// Dynamic pointer, channel, unsafe-pointer, and NaN map keys are rejected when
	// their range begins. Native functions and methods remain outside this policy.
	Deterministic bool

	// Vector installs concrete slice columns into every mutable global slot
	// with the corresponding name. A native.VectorEnv controller selects the
	// active element. Used for templates only.
	Vector *VectorRunOptions

	// NativeFunctionTrampolines provide direct paths for function values created
	// by native.MakeFunctionTrampoline. Other native functions use normal dispatch.
	NativeFunctionTrampolines []*native.FunctionTrampoline

	// Context is a context that can be read by native functions and methods
	// via the Context method of native.Env. Canceling the context, the
	// execution is terminated and the Run method returns Context.Err().
	Context context.Context

	// ChildContext derives the context exposed through native.Env in a child
	// execution from the context of its parent execution. It is called for
	// Scriggo functions invoked by native code and for executions started by a
	// go statement, including parallel template renders. The returned context
	// must derive from parent. Calls may occur concurrently.
	ChildContext func(parent context.Context) context.Context

	// ObserveMutation is called synchronously immediately before template code
	// mutates reachable storage. Returning an error prevents the mutation and
	// makes Run return that error. The Env carries the current child execution
	// context. Calls may occur concurrently.
	//
	// Native functions and methods are outside this observer. Clone or guard
	// mutable values before passing them to native code, or use
	// ObserveNativeCall for method receivers. Detached Scriggo go
	// statements return [ErrMutationObserverDetachedGo] before they start.
	// Channel operations return this function's error, or
	// [ErrMutationObserverChannelOperation] when it returns nil. A select reports
	// every channel candidate; an empty select has an invalid Target.
	ObserveMutation func(env native.Env, mutation Mutation) error

	// ObserveMutationContext is the non-escaping context-only form of ObserveMutation.
	ObserveMutationContext func(ctx context.Context, mutation Mutation) error

	// ObserveNativeCall is called synchronously before Scriggo calls a native
	// method or exposes it as a method value. Returning an error prevents the
	// access and makes Run return that error. It is not called for native
	// functions. Calls may occur concurrently.
	ObserveNativeCall func(env native.Env, call NativeCall) error

	// ObserveNativeCallContext is the non-escaping context-only form of ObserveNativeCall.
	ObserveNativeCallContext func(ctx context.Context, call NativeCall) error

	// BeforeNativeCall is called synchronously immediately before every native
	// function or method invocation. Returning an error prevents the call and
	// makes Run return that error. Calls may occur concurrently.
	BeforeNativeCall func(env native.Env) error

	// BeforeNativeCallContext is the non-escaping context-only form of BeforeNativeCall.
	BeforeNativeCallContext func(ctx context.Context) error

	// Print is called by the print and println builtins to print values.
	// If it is nil, the print and println builtins format their arguments as
	// expected and write the result to standard error.
	Print PrintFunc

	// Profile receives profiling data after execution completes.
	// Only populated if the template was built with EnableProfiling=true.
	// Caller must allocate: &scriggo.Profile{}
	//
	// Note: Native function calls executed as goroutines (via go statement)
	// are not included in the profile since their execution is asynchronous.
	//
	// Used for templates only. Ignored for programs.
	Profile *Profile

	// MaxParallelRenders limits the number of concurrent "go render" operations.
	// If zero or negative, defaults to 2*GOMAXPROCS.
	//
	// Used for templates only. Ignored for programs.
	MaxParallelRenders int

	// CollectSourceMap requests an output-to-source line map: after Run,
	// SourceSpans holds one span per contiguous run of rendered output,
	// attributing it to the template source position that produced it. This
	// is pure observation and does not change the rendered bytes, but adds
	// per-write bookkeeping, so leave it false for production rendering.
	//
	// Used for templates only. Ignored for programs.
	CollectSourceMap bool

	// SourceSpans is populated after Run when CollectSourceMap is true. The
	// spans are in output order and their Length fields sum to the output
	// size, so a caller can walk the rendered bytes and attribute each output
	// line to (Path, Line). Output produced by a parallel "{{ go … }}" render
	// is attributed as one span to the go-render call site.
	SourceSpans []SourceSpan
}

// SourceFrame is one level of a template include stack: the template registry
// key (e.g. "haproxy.cfg" for the main template, or a snippet/map/file name) and
// the 1-based source line active in it.
type SourceFrame struct {
	Path string
	Line int
}

// SourceSpan attributes a contiguous run of rendered output to the template
// include stack that produced it. See RunOptions.CollectSourceMap.
type SourceSpan struct {
	// Frames is the include stack, innermost first: Frames[0] is where the text
	// literally is, followed by the render/include callers up to the entry
	// template. Piped renders ("{{ render \"x\" | indent }}") resolve into x's
	// own source when the filter is line-preserving.
	Frames []SourceFrame
	// Length is the number of output bytes this span covers.
	Length int
	// IsText reports whether the span is literal template text (so Frames[0].Line
	// advances with each '\n' in the output) rather than a show-expression value.
	IsText bool
}

// Program is a program compiled with the [Build] function.
type Program struct {
	fn      *runtime.Function
	typeof  runtime.TypeOfFunc
	globals []compiler.Global
}

// Build builds a program from the package in the root of fsys with the given
// options.
//
// Current limitation: fsys can contain only one Go file in its root.
//
// If a build error occurs, it returns a [*BuildError].
func Build(fsys fs.FS, options *BuildOptions) (*Program, error) {
	co := compiler.Options{}
	if options != nil {
		co.AllowGoStmt = options.AllowGoStmt
		co.Importer = options.Packages
	}
	code, err := compiler.BuildProgram(fsys, co)
	if err != nil {
		if e, ok := err.(compiler.Error); ok {
			err = &BuildError{err: e}
		}
		return nil, err
	}
	return &Program{fn: code.Main, globals: code.Globals, typeof: code.TypeOf}, nil
}

// Disassemble disassembles the package with the given path and returns its
// assembly code. Native packages can not be disassembled.
func (p *Program) Disassemble(pkgPath string) ([]byte, error) {
	assemblies := compiler.Disassemble(p.fn, p.globals, 0)
	asm, ok := assemblies[pkgPath]
	if !ok {
		return nil, errors.New("scriggo: package path does not exist")
	}
	return asm, nil
}

// Run starts the program and waits for it to complete. It can be called
// concurrently by multiple goroutines.
//
// If the executed program panics, and it is not recovered, Run returns a
// [*PanicError].
//
// If the Stop method of native.Env is called, Run returns the argument passed
// to Stop.
//
// If the Fatal method of native.Env is called, Run panics with the argument
// passed to Fatal.
//
// If the context has been canceled, Run returns the error returned by the Err
// method of the context.
func (p *Program) Run(options *RunOptions) error {
	if options != nil && options.Vector != nil {
		return ErrVectorInvalidConfiguration
	}
	vm := runtime.NewVM()
	if options != nil {
		if options.Context != nil {
			vm.SetContext(options.Context)
		}
		if options.ChildContext != nil {
			vm.SetChildContext(options.ChildContext)
		}
		if options.ObserveMutation != nil {
			vm.SetMutationObserver(func(env native.Env, mutation runtime.Mutation) error {
				return options.ObserveMutation(env, Mutation{
					Operation: MutationOperation(mutation.Operation),
					Target:    mutation.Target,
					Path:      mutation.Path,
					Line:      mutation.Line,
				})
			})
		}
		if options.ObserveMutationContext != nil {
			vm.SetMutationContextObserver(runtime.MutationContextObserver(options.ObserveMutationContext))
		}
		if options.ObserveNativeCall != nil {
			vm.SetNativeCallObserver(func(env native.Env, call runtime.NativeCall) error {
				return options.ObserveNativeCall(env, NativeCall{
					Receiver:    call.Receiver,
					Method:      call.Method,
					MethodValue: call.MethodValue,
					Path:        call.Path,
					Line:        call.Line,
				})
			})
		}
		if options.ObserveNativeCallContext != nil {
			vm.SetNativeCallContextObserver(runtime.NativeCallContextObserver(options.ObserveNativeCallContext))
		}
		if options.BeforeNativeCall != nil {
			vm.SetBeforeNativeCall(runtime.BeforeNativeCall(options.BeforeNativeCall))
		}
		if options.BeforeNativeCallContext != nil {
			vm.SetBeforeNativeCallContext(runtime.BeforeNativeCallContext(options.BeforeNativeCallContext))
		}
		vm.SetDeterministic(options.Deterministic)
		if options.Print != nil {
			vm.SetPrint(runtime.PrintFunc(options.Print))
		}
	}
	err := vm.Run(p.fn, p.typeof, initPackageLevelVariables(p.globals))
	if err != nil {
		if p, ok := err.(*runtime.PanicError); ok {
			err = &PanicError{p}
		}
		return err
	}
	return nil
}

// initPackageLevelVariables initializes the package level variables and
// returns the values.
func initPackageLevelVariables(globals []compiler.Global) []reflect.Value {
	n := len(globals)
	if n == 0 {
		return nil
	}
	values := make([]reflect.Value, n)
	for i, global := range globals {
		if global.Value.IsValid() {
			values[i] = global.Value
		} else {
			values[i] = reflect.New(global.Type).Elem()
		}
	}
	return values
}
