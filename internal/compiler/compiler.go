// Copyright 2019 The Scriggo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package compiler implements parsing, type checking and emitting of sources.
//
// A program can be compiled using
//
//	BuildProgram
//
// while a template is compiled through
//
//	BuildTemplate
package compiler

import (
	"fmt"
	"io/fs"
	"maps"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"gitlab.com/haproxy-haptic/scriggo/ast"
	"gitlab.com/haproxy-haptic/scriggo/internal/runtime"
	"gitlab.com/haproxy-haptic/scriggo/native"
)

// formatTypeName reports the type name for each format.
var formatTypeName = [...]string{"string", "html", "css", "js", "json", "markdown"}

// internalOperatorZero and internalOperatorNotZero are two internal operators
// that are inserted in the tree by the type checker and that are handled by the
// emitter as two unary operators that return true if the operand is,
// respectively, the zero or not the zero of its type.
//
// As a special case, if the operand is an interface type then its value is
// compared with the zero of the dynamic type of the interface.
const (
	internalOperatorZero = ast.OperatorExtendedNot + iota + 1
	internalOperatorNotZero
)

// Error represents an error returned by the compiler. The types that
// implement the Error interface are four types of the compiler package
//
//	*GoModError
//	*SyntaxError
//	*CycleError
//	*CheckingError
//	*LimitExceededError
type Error interface {
	error
	Position() ast.Position
	Path() string
	Message() string
}

// Options represents a set of options used during the compilation.
type Options struct {
	AllowGoStmt            bool
	NoParseShortShowStmt   bool
	IsolateRootRenderState bool

	FormatTypes map[ast.Format]reflect.Type
	Globals     native.Declarations

	// Importer imports the native packages.
	Importer native.Importer

	// MDConverter converts a Markdown source code to HTML.
	MDConverter Converter

	UnexpandedTransformer func(*ast.Tree) error

	ExpandedTransformer func(*ast.Tree) error

	// EnableProfiling enables profiling instrumentation in the compiled code.
	// When true, the runtime will collect timing data for function calls,
	// macros, and includes during execution.
	EnableProfiling bool
}

// GoModError represents an error in a go.mod file.
type GoModError struct {
	path string
	pos  ast.Position
	msg  string
}

// Error returns a string representing the error.
func (e *GoModError) Error() string {
	return fmt.Sprintf("%s:%s: %s", e.path, e.pos, e.msg)
}

// Message returns the message of error, without position and path.
func (e *GoModError) Message() string {
	return e.msg
}

// Path returns the path of the go.mod file.
func (e *GoModError) Path() string {
	return e.path
}

// Position returns the position of error in the go.mod file.
func (e *GoModError) Position() ast.Position {
	return e.pos
}

// isHiddenPath returns true if any component of the path starts with '.'.
// This handles both hidden files (e.g., ".hidden") and files in hidden directories
// (e.g., ".hidden/file.txt" or "dir/.secret/file.txt").
func isHiddenPath(path string) bool {
	for _, part := range strings.Split(path, "/") {
		if len(part) > 0 && part[0] == '.' {
			return true
		}
	}
	return false
}

// compileTemplateFile compiles a single template file and returns the compiled code.
// If canonicalGlobals is provided (non-nil), it will be used to ensure consistent
// global variable indexing.
func compileTemplateFile(fsys fs.FS, path string, opts Options, canonicalGlobals []Global) (*Code, error) {
	// Parse the source code.
	tree, err := ParseTemplate(fsys, path, opts.NoParseShortShowStmt, opts.UnexpandedTransformer)
	if err != nil {
		return nil, err
	}

	// Transform the expanded tree if a transformer is provided.
	if opts.ExpandedTransformer != nil {
		if err := opts.ExpandedTransformer(tree); err != nil {
			return nil, err
		}
	}

	// Desugar the wildcard-slice-access fork extension. Walks the
	// tree and replaces every for-range whose range expression
	// contains `[*]` with a chain of nested synthetic for-ranges.
	// Uses a per-build counter (`compilation.currentWildcardIndex`)
	// to make synthetic `$wildN` identifiers unique across the
	// build.
	if err := desugarWildcards(tree, &compilation{currentWildcardIndex: -1}); err != nil {
		return nil, err
	}

	// Lower collection pipelines whose stages take literal closures into
	// fused loops, so the closures are called from inside the VM instead of
	// across the native boundary once per element.
	if err := desugarPipelines(tree, &compilation{currentPipelineIndex: -1}); err != nil {
		return nil, err
	}

	// Type check the tree.
	var isolateRootRenderPath string
	if opts.IsolateRootRenderState {
		isolateRootRenderPath = tree.Path
	}
	checkerOpts := checkerOptions{
		allowGoStmt:           opts.AllowGoStmt,
		formatTypes:           opts.FormatTypes,
		globals:               opts.Globals,
		mdConverter:           opts.MDConverter,
		mod:                   templateMod,
		isolateRootRenderPath: isolateRootRenderPath,
	}
	tci, err := typecheck(tree, opts.Importer, checkerOpts)
	if err != nil {
		return nil, err
	}
	typeInfos := map[ast.Node]*typeInfo{}
	for _, pkgInfos := range tci {
		for node, ti := range pkgInfos.TypeInfos {
			typeInfos[node] = ti
		}
	}

	// Emit the code.
	return emitTemplate(tree, typeInfos, tci["main"].IndirectVars, opts.FormatTypes, canonicalGlobals)
}

// BuildProgram builds a Go program from the package in the root of fsys with
// the given options, importing the imported packages from packages.
//
// Current limitation: fsys can contain only one Go file in its root.
//
// If a compilation error occurs, it returns a CompilerError error.
func BuildProgram(fsys fs.FS, opts Options) (*Code, error) {

	// Parse the source code.
	tree, err := ParseProgram(fsys)
	if err != nil {
		return nil, err
	}

	// Desugar the wildcard-slice-access fork extension. See the
	// matching call in compileTemplateFile for context — same
	// pass applied to Go-program builds for parity.
	if err := desugarWildcards(tree, &compilation{currentWildcardIndex: -1}); err != nil {
		return nil, err
	}

	// Type check the tree.
	checkerOpts := checkerOptions{
		mod:         programMod,
		allowGoStmt: opts.AllowGoStmt,
		globals:     opts.Globals,
	}
	tci, err := typecheck(tree, opts.Importer, checkerOpts)
	if err != nil {
		return nil, err
	}
	typeInfos := map[ast.Node]*typeInfo{}
	for _, pkgInfos := range tci {
		maps.Copy(typeInfos, pkgInfos.TypeInfos)
	}

	// Emit the code.
	code, err := emitProgram(tree.Nodes[0].(*ast.Package), typeInfos, tci[tree.Path].IndirectVars)
	if err != nil {
		return nil, err
	}

	return code, nil
}

// BuildTemplate builds the named template file rooted at the given file
// system. If fsys implements FormatFS, the file format is read from its
// Format method, otherwise it depends on the extension of the file name.
// Any error related to the compilation itself is returned as a CompilerError.
func BuildTemplate(fsys fs.FS, name string, opts Options) (*Code, error) {
	// Compile the main template (no canonical globals - it creates them).
	code, err := compileTemplateFile(fsys, name, opts, nil)
	if err != nil {
		return nil, err
	}

	code.ProfilingEnabled = opts.EnableProfiling

	return code, nil
}

// CheckingError records a type checking error with the path and the position
// where the error occurred.
type CheckingError struct {
	path string
	pos  ast.Position
	err  error
}

// Error returns a string representation of the type checking error.
func (e *CheckingError) Error() string {
	return fmt.Sprintf("%s:%s: %s", e.path, e.pos, e.err)
}

// Message returns the message of the type checking error, without position and
// path.
func (e *CheckingError) Message() string {
	return e.err.Error()
}

// Path returns the path of the type checking error.
func (e *CheckingError) Path() string {
	return e.path
}

// Position returns the position of the checking error.
func (e *CheckingError) Position() ast.Position {
	return e.pos
}

// Global represents a global variable with a package, name, type (only for
// not predefined globals) and value (only for predefined globals). Value, if
// present, must be a pointer to the variable value.
type Global struct {
	Pkg              string
	Path             string
	Name             string
	Type             reflect.Type
	Value            reflect.Value
	Native           bool
	FunctionPath     string
	FunctionName     string
	FunctionPosition ast.Position
}

type NativeDeclarationKind uint8

const (
	NativeDeclarationVariable NativeDeclarationKind = iota
	NativeDeclarationFunction
	NativeDeclarationType
	NativeDeclarationConstant
)

type NativeDeclaration struct {
	Package            string
	Name               string
	Kind               NativeDeclarationKind
	Declaration        native.Declaration
	Synchronous        bool
	SynchronousMembers []string
	identity           *NativeDeclaration
}

type NativeCallableKind uint8

const (
	NativeCallableMethod NativeCallableKind = iota
	NativeCallableFunctionField
	NativeCallableIndexedFunction
	NativeCallableFunctionResult
)

type NativeCallable struct {
	Kind        NativeCallableKind
	Receiver    reflect.Type
	Name        string
	Path        string
	Declaration *NativeDeclaration
	MemberPath  string
	Synchronous bool
	Constructed bool
}

type NativeValueAccess struct {
	Declaration *NativeDeclaration
	MemberPath  string
}

// globalKey returns the unique key for a global variable, combining package and name.
func globalKey(pkg, name string) string {
	return pkg + "." + name
}

// Code is the result of a package emitting process.
type Code struct {
	// Globals is a slice of all globals used in Code.
	Globals []Global
	// NativeDeclarations contains every referenced native declaration.
	NativeDeclarations []NativeDeclaration
	// NativeCallables contains every referenced native method and function field.
	NativeCallables []NativeCallable
	// NativeValueAccesses contains native values consumed outside pure selector chains.
	NativeValueAccesses []NativeValueAccess
	// Functions is a map of exported functions indexed by name.
	Functions map[string]*runtime.Function
	// Main is the Code entry point.
	Main *runtime.Function
	// TypeOf returns the type of a value, including new types defined in code.
	TypeOf runtime.TypeOfFunc
	// ProfilingEnabled indicates whether profiling was enabled at build time.
	ProfilingEnabled bool
}

type nativeValueAccessKey struct {
	declaration *NativeDeclaration
	memberPath  string
}

type nativeValueParentCollector struct {
	parents map[ast.Node][]ast.Node
	seen    map[ast.Node]struct{}
}

var astNodeType = reflect.TypeFor[ast.Node]()
var astPositionType = reflect.TypeFor[ast.Position]()

func (collector *nativeValueParentCollector) walk(node, parent ast.Node) {
	if node == nil {
		return
	}
	value := reflect.ValueOf(node)
	if value.Kind() == reflect.Pointer && value.IsNil() {
		return
	}
	if parent != nil {
		collector.parents[node] = append(collector.parents[node], parent)
	}
	if _, ok := collector.seen[node]; ok {
		return
	}
	collector.seen[node] = struct{}{}
	if value.Kind() == reflect.Pointer {
		value = value.Elem()
	}
	collector.walkContainer(value, node)
}

func (collector *nativeValueParentCollector) walkContainer(value reflect.Value, parent ast.Node) {
	if !value.IsValid() {
		return
	}
	if value.Kind() == reflect.Interface {
		if value.IsNil() {
			return
		}
		value = value.Elem()
	}
	if value.Kind() == reflect.Pointer && value.Type().Implements(astNodeType) &&
		value.Type().Elem() != astPositionType {
		if !value.IsNil() {
			collector.walk(value.Interface().(ast.Node), parent)
		}
		return
	}
	switch value.Kind() {
	case reflect.Array, reflect.Slice:
		for index := range value.Len() {
			collector.walkContainer(value.Index(index), parent)
		}
	case reflect.Pointer:
		if value.IsNil() || !nativeValueASTContainer(value.Type().Elem()) {
			return
		}
		collector.walkContainer(value.Elem(), parent)
	case reflect.Struct:
		if !nativeValueASTContainer(value.Type()) {
			return
		}
		for index := range value.NumField() {
			fieldType := value.Type().Field(index)
			if fieldType.PkgPath != "" {
				continue
			}
			collector.walkContainer(value.Field(index), parent)
		}
	}
}

func nativeValueASTContainer(typ reflect.Type) bool {
	return typ != astPositionType && (typ.Name() == "" || typ.PkgPath() == astPositionType.PkgPath())
}

func usedNativeValueAccesses(
	root ast.Node,
	typeInfos map[ast.Node]*typeInfo,
) []NativeValueAccess {
	parents := map[ast.Node][]ast.Node{}
	collector := nativeValueParentCollector{
		parents: parents,
		seen:    map[ast.Node]struct{}{},
	}
	collector.walk(root, nil)
	byKey := map[nativeValueAccessKey]struct{}{}
	for node, info := range typeInfos {
		if info == nil || info.Type == nil || info.IsType() || info.IsPackage() {
			continue
		}
		if nativeValueUsedOnlyAsSelectorOrCallable(node, parents, typeInfos, info) {
			continue
		}
		for _, origin := range resolvedNativeProvenance(info) {
			if origin.Declaration == nil || origin.Local || origin.Result || origin.Selected {
				continue
			}
			byKey[nativeValueAccessKey{
				declaration: origin.Declaration,
				memberPath:  origin.Path,
			}] = struct{}{}
		}
	}
	accesses := make([]NativeValueAccess, 0, len(byKey))
	for key := range byKey {
		accesses = append(accesses, NativeValueAccess{
			Declaration: key.declaration,
			MemberPath:  key.memberPath,
		})
	}
	sort.Slice(accesses, func(leftIndex, rightIndex int) bool {
		left := accesses[leftIndex]
		right := accesses[rightIndex]
		var leftPackage, leftName, rightPackage, rightName string
		if left.Declaration != nil {
			leftPackage = left.Declaration.Package
			leftName = left.Declaration.Name
		}
		if right.Declaration != nil {
			rightPackage = right.Declaration.Package
			rightName = right.Declaration.Name
		}
		if leftPackage != rightPackage {
			return leftPackage < rightPackage
		}
		if leftName != rightName {
			return leftName < rightName
		}
		return left.MemberPath < right.MemberPath
	})
	return accesses
}

func nativeValueUsedOnlyAsSelectorOrCallable(
	node ast.Node,
	parents map[ast.Node][]ast.Node,
	typeInfos map[ast.Node]*typeInfo,
	info *typeInfo,
) bool {
	nodeParents := parents[node]
	if len(nodeParents) == 0 {
		return false
	}
	if info.NativeCallable {
		return true
	}
	expression, ok := node.(ast.Expression)
	if !ok {
		return false
	}
	for _, parent := range nodeParents {
		if parentInfo := typeInfos[parent]; parentInfo != nil && parentInfo.IsType() {
			continue
		}
		switch typed := parent.(type) {
		case *ast.Call:
			if typed.Func != expression {
				return false
			}
		case *ast.Index:
			if typed.Expr != expression {
				return false
			}
		case *ast.Selector:
			if typed.Expr != expression {
				return false
			}
		case *ast.UnaryOperator:
			parentInfo := typeInfos[parent]
			if typed.Op != ast.OperatorAddress || typed.Expr != expression || parentInfo == nil ||
				parentInfo.MethodType != methodValueConcrete ||
				!nativeValueUsedOnlyAsSelectorOrCallable(parent, parents, typeInfos, parentInfo) {
				return false
			}
		default:
			return false
		}
	}
	return true
}

func usedNativeDeclarations(typeInfos map[ast.Node]*typeInfo) []NativeDeclaration {
	seen := map[*NativeDeclaration]struct{}{}
	declarations := make([]NativeDeclaration, 0)
	for _, info := range typeInfos {
		declaration := info.NativeDeclaration
		if declaration == nil {
			continue
		}
		if _, ok := seen[declaration]; ok {
			continue
		}
		seen[declaration] = struct{}{}
		declarations = append(declarations, *declaration)
	}
	sort.Slice(declarations, func(i, j int) bool {
		if declarations[i].Package != declarations[j].Package {
			return declarations[i].Package < declarations[j].Package
		}
		if declarations[i].Name != declarations[j].Name {
			return declarations[i].Name < declarations[j].Name
		}
		return declarations[i].Kind < declarations[j].Kind
	})
	return declarations
}

func batchUnsafeNative(declarations []NativeDeclaration, callables []NativeCallable) string {
	for _, declaration := range declarations {
		if declaration.Synchronous || !nativeDeclarationNeedsDirectCertificate(declaration) {
			continue
		}
		if declaration.Package == "" || declaration.Package == "main" {
			return declaration.Name
		}
		return declaration.Package + "." + declaration.Name
	}
	for _, callable := range callables {
		if !callable.Synchronous {
			if callable.Declaration != nil {
				name := callable.Declaration.Name
				if callable.Declaration.Package != "" && callable.Declaration.Package != "main" {
					name = callable.Declaration.Package + "." + name
				}
				if callable.MemberPath != "" {
					name += "." + callable.MemberPath
				}
				return name
			}
			return callable.Path
		}
	}
	return ""
}

func nativeDeclarationNeedsDirectCertificate(declaration NativeDeclaration) bool {
	if declaration.Kind == NativeDeclarationFunction {
		return true
	}
	if declaration.Kind != NativeDeclarationVariable {
		return false
	}
	typ := reflect.TypeOf(declaration.Declaration)
	if typ == nil || typ.Kind() != reflect.Pointer {
		return true
	}
	return typ.Elem().Kind() == reflect.Func
}

type nativeCallableKey struct {
	kind        NativeCallableKind
	receiver    reflect.Type
	name        string
	declaration *NativeDeclaration
	memberPath  string
	constructed bool
}

func usedNativeCallables(
	typeInfos map[ast.Node]*typeInfo,
	declarations []NativeDeclaration,
) []NativeCallable {
	nativeTypes := map[reflect.Type]struct{}{}
	for _, declaration := range declarations {
		for _, root := range nativeDeclarationRoots(declaration) {
			addNativeReachableTypes(nativeTypes, root)
		}
	}
	byKey := map[nativeCallableKey]NativeCallable{}
	for node, info := range typeInfos {
		if info == nil || info.NativeDeclaration != nil {
			continue
		}
		var receiver ast.Expression
		var name string
		selector, isSelector := node.(*ast.Selector)
		index, isIndex := node.(*ast.Index)
		call, isCall := node.(*ast.Call)
		switch {
		case isSelector:
			receiver = selector.Expr
			name = selector.Ident
		case isIndex && info.Type != nil && info.Type.Kind() == reflect.Func:
			receiver = index.Expr
			name = index.Index.String()
		case isCall:
			switch call.Func.(type) {
			case *ast.Selector, *ast.Index:
				continue
			}
			receiver = call.Func
			name = "<result>"
		default:
			continue
		}
		receiverInfo := typeInfos[receiver]
		if receiverInfo == nil || receiverInfo.Type == nil {
			continue
		}
		nativeReachable := isNativeReachableType(nativeTypes, receiverInfo.Type)
		provenance := resolvedNativeProvenance(info)
		if !isCall && !nativeReachable {
			requiresAudit := false
			for _, origin := range provenance {
				if !origin.Local {
					requiresAudit = true
					break
				}
			}
			if !requiresAudit {
				continue
			}
		}
		if isCall {
			provenance = resolvedNativeProvenance(receiverInfo)
			if len(provenance) == 0 {
				provenance = []nativeProvenance{{}}
			}
			filtered := provenance[:0]
			for _, origin := range provenance {
				if origin.Local || origin.Selected {
					continue
				}
				if nativeProvenanceIsDirectCallable(origin) {
					continue
				}
				if origin.Result || origin.Declaration != nil || nativeReachable {
					origin.Path = appendNativePath(origin.Path, name)
					origin.Result = false
					filtered = append(filtered, origin)
				}
			}
			provenance = filtered
			if len(provenance) == 0 {
				continue
			}
		}
		var baseKey nativeCallableKey
		switch {
		case isCall:
			baseKey = nativeCallableKey{
				kind:     NativeCallableFunctionResult,
				receiver: receiverInfo.Type,
				name:     name,
			}
		case isIndex:
			baseKey = nativeCallableKey{
				kind:     NativeCallableIndexedFunction,
				receiver: receiverInfo.Type,
				name:     name,
			}
		case info.NativeMethod || info.MethodType != noMethod:
			baseKey = nativeCallableKey{
				kind:     NativeCallableMethod,
				receiver: receiverInfo.Type,
				name:     name,
			}
		case info.Type != nil && info.Type.Kind() == reflect.Func:
			structType := receiverInfo.Type
			if structType.Kind() == reflect.Pointer {
				structType = structType.Elem()
			}
			if structType.Kind() != reflect.Struct {
				continue
			}
			field, ok := structType.FieldByName(name)
			if !ok || field.Type.Kind() != reflect.Func {
				continue
			}
			baseKey = nativeCallableKey{
				kind:     NativeCallableFunctionField,
				receiver: structType,
				name:     field.Name,
			}
		default:
			continue
		}
		var path string
		if isSelector {
			path = selector.String()
		} else if isCall {
			path = call.String()
		} else {
			path = index.String()
		}
		if len(provenance) == 0 {
			provenance = []nativeProvenance{{}}
		}
		for _, origin := range provenance {
			key := baseKey
			key.declaration = origin.Declaration
			key.memberPath = origin.Path
			key.constructed = origin.Constructed
			callable, exists := byKey[key]
			if !exists || path < callable.Path {
				byKey[key] = NativeCallable{
					Kind:        key.kind,
					Receiver:    key.receiver,
					Name:        key.name,
					Path:        path,
					Declaration: origin.Declaration,
					MemberPath:  origin.Path,
					Synchronous: nativeCallableCertified(key),
					Constructed: origin.Constructed,
				}
			}
		}
	}
	callables := make([]NativeCallable, 0, len(byKey))
	for _, callable := range byKey {
		callables = append(callables, callable)
	}
	sort.Slice(callables, func(i, j int) bool {
		left, right := callables[i], callables[j]
		if left.Path != right.Path {
			return left.Path < right.Path
		}
		if left.Name != right.Name {
			return left.Name < right.Name
		}
		if left.Kind != right.Kind {
			return left.Kind < right.Kind
		}
		var leftPackage, leftDeclaration, rightPackage, rightDeclaration string
		if left.Declaration != nil {
			leftPackage = left.Declaration.Package
			leftDeclaration = left.Declaration.Name
		}
		if right.Declaration != nil {
			rightPackage = right.Declaration.Package
			rightDeclaration = right.Declaration.Name
		}
		if leftPackage != rightPackage {
			return leftPackage < rightPackage
		}
		if leftDeclaration != rightDeclaration {
			return leftDeclaration < rightDeclaration
		}
		if left.MemberPath != right.MemberPath {
			return left.MemberPath < right.MemberPath
		}
		return left.Receiver.String() < right.Receiver.String()
	})
	return callables
}

func nativeProvenanceIsDirectCallable(provenance nativeProvenance) bool {
	if provenance.Declaration == nil || provenance.Path != "" || provenance.Result {
		return false
	}
	return nativeDeclarationNeedsDirectCertificate(*provenance.Declaration)
}

func nativeDeclarationRoots(declaration NativeDeclaration) []reflect.Type {
	switch declaration.Kind {
	case NativeDeclarationVariable:
		typ := reflect.TypeOf(declaration.Declaration)
		if typ != nil && typ.Kind() == reflect.Pointer {
			return []reflect.Type{typ.Elem()}
		}
	case NativeDeclarationType:
		if typ, ok := declaration.Declaration.(reflect.Type); ok {
			return []reflect.Type{typ}
		}
	case NativeDeclarationFunction:
		function := declaration.Declaration
		if adaptive, ok := function.(native.AdaptiveFunc); ok {
			function = adaptive.Impl
		}
		typ := reflect.TypeOf(function)
		if typ != nil && typ.Kind() == reflect.Func {
			roots := make([]reflect.Type, typ.NumOut())
			for index := range typ.NumOut() {
				roots[index] = typ.Out(index)
			}
			return roots
		}
	}
	return nil
}

func addNativeReachableTypes(types map[reflect.Type]struct{}, typ reflect.Type) {
	if typ == nil {
		return
	}
	if _, exists := types[typ]; exists {
		return
	}
	types[typ] = struct{}{}
	switch typ.Kind() {
	case reflect.Array, reflect.Chan, reflect.Pointer, reflect.Slice:
		addNativeReachableTypes(types, typ.Elem())
	case reflect.Func:
		for index := range typ.NumOut() {
			addNativeReachableTypes(types, typ.Out(index))
		}
	case reflect.Map:
		addNativeReachableTypes(types, typ.Key())
		addNativeReachableTypes(types, typ.Elem())
	case reflect.Struct:
		for index := range typ.NumField() {
			addNativeReachableTypes(types, typ.Field(index).Type)
		}
	}
}

func isNativeReachableType(types map[reflect.Type]struct{}, typ reflect.Type) bool {
	if _, ok := types[typ]; ok {
		return true
	}
	if typ.Kind() == reflect.Pointer {
		_, ok := types[typ.Elem()]
		return ok
	}
	return false
}

func nativeCallableCertified(callable nativeCallableKey) bool {
	declaration := callable.declaration
	if declaration == nil || !declaration.Synchronous || callable.memberPath == "" {
		return false
	}
	if callable.constructed && (callable.kind != NativeCallableMethod || callable.receiver.Kind() == reflect.Interface) {
		return false
	}
	for _, pattern := range declaration.SynchronousMembers {
		if !nativeMemberPatternMatches(pattern, callable.memberPath) {
			continue
		}
		if nativeDeclarationSelectsCallable(*declaration, callable) {
			return true
		}
	}
	return false
}

func nativeDeclarationSelectsCallable(declaration NativeDeclaration, callable nativeCallableKey) bool {
	roots := nativeDeclarationRoots(declaration)
	if declaration.Kind == NativeDeclarationFunction && len(roots) > 1 {
		resultIndex, memberPath, ok := splitNativeResultMemberPath(callable.memberPath)
		if !ok || resultIndex >= len(roots) {
			return false
		}
		callable.memberPath = memberPath
		return nativeDeclarationPathSelectsCallable(roots[resultIndex], callable)
	}
	for _, root := range roots {
		if nativeDeclarationPathSelectsCallable(root, callable) {
			return true
		}
	}
	return false
}

func splitNativeResultMemberPath(path string) (int, string, bool) {
	if len(path) < 5 || path[0] != '[' {
		return 0, "", false
	}
	closing := strings.IndexByte(path, ']')
	if closing <= 1 || closing+2 >= len(path) || path[closing+1] != '.' {
		return 0, "", false
	}
	index, err := strconv.Atoi(path[1:closing])
	if err != nil || index < 0 {
		return 0, "", false
	}
	return index, path[closing+2:], true
}

func nativeMemberPatternMatches(pattern, memberPath string) bool {
	patternParts := strings.Split(pattern, ".")
	pathParts := strings.Split(memberPath, ".")
	if len(patternParts) != len(pathParts) || len(patternParts) == 0 {
		return false
	}
	for index, part := range patternParts {
		if part == "*" && index < len(patternParts)-1 {
			continue
		}
		if part != pathParts[index] {
			return false
		}
	}
	return true
}

func nativeDeclarationPathSelectsCallable(root reflect.Type, callable nativeCallableKey) bool {
	parts := strings.Split(callable.memberPath, ".")
	if len(parts) == 0 {
		return false
	}
	typ := root
	for _, member := range parts[:len(parts)-1] {
		if name, resultIndex, indexed := splitNativeCallableResult(member); indexed {
			var ok bool
			typ, ok = nativeCallableResultType(typ, name, resultIndex)
			if !ok {
				return false
			}
			continue
		}
		for typ.Kind() == reflect.Pointer {
			typ = typ.Elem()
		}
		switch typ.Kind() {
		case reflect.Array, reflect.Slice:
			if len(member) < 2 || member[0] != '[' || member[len(member)-1] != ']' {
				return false
			}
			typ = typ.Elem()
		case reflect.Map:
			typ = typ.Elem()
		case reflect.Struct:
			field, ok := nativeFieldBySelectorName(typ, member)
			if !ok {
				return false
			}
			typ = field.Type
		default:
			return false
		}
	}
	name := parts[len(parts)-1]
	switch callable.kind {
	case NativeCallableMethod:
		if callable.receiver != typ {
			if typ.Kind() == reflect.Pointer || callable.receiver != reflect.PointerTo(typ) {
				return false
			}
			typ = callable.receiver
		}
		method, ok := typ.MethodByName(name)
		return ok && method.Name == callable.name
	case NativeCallableFunctionField:
		for typ.Kind() == reflect.Pointer {
			typ = typ.Elem()
		}
		if typ.Kind() != reflect.Struct || typ != callable.receiver {
			return false
		}
		field, ok := nativeFieldBySelectorName(typ, name)
		return ok && field.Type.Kind() == reflect.Func && field.Name == callable.name
	}
	return false
}

func splitNativeCallableResult(member string) (string, int, bool) {
	opening := strings.LastIndexByte(member, '[')
	if opening <= 0 || len(member) < opening+3 || member[len(member)-1] != ']' {
		return "", 0, false
	}
	index, err := strconv.Atoi(member[opening+1 : len(member)-1])
	if err != nil || index < 0 {
		return "", 0, false
	}
	return member[:opening], index, true
}

func nativeCallableResultType(typ reflect.Type, name string, resultIndex int) (reflect.Type, bool) {
	if method, ok := typ.MethodByName(name); ok {
		if resultIndex < method.Type.NumOut() {
			return method.Type.Out(resultIndex), true
		}
		return nil, false
	}
	if typ.Kind() != reflect.Pointer && typ.Kind() != reflect.Interface {
		if method, ok := reflect.PointerTo(typ).MethodByName(name); ok {
			if resultIndex < method.Type.NumOut() {
				return method.Type.Out(resultIndex), true
			}
			return nil, false
		}
	}
	for typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}
	var callable reflect.Type
	switch typ.Kind() {
	case reflect.Map:
		callable = typ.Elem()
	case reflect.Struct:
		field, ok := nativeFieldBySelectorName(typ, name)
		if !ok {
			return nil, false
		}
		callable = field.Type
	default:
		return nil, false
	}
	if callable.Kind() != reflect.Func || resultIndex >= callable.NumOut() {
		return nil, false
	}
	return callable.Out(resultIndex), true
}

func nativeFieldBySelectorName(typ reflect.Type, name string) (reflect.StructField, bool) {
	if field, ok := typ.FieldByName(name); ok {
		return field, true
	}
	for _, field := range reflect.VisibleFields(typ) {
		tagName, _, _ := strings.Cut(field.Tag.Get("json"), ",")
		if tagName == name && tagName != "" && tagName != "-" {
			return field, true
		}
	}
	return reflect.StructField{}, false
}

// emitProgram emits the code for a program given its ast node, the type info
// and indirect variables. emitProgram returns an emittedPackage  instance
// with the global variables and the main function.
func emitProgram(pkgMain *ast.Package, typeInfos map[ast.Node]*typeInfo, indirectVars map[*ast.Identifier]bool) (_ *Code, err error) {
	defer func() {
		if r := recover(); r != nil {
			if e, ok := r.(*LimitExceededError); ok {
				err = e
				return
			}
			panic(r)
		}
	}()
	e := newEmitter(typeInfos, nil, indirectVars, nil)
	functions, _, _ := e.emitPackage(pkgMain, false, "main")
	main, _ := e.fnStore.availableScriggoFn(pkgMain, "main")
	nativeDeclarations := usedNativeDeclarations(typeInfos)
	nativeCallables := usedNativeCallables(typeInfos, nativeDeclarations)
	nativeValueAccesses := usedNativeValueAccesses(pkgMain, typeInfos)
	main.BatchUnsafeNative = batchUnsafeNative(nativeDeclarations, nativeCallables)
	globals := e.varStore.getGlobals()
	setInternalFunctionGlobals(main, globals)
	pkg := &Code{
		Globals:             globals,
		NativeDeclarations:  nativeDeclarations,
		NativeCallables:     nativeCallables,
		NativeValueAccesses: nativeValueAccesses,
		Functions:           functions,
		Main:                main,
		TypeOf:              e.types.TypeOf,
	}
	return pkg, nil
}

// emitTemplate emits the code for a template given its tree, the type info and
// indirect variables. emitTemplate returns a function that is the entry point
// of the template and the global variables.
// If canonicalGlobals is provided (non-nil), it will be used to ensure consistent
// global variable indexing for dynamic template compilation.
func emitTemplate(tree *ast.Tree, typeInfos map[ast.Node]*typeInfo, indirectVars map[*ast.Identifier]bool, formatTypes map[ast.Format]reflect.Type, canonicalGlobals []Global) (_ *Code, err error) {
	// Recover and eventually return a LimitExceededError.
	defer func() {
		if r := recover(); r != nil {
			if e, ok := r.(*LimitExceededError); ok {
				err = e
				return
			}
			panic(r)
		}
	}()
	e := newEmitter(typeInfos, formatTypes, indirectVars, canonicalGlobals)
	e.pkg = &ast.Package{}
	e.isTemplate = true
	typ := reflect.FuncOf(nil, nil, false)
	e.fb = newBuilder(newMacro("main", "main", typ, tree.Format, tree.Path, tree.Pos()), tree.Path)
	e.fb.changePath(tree.Path)
	e.fb.enterScope()
	e.emitNodes(tree.Nodes)
	e.fb.exitScope()
	e.fb.end()
	nativeDeclarations := usedNativeDeclarations(typeInfos)
	nativeCallables := usedNativeCallables(typeInfos, nativeDeclarations)
	nativeValueAccesses := usedNativeValueAccesses(tree, typeInfos)
	e.fb.fn.BatchUnsafeNative = batchUnsafeNative(nativeDeclarations, nativeCallables)
	globals := e.varStore.getGlobals()
	setInternalFunctionGlobals(e.fb.fn, globals)
	return &Code{
		Main:                e.fb.fn,
		TypeOf:              e.types.TypeOf,
		Globals:             globals,
		NativeDeclarations:  nativeDeclarations,
		NativeCallables:     nativeCallables,
		NativeValueAccesses: nativeValueAccesses,
	}, nil
}

func setInternalFunctionGlobals(fn *runtime.Function, globals []Global) {
	var internal []bool
	for index, global := range globals {
		if global.Native || global.Value.IsValid() || global.Type.Kind() != reflect.Func {
			continue
		}
		if internal == nil {
			internal = make([]bool, len(globals))
		}
		internal[index] = true
	}
	fn.InternalFunctionGlobals = internal
}

// isExported reports whether name is exported, according to
// https://go.dev/ref/spec#Exported_identifiers.
// It panics if name is empty.
func isExported(name string) bool {
	if c := name[0]; c < utf8.RuneSelf {
		return 'A' <= c && c <= 'Z'
	}
	r, _ := utf8.DecodeRuneInString(name)
	return unicode.Is(unicode.Lu, r)
}

// getExtends returns the 'extends' node contained in nodes, if exists. Note
// that such node can only be preceded by a comment node or a text node.
func getExtends(nodes []ast.Node) (*ast.Extends, bool) {
	for _, node := range nodes {
		switch n := node.(type) {
		case *ast.Comment, *ast.Text:
		case *ast.Import:
			// This is the import declaration added when transforming the tree
			// in case of extends. It must be skipped because it is added
			// before the 'extends' declaration.
		case *ast.Extends:
			return n, true
		case *ast.Statements:
			if len(n.Nodes) > 0 {
				n, ok := n.Nodes[0].(*ast.Extends)
				return n, ok
			}
			return nil, false
		default:
			return nil, false
		}
	}
	return nil, false
}

// internalError returns a string that can be used as panic argument in case of
// a Scriggo internal error.
//
// It may be used as:
//
//	panic(internalError(format, a...))
//
// Keep in sync with types.internalError.
func internalError(format string, a ...any) string {
	return fmt.Sprintf("scriggo: internal error: "+format, a...)
}
