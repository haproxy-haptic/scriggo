// Copyright 2019 The Scriggo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package scriggo

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"reflect"
	"slices"
	"sort"

	"gitlab.com/haproxy-haptic/scriggo/ast"
	"gitlab.com/haproxy-haptic/scriggo/internal/compiler"
	"gitlab.com/haproxy-haptic/scriggo/internal/compiler/types"
	"gitlab.com/haproxy-haptic/scriggo/internal/runtime"
	"gitlab.com/haproxy-haptic/scriggo/native"
)

// A Format represents a content format.
type Format int

// Formats.
const (
	FormatText Format = iota
	FormatHTML
	FormatCSS
	FormatJS
	FormatJSON
	FormatMarkdown
)

// String returns the name of the format.
func (format Format) String() string {
	return ast.Format(format).String()
}

// Converter is implemented by format converters.
type Converter func(src []byte, out io.Writer) error

// Template is a template compiled with the BuildTemplate function.
type Template struct {
	fn               *runtime.Function
	typeof           runtime.TypeOfFunc
	globals          []compiler.Global
	nativeDecls      []UsedNativeDeclaration
	nativeCalls      []UsedNativeCallable
	nativeValues     []UsedNativeValueAccess
	conv             runtime.Converter
	profilingEnabled bool
}

// UsedVariable identifies one global variable referenced by compiled code.
// Path is empty for a native declaration and identifies the declaring template
// file for a Scriggo package variable.
type UsedVariable struct {
	Package          string
	Path             string
	Name             string
	Native           bool
	FunctionPath     string
	FunctionName     string
	FunctionPosition ast.Position
}

// NativeDeclarationKind identifies how a native declaration is bound.
type NativeDeclarationKind uint8

const (
	NativeDeclarationVariable NativeDeclarationKind = iota
	NativeDeclarationFunction
	NativeDeclarationType
	NativeDeclarationConstant
)

// UsedNativeDeclaration describes one native declaration referenced by a
// compiled template. Declaration is the exact value supplied at build time.
type UsedNativeDeclaration struct {
	Package            string
	Name               string
	Kind               NativeDeclarationKind
	Declaration        native.Declaration
	Synchronous        bool
	SynchronousMembers []string
}

// NativeCallableKind identifies a native callable surface.
type NativeCallableKind uint8

const (
	NativeCallableMethod NativeCallableKind = iota
	NativeCallableFunctionField
	NativeCallableIndexedFunction
	NativeCallableFunctionResult
)

// UsedNativeCallable describes one selected or invoked native callable.
// Receiver and Name identify its exact static surface.
type UsedNativeCallable struct {
	Kind            NativeCallableKind
	Receiver        reflect.Type
	Name            string
	Path            string
	Package         string
	DeclarationName string
	Declaration     native.Declaration
	MemberPath      string
	Synchronous     bool
	Constructed     bool
}

// UsedNativeValueAccess describes a native value consumed outside a pure
// selector or callable-selection chain. An empty MemberPath identifies the
// declaration's root value.
type UsedNativeValueAccess struct {
	Package         string
	DeclarationName string
	Declaration     native.Declaration
	MemberPath      string
}

// FormatFS is the interface implemented by a file system that can determine
// the file format from a path name.
type FormatFS interface {
	fs.FS
	Format(name string) (Format, error)
}

// formatFS wraps a FormatFS value to conform to the FormatFS expected by the
// compiler.
type formatFS struct {
	FormatFS
}

func (fsys formatFS) Format(name string) (ast.Format, error) {
	format, err := fsys.FormatFS.Format(name)
	return ast.Format(format), err
}

// formatTypes contains the format types added to the universe block.
var formatTypes = map[ast.Format]reflect.Type{
	ast.FormatHTML:     reflect.TypeFor[native.HTML](),
	ast.FormatCSS:      reflect.TypeFor[native.CSS](),
	ast.FormatJS:       reflect.TypeFor[native.JS](),
	ast.FormatJSON:     reflect.TypeFor[native.JSON](),
	ast.FormatMarkdown: reflect.TypeFor[native.Markdown](),
}

// BuildTemplate builds the named template file rooted at the given file
// system. Imported, rendered and extended files are read from fsys.
//
// If fsys implements FormatFS, file formats are read with its Format method,
// otherwise it depends on the file name extension
//
//	HTML       : .html
//	CSS        : .css
//	JavaScript : .js
//	JSON       : .json
//	Markdown   : .md .mdx .mkd .mkdn .mdown .markdown
//	Text       : all other extensions
//
// If the named file does not exist, BuildTemplate returns an error satisfying
// errors.Is(err, fs.ErrNotExist).
//
// If a build error occurs, it returns a [*BuildError].
func BuildTemplate(fsys fs.FS, name string, options *BuildOptions) (*Template, error) {
	if f, ok := fsys.(FormatFS); ok {
		fsys = formatFS{f}
	}
	co := compiler.Options{
		FormatTypes: formatTypes,
	}
	var conv Converter
	if options != nil {
		co.Globals = options.Globals
		co.UnexpandedTransformer = options.UnexpandedTransformer
		co.ExpandedTransformer = options.ExpandedTransformer
		co.AllowGoStmt = options.AllowGoStmt
		co.NoParseShortShowStmt = options.NoParseShortShowStmt
		co.Importer = options.Packages
		co.MDConverter = compiler.Converter(options.MarkdownConverter)
		co.EnableProfiling = options.EnableProfiling
		co.IsolateRootRenderState = options.IsolateRootRenderState
		conv = options.MarkdownConverter
	}
	code, err := compiler.BuildTemplate(fsys, name, co)
	if err != nil {
		if e, ok := err.(compiler.Error); ok {
			err = &BuildError{err: e}
		}
		return nil, err
	}
	template := &Template{
		fn:               code.Main,
		typeof:           code.TypeOf,
		globals:          code.Globals,
		conv:             runtime.Converter(conv),
		profilingEnabled: code.ProfilingEnabled,
		nativeDecls:      make([]UsedNativeDeclaration, len(code.NativeDeclarations)),
		nativeCalls:      make([]UsedNativeCallable, len(code.NativeCallables)),
		nativeValues:     make([]UsedNativeValueAccess, len(code.NativeValueAccesses)),
	}
	for index, declaration := range code.NativeDeclarations {
		template.nativeDecls[index] = UsedNativeDeclaration{
			Package:            declaration.Package,
			Name:               declaration.Name,
			Kind:               NativeDeclarationKind(declaration.Kind),
			Declaration:        declaration.Declaration,
			Synchronous:        declaration.Synchronous,
			SynchronousMembers: append([]string(nil), declaration.SynchronousMembers...),
		}
	}
	for index, callable := range code.NativeCallables {
		used := UsedNativeCallable{
			Kind:        NativeCallableKind(callable.Kind),
			Receiver:    callable.Receiver,
			Name:        callable.Name,
			Path:        callable.Path,
			MemberPath:  callable.MemberPath,
			Synchronous: callable.Synchronous,
			Constructed: callable.Constructed,
		}
		if callable.Declaration != nil {
			used.Package = callable.Declaration.Package
			used.DeclarationName = callable.Declaration.Name
			used.Declaration = callable.Declaration.Declaration
		}
		template.nativeCalls[index] = used
	}
	for index, access := range code.NativeValueAccesses {
		used := UsedNativeValueAccess{MemberPath: access.MemberPath}
		if access.Declaration != nil {
			used.Package = access.Declaration.Package
			used.DeclarationName = access.Declaration.Name
			used.Declaration = access.Declaration.Declaration
		}
		template.nativeValues[index] = used
	}
	return template, nil
}

// Run runs the template and write the rendered code to out. vars contains
// the values of the global variables. It can be called concurrently by
// multiple goroutines.
//
// If the executed template panics, and it is not recovered, Run returns a
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
//
// If a call to out.Write returns an error, a panic occurs. If the executed
// code does not recover the panic, Run returns the error returned by
// out.Write.
func (t *Template) Run(out io.Writer, vars map[string]any, options *RunOptions) error {
	if out == nil {
		return errors.New("invalid nil out")
	}
	if err := validateVectorRunInput(vars, options); err != nil {
		return err
	}
	vm := runtime.NewVM()
	return t.run(vm, out, initGlobalVariables(t.globals, vars), options)
}

// BatchRun describes one isolated execution in [Template.RunBatch].
type BatchRun struct {
	Out     io.Writer
	Vars    map[string]any
	Options *RunOptions
	Before  func() error
	After   func()
}

// BatchRunError reports the first failed execution in [Template.RunBatch].
type BatchRunError struct {
	Index int
	Err   error
}

func (e *BatchRunError) Error() string {
	return fmt.Sprintf("template batch execution %d failed: %v", e.Index, e.Err)
}

func (e *BatchRunError) Unwrap() error {
	return e.Err
}

// BatchSafe reports whether the compiled template can execute in a batch
// without launching work that can outlive an individual execution.
func (t *Template) BatchSafe() bool {
	return runtime.ValidateBatchProgram(t.fn) == nil
}

// DeterministicSafe reports whether deterministic execution can run the
// compiled template. A non-nil error identifies the rejected operation.
func (t *Template) DeterministicSafe() error {
	return runtime.ValidateDeterministicProgram(t.fn)
}

// RunBatch runs the same compiled template over independent variables,
// contexts, options, and writers while reusing the virtual machine. It stops
// at the first error. Output written by earlier executions is not rolled back.
func (t *Template) RunBatch(runs []BatchRun) error {
	if err := runtime.ValidateBatchProgram(t.fn); err != nil {
		return err
	}
	if len(runs) == 0 {
		return nil
	}
	vm := runtime.NewVM()
	generation := newBatchGlobalVariableGeneration(t.globals)
	for index := range runs {
		if runs[index].Out == nil {
			return &BatchRunError{Index: index, Err: errors.New("invalid nil out")}
		}
		if err := validateVectorRunInput(runs[index].Vars, runs[index].Options); err != nil {
			return &BatchRunError{Index: index, Err: err}
		}
		if index > 0 {
			if vm.BatchGlobalsEscaped() {
				generation = newBatchGlobalVariableGeneration(t.globals)
			}
			vm.ResetBatch()
		}
		globals := generation.initialize(t.globals, runs[index].Vars)
		if runs[index].Before != nil {
			if err := runs[index].Before(); err != nil {
				return &BatchRunError{Index: index, Err: err}
			}
		}
		err := func() error {
			if runs[index].After != nil {
				defer runs[index].After()
			}
			return t.run(vm, runs[index].Out, globals, runs[index].Options)
		}()
		if err != nil {
			return &BatchRunError{Index: index, Err: err}
		}
	}
	return nil
}

func (t *Template) run(
	vm *runtime.VM,
	out io.Writer,
	globals []reflect.Value,
	options *RunOptions,
) (err error) {
	var vectorState *runtime.VectorState
	if options != nil && options.Vector != nil {
		vectorState, err = prepareVectorState(t.globals, globals, options.Vector)
		if err != nil {
			return err
		}
		vm.SetVectorState(vectorState)
		defer func() {
			if vectorState == nil {
				return
			}
			if recovered := recover(); recovered != nil {
				cause, ok := recovered.(error)
				if !ok {
					cause = fmt.Errorf("template execution panicked: %v", recovered)
				}
				vectorState.CloseWithError(cause)
				panic(recovered)
			}
			if err != nil {
				vectorState.CloseWithError(err)
			} else {
				vectorState.Close()
			}
		}()
	}
	if options != nil {
		if options.Context != nil {
			vm.SetContext(options.Context)
		}
		if options.ChildContext != nil {
			vm.SetChildContext(options.ChildContext)
		}
		if options.ObserveMutation != nil {
			vm.SetMutationObserver(runtime.MutationObserver(options.ObserveMutation))
		}
		if options.ObserveMutationContext != nil {
			vm.SetMutationContextObserver(runtime.MutationContextObserver(options.ObserveMutationContext))
		}
		if options.ObserveNativeCall != nil {
			vm.SetNativeCallObserver(runtime.NativeCallObserver(options.ObserveNativeCall))
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
		if err := vm.SetNativeFunctionTrampolines(options.NativeFunctionTrampolines); err != nil {
			return err
		}
		vm.SetDeterministic(options.Deterministic)
		if options.Print != nil {
			vm.SetPrint(runtime.PrintFunc(options.Print))
		}
	}
	vm.SetRenderer(out, t.conv)
	if options != nil && options.MaxParallelRenders != 0 {
		vm.SetMaxParallelRenders(options.MaxParallelRenders)
	}
	if options != nil && options.CollectSourceMap {
		vm.SetCollectSourceMap(true)
	}
	// Enable profiling if it was enabled at build time.
	if t.profilingEnabled {
		vm.SetProfiling(true)
	}
	err = vm.Run(t.fn, t.typeof, globals)
	if vectorState != nil {
		if err == nil {
			err = vectorState.VMNativeCompletionError()
		}
		if err != nil {
			vectorState.CloseWithError(err)
		} else {
			vectorState.Close()
		}
		vectorState = nil
	}
	// Collect the source map if requested, converting runtime frames to public ones.
	if options != nil && options.CollectSourceMap {
		spans := vm.SourceSpans()
		options.SourceSpans = make([]SourceSpan, len(spans))
		for i, s := range spans {
			frames := make([]SourceFrame, len(s.Frames))
			for j, f := range s.Frames {
				frames[j] = SourceFrame{Path: f.Path, Line: f.Line}
			}
			options.SourceSpans[i] = SourceSpan{Frames: frames, Length: s.Length, IsText: s.IsText}
		}
	}
	// Collect profiling data if profiling was enabled and Profile was provided.
	if t.profilingEnabled && options != nil && options.Profile != nil {
		if data := vm.GetProfileData(); data != nil {
			options.Profile.StartTime = data.StartTime
			options.Profile.EndTime = data.EndTime
			options.Profile.TotalDuration = data.TotalDuration
			// Convert runtime.ProfileCallRecord to scriggo.CallRecord.
			options.Profile.Calls = make([]*CallRecord, len(data.Calls))
			for i, rc := range data.Calls {
				options.Profile.Calls[i] = &CallRecord{
					ID:           rc.ID,
					ParentID:     rc.ParentID,
					Name:         rc.Name,
					Package:      rc.Package,
					File:         rc.File,
					Line:         rc.Line,
					Kind:         CallKind(rc.Kind),
					StartTime:    rc.StartTime,
					Duration:     rc.Duration,
					SelfDuration: rc.SelfDuration,
					TemplatePath: rc.TemplatePath,
				}
			}
		}
	}
	if err != nil {
		if p, ok := err.(*runtime.PanicError); ok {
			err = &PanicError{p}
		}
		return err
	}
	return nil
}

func validateVectorRunInput(vars map[string]any, options *RunOptions) error {
	if options == nil || options.Vector == nil {
		return nil
	}
	vector := options.Vector
	if vector.Authority == nil || vector.Count <= 0 {
		return runtime.ErrVectorInvalidConfiguration
	}
	if vector.Boundary != nil || vector.Lifecycle != nil {
		lifecycle := reflect.ValueOf(vector.Lifecycle)
		if !vector.VMNative || vector.Boundary == nil || !vector.Boundary.Valid() ||
			vector.Lifecycle == nil || lifecycle.Kind() == reflect.Pointer && lifecycle.IsNil() {
			return runtime.ErrVectorInvalidConfiguration
		}
	}
	if vector.FiberBoundary != nil || vector.FiberLifecycle != nil {
		lifecycle := reflect.ValueOf(vector.FiberLifecycle)
		if !vector.VMNative || vector.FiberBoundary == nil || !vector.FiberBoundary.Valid() ||
			vector.FiberLifecycle == nil || lifecycle.Kind() == reflect.Pointer && lifecycle.IsNil() ||
			vector.Boundary == nil || vector.Lifecycle == nil {
			return runtime.ErrVectorInvalidConfiguration
		}
	}
	deferred := len(vector.DeferredBindings) != 0
	if deferred && (len(vector.Bindings) != 0 || len(vector.Contexts) != 0) {
		return runtime.ErrVectorInvalidConfiguration
	}
	if !deferred && len(vector.Contexts) != vector.Count {
		return runtime.ErrVectorInvalidConfiguration
	}
	for index, ctx := range vector.Contexts {
		if ctx == nil {
			return fmt.Errorf("%w: context %d is nil", runtime.ErrVectorInvalidConfiguration, index)
		}
	}
	for name, column := range vector.Bindings {
		if name == "" {
			return fmt.Errorf("%w: empty binding name", runtime.ErrVectorInvalidConfiguration)
		}
		if _, duplicate := vars[name]; duplicate {
			return fmt.Errorf("%w: binding %q is also initialized", runtime.ErrVectorInvalidConfiguration, name)
		}
		value := reflect.ValueOf(column)
		if !value.IsValid() || value.Kind() != reflect.Slice || value.Len() != vector.Count {
			return fmt.Errorf("%w: binding %q must be a concrete slice of length %d", runtime.ErrVectorInvalidConfiguration, name, vector.Count)
		}
	}
	seen := make(map[string]struct{}, len(vector.DeferredBindings))
	for _, name := range vector.DeferredBindings {
		if name == "" {
			return fmt.Errorf("%w: empty deferred binding name", runtime.ErrVectorInvalidConfiguration)
		}
		if _, duplicate := seen[name]; duplicate {
			return fmt.Errorf("%w: deferred binding %q is duplicated", runtime.ErrVectorInvalidConfiguration, name)
		}
		seen[name] = struct{}{}
		if _, duplicate := vars[name]; duplicate {
			return fmt.Errorf("%w: binding %q is also initialized", runtime.ErrVectorInvalidConfiguration, name)
		}
	}
	return nil
}

func prepareVectorState(
	variables []compiler.Global,
	globals []reflect.Value,
	options *VectorRunOptions,
) (*runtime.VectorState, error) {
	deferred := len(options.DeferredBindings) != 0
	names := slices.Clone(options.DeferredBindings)
	if !deferred {
		names = make([]string, 0, len(options.Bindings))
		for name := range options.Bindings {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	bindings := make([]runtime.VectorBinding, 0, len(names))
	for _, name := range names {
		slots := make([]reflect.Value, 0, 1)
		globalIndexes := make([]int, 0, 1)
		for index, variable := range variables {
			if variable.Name != name {
				continue
			}
			if variable.Value.IsValid() || index >= len(globals) {
				return nil, fmt.Errorf("%w: binding %q resolves to a fixed global", runtime.ErrVectorInvalidConfiguration, name)
			}
			slots = append(slots, globals[index])
			if options.VMNative {
				globalIndexes = append(globalIndexes, index)
			}
		}
		if len(slots) == 0 {
			return nil, fmt.Errorf("%w: binding %q has no mutable global", runtime.ErrVectorInvalidConfiguration, name)
		}
		for _, slot := range slots[1:] {
			if slot.Type() != slots[0].Type() {
				return nil, fmt.Errorf(
					"%w: binding %q resolves to globals with different types",
					runtime.ErrVectorInvalidConfiguration,
					name,
				)
			}
		}
		binding := runtime.VectorBinding{
			Name: name, Type: slots[0].Type(), Slots: slots, GlobalIndexes: globalIndexes,
		}
		if !deferred {
			owned, err := normalizeVectorColumn(
				name,
				reflect.ValueOf(options.Bindings[name]),
				slots[0].Type(),
				options.Count,
			)
			if err != nil {
				return nil, err
			}
			binding.Column = owned
		}
		bindings = append(bindings, binding)
	}
	if deferred {
		if options.VMNative {
			state, err := runtime.NewVMNativeDeferredVectorState(options.Authority, options.Count, bindings)
			if err != nil {
				return nil, err
			}
			if options.Boundary != nil {
				if err := state.SetVMNativeBoundary(options.Boundary, options.Lifecycle); err != nil {
					state.Close()
					return nil, err
				}
			}
			if options.FiberBoundary != nil {
				if err := state.SetVMNativeFiberBoundary(options.FiberBoundary, options.FiberLifecycle); err != nil {
					state.Close()
					return nil, err
				}
			}
			return state, nil
		}
		return runtime.NewDeferredVectorState(options.Authority, options.Count, bindings)
	}
	if options.VMNative {
		state, err := runtime.NewVMNativeVectorState(options.Authority, options.Count, bindings, options.Contexts)
		if err != nil {
			return nil, err
		}
		if options.Boundary != nil {
			if err := state.SetVMNativeBoundary(options.Boundary, options.Lifecycle); err != nil {
				state.Close()
				return nil, err
			}
		}
		if options.FiberBoundary != nil {
			if err := state.SetVMNativeFiberBoundary(options.FiberBoundary, options.FiberLifecycle); err != nil {
				state.Close()
				return nil, err
			}
		}
		return state, nil
	}
	return runtime.NewVectorState(options.Authority, options.Count, bindings, options.Contexts)
}

func normalizeVectorColumn(
	name string,
	column reflect.Value,
	variableType reflect.Type,
	count int,
) (reflect.Value, error) {
	elementType := column.Type().Elem()
	direct := elementType == variableType
	indirect := elementType.Kind() == reflect.Pointer && elementType.Elem().AssignableTo(variableType)
	if !direct && !indirect {
		return reflect.Value{}, fmt.Errorf(
			"%w: binding %q has element type %s, want %s or a pointer to it",
			runtime.ErrVectorInvalidConfiguration,
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
					"%w: binding %q item %d is a nil pointer",
					runtime.ErrVectorInvalidConfiguration,
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

// Disassemble disassembles a template and returns its assembly code.
//
// n determines the maximum length, in runes, of a disassembled text:
//
//	n > 0: at most n runes; leading and trailing white space are removed
//	n == 0: no text
//	n < 0: all text
func (t *Template) Disassemble(n int) []byte {
	assemblies := compiler.Disassemble(t.fn, t.globals, n)
	return assemblies["main"]
}

// Format returns the output format of the template when executed.
func (t *Template) Format() Format {
	return Format(t.fn.Format)
}

// UsedVars returns the names of the global variables used in the template.
// A variable used in dead code may not be returned as used.
func (t *Template) UsedVars() []string {
	vars := make([]string, len(t.globals))
	for i, global := range t.globals {
		vars[i] = global.Name
	}
	sort.Strings(vars)
	return vars
}

// UsedVariables returns the global variables referenced by the compiled
// template. A variable used only in dead code may not be returned.
func (t *Template) UsedVariables() []UsedVariable {
	variables := make([]UsedVariable, len(t.globals))
	for index, global := range t.globals {
		variables[index] = UsedVariable{
			Package:          global.Pkg,
			Path:             global.Path,
			Name:             global.Name,
			Native:           global.Native,
			FunctionPath:     global.FunctionPath,
			FunctionName:     global.FunctionName,
			FunctionPosition: global.FunctionPosition,
		}
	}
	sort.Slice(variables, func(i, j int) bool {
		left, right := variables[i], variables[j]
		if left.Native != right.Native {
			return !left.Native
		}
		if left.Path != right.Path {
			return left.Path < right.Path
		}
		if left.Package != right.Package {
			return left.Package < right.Package
		}
		if left.Name != right.Name {
			return left.Name < right.Name
		}
		if left.FunctionPath != right.FunctionPath {
			return left.FunctionPath < right.FunctionPath
		}
		if left.FunctionName != right.FunctionName {
			return left.FunctionName < right.FunctionName
		}
		if left.FunctionPosition.Start != right.FunctionPosition.Start {
			return left.FunctionPosition.Start < right.FunctionPosition.Start
		}
		return left.FunctionPosition.End < right.FunctionPosition.End
	})
	return variables
}

// UsedNativeDeclarations returns every native function, variable, type, and
// constant referenced by the compiled template and its nested function graph.
func (t *Template) UsedNativeDeclarations() []UsedNativeDeclaration {
	declarations := make([]UsedNativeDeclaration, len(t.nativeDecls))
	for index, declaration := range t.nativeDecls {
		declarations[index] = declaration
		declarations[index].SynchronousMembers = append([]string(nil), declaration.SynchronousMembers...)
	}
	return declarations
}

// UsedNativeCallables returns every native callable surface used by the
// compiled template and its nested function graph.
func (t *Template) UsedNativeCallables() []UsedNativeCallable {
	callables := make([]UsedNativeCallable, len(t.nativeCalls))
	copy(callables, t.nativeCalls)
	return callables
}

// NativeFunctionUsesReflection reports whether calls of typ use the reflective fallback.
func NativeFunctionUsesReflection(typ reflect.Type) bool {
	return runtime.NativeFunctionUsesReflection(typ)
}

// UsedNativeValueAccesses returns native values consumed outside pure
// selector or callable-selection chains.
func (t *Template) UsedNativeValueAccesses() []UsedNativeValueAccess {
	accesses := make([]UsedNativeValueAccess, len(t.nativeValues))
	copy(accesses, t.nativeValues)
	return accesses
}

// ClearVMPool releases pooled VMs to allow garbage collection.
// Call this after large rendering operations complete to reduce memory usage
// from parallel rendering spikes (go render statements).
//
// The pool will rebuild naturally on the next render operation - there is no
// persistent impact on performance, just a one-time ~10ms overhead for creating
// new VMs on the first render after clearing.
//
// Thread-safe: can be called from any goroutine.
func ClearVMPool() {
	runtime.ClearVMPool()
}

var emptyInit = map[string]any{}

// initGlobalVariables initializes the global variables and returns their
// values. It panics if init is not valid.
func initGlobalVariables(variables []compiler.Global, init map[string]any) []reflect.Value {
	return initGlobalVariableGeneration(variables, init)
}

func initGlobalVariableGeneration(variables []compiler.Global, init map[string]any) []reflect.Value {
	if len(variables) == 0 {
		return nil
	}
	if init == nil {
		init = emptyInit
	}
	values := make([]reflect.Value, len(variables))
	for index, variable := range variables {
		if variable.Value.IsValid() {
			if variable.Pkg == "main" {
				if _, initialized := init[variable.Name]; initialized {
					panic(fmt.Sprintf("variable %q already initialized", variable.Name))
				}
			}
			values[index] = variable.Value
			continue
		}
		slot := reflect.New(variable.Type).Elem()
		values[index] = initializeGlobalVariable(variable, init, slot)
	}
	return values
}

type batchGlobalVariableGeneration struct {
	values []reflect.Value
	slots  []reflect.Value
}

func newBatchGlobalVariableGeneration(variables []compiler.Global) batchGlobalVariableGeneration {
	storage := make([]reflect.Value, len(variables)*2)
	generation := batchGlobalVariableGeneration{
		values: storage[:len(variables):len(variables)],
		slots:  storage[len(variables):],
	}
	for index, variable := range variables {
		if !variable.Value.IsValid() {
			generation.slots[index] = reflect.New(variable.Type).Elem()
		}
	}
	return generation
}

func (g *batchGlobalVariableGeneration) initialize(
	variables []compiler.Global,
	init map[string]any,
) []reflect.Value {
	if len(variables) == 0 {
		return nil
	}
	if init == nil {
		init = emptyInit
	}
	for index, variable := range variables {
		if variable.Value.IsValid() {
			if variable.Pkg == "main" {
				if _, initialized := init[variable.Name]; initialized {
					panic(fmt.Sprintf("variable %q already initialized", variable.Name))
				}
			}
			g.values[index] = variable.Value
			continue
		}
		slot := g.slots[index]
		slot.SetZero()
		g.values[index] = initializeGlobalVariable(variable, init, slot)
	}
	return g.values
}

func initializeGlobalVariable(variable compiler.Global, init map[string]any, slot reflect.Value) reflect.Value {
	if variable.Pkg != "main" {
		return slot
	}
	value, ok := init[variable.Name]
	if !ok {
		return slot
	}
	if value == nil {
		panic(fmt.Sprintf("variable initializer %q cannot be nil", variable.Name))
	}
	val := reflect.ValueOf(value)
	typ := val.Type()
	if types.AssignableTo(typ, variable.Type) {
		slot.Set(val)
		return slot
	}
	if typ.Kind() == reflect.Pointer && types.AssignableTo(typ.Elem(), variable.Type) {
		if val.IsNil() {
			panic(fmt.Sprintf("variable initializer %q cannot be a nil pointer", variable.Name))
		}
		return val.Elem()
	}
	panic(fmt.Sprintf("variable initializer %q must have type assignable to %s, but have %s",
		variable.Name, variable.Type, typ))
}

// HTMLEscape escapes s, replacing the characters <, >, &, " and ' and returns
// the escaped string as HTML type.
//
// Use HTMLEscape to put a trusted or untrusted string into an HTML element
// content or in a quoted attribute value. But don't use it with complex
// attributes like href, src, style, or any of the event handlers like
// onmouseover.
func HTMLEscape(s string) native.HTML {
	n := 0
	j := 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '"', '\'', '&':
			n += 4
		case '<', '>':
			n += 3
		default:
			continue
		}
		if n <= 4 {
			j = i
		}
	}
	if n == 0 {
		return native.HTML(s)
	}
	b := make([]byte, len(s)+n)
	if j > 0 {
		copy(b[:j], s[:j])
	}
	for i := j; i < len(s); i++ {
		switch c := s[i]; c {
		case '"':
			copy(b[j:], "&#34;")
			j += 5
		case '\'':
			copy(b[j:], "&#39;")
			j += 5
		case '&':
			copy(b[j:], "&amp;")
			j += 5
		case '<':
			copy(b[j:], "&lt;")
			j += 4
		case '>':
			copy(b[j:], "&gt;")
			j += 4
		default:
			b[j] = c
			j++
			continue
		}
		if j == i+n {
			copy(b[j:], s[i:])
			break
		}
	}
	return native.HTML(b)
}
