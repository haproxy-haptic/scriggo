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
	"reflect"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/open2b/scriggo/ast"
	"github.com/open2b/scriggo/internal/runtime"
	"github.com/open2b/scriggo/native"
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
	AllowGoStmt          bool
	NoParseShortShowStmt bool

	FormatTypes map[ast.Format]reflect.Type
	Globals     native.Declarations

	// Importer imports the native packages.
	Importer native.Importer

	// MDConverter converts a Markdown source code to HTML.
	MDConverter Converter

	TreeTransformer func(*ast.Tree) error

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
// This is a helper that contains the common compilation logic used by BuildTemplate
// and buildDynamicMacros to avoid code duplication.
// If canonicalGlobals is provided (non-nil), it will be used to ensure consistent
// global variable indexing for dynamic template compilation.
func compileTemplateFile(fsys fs.FS, path string, opts Options, canonicalGlobals []Global) (*Code, error) {
	// Parse the source code.
	tree, err := ParseTemplate(fsys, path, opts.NoParseShortShowStmt)
	if err != nil {
		return nil, err
	}

	// Transform the tree if transformer is provided.
	if opts.TreeTransformer != nil {
		if err := opts.TreeTransformer(tree); err != nil {
			return nil, err
		}
	}

	// Type check the tree.
	checkerOpts := checkerOptions{
		allowGoStmt: opts.AllowGoStmt,
		formatTypes: opts.FormatTypes,
		globals:     opts.Globals,
		mdConverter: opts.MDConverter,
		mod:         templateMod,
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

	// Transform the tree.
	if opts.TreeTransformer != nil {
		err := opts.TreeTransformer(tree)
		if err != nil {
			return nil, err
		}
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
		for node, ti := range pkgInfos.TypeInfos {
			typeInfos[node] = ti
		}
	}

	// Emit the code.
	code, err := emitProgram(tree.Nodes[0].(*ast.Package), typeInfos, tci["main"].IndirectVars)
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

	// Compile all other templates in the filesystem for dynamic includes.
	// This allows {% include variable %} to work at runtime.
	// Pass main template's globals so dynamic templates use the same indices.
	result, err := buildDynamicMacros(fsys, name, opts, code.Globals)
	if err != nil {
		return nil, err
	}

	// Update code with the unified globals and dynamic macros.
	code.DynamicMacros = result.Macros
	code.Globals = result.Globals
	code.ProfilingEnabled = opts.EnableProfiling

	return code, nil
}

// buildDynamicMacros compiles all templates in the filesystem (except the main template)
// and returns a result containing compiled functions and unified globals for dynamic includes.
// canonicalGlobals contains the globals from the main template compilation which ensures
// dynamic templates use consistent global variable indices.
func buildDynamicMacros(fsys fs.FS, mainName string, opts Options, canonicalGlobals []Global) (dynamicMacrosResult, error) {
	macros := make(map[string]*runtime.Function)

	// Initialize with main template's globals
	allGlobals := make([]Global, len(canonicalGlobals))
	copy(allGlobals, canonicalGlobals)

	// Track known globals by name for fast lookup
	globalIndex := make(map[string]int16, len(canonicalGlobals))
	for i, g := range canonicalGlobals {
		globalIndex[globalKey(g.Pkg, g.Name)] = int16(i)
	}

	// Walk the filesystem to find all template files
	err := fs.WalkDir(fsys, ".", func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			return nil
		}

		// Skip the main template (already compiled)
		if path == mainName {
			return nil
		}

		// Skip hidden files and files in hidden directories
		if isHiddenPath(path) {
			return nil
		}

		// Try to compile this file as a template.
		// If it fails (not a valid template), silently skip it.
		// Pass current allGlobals as canonical so this template uses consistent indices.
		code, compileErr := compileTemplateFile(fsys, path, opts, allGlobals)
		if compileErr != nil {
			// Skip files that fail to compile (not valid templates)
			return nil
		}

		// Check for any new globals this template added.
		// New globals are those with indices >= len(allGlobals) at compile time.
		for i := len(allGlobals); i < len(code.Globals); i++ {
			g := code.Globals[i]
			key := globalKey(g.Pkg, g.Name)
			if _, exists := globalIndex[key]; !exists {
				globalIndex[key] = int16(len(allGlobals))
				allGlobals = append(allGlobals, g)
			}
		}

		// Store the compiled function
		macros[path] = code.Main
		return nil
	})

	if err != nil {
		return dynamicMacrosResult{}, err
	}

	return dynamicMacrosResult{Macros: macros, Globals: allGlobals}, nil
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
	Pkg   string
	Name  string
	Type  reflect.Type
	Value reflect.Value
}

// globalKey returns the unique key for a global variable, combining package and name.
func globalKey(pkg, name string) string {
	return pkg + "." + name
}

// dynamicMacrosResult holds the result of building dynamic macros.
type dynamicMacrosResult struct {
	// Macros maps template paths to compiled functions for dynamic includes.
	Macros map[string]*runtime.Function
	// Globals is the unified list of globals from main and all dynamic templates.
	Globals []Global
}

// Code is the result of a package emitting process.
type Code struct {
	// Globals is a slice of all globals used in Code.
	Globals []Global
	// Functions is a map of exported functions indexed by name.
	Functions map[string]*runtime.Function
	// Main is the Code entry point.
	Main *runtime.Function
	// TypeOf returns the type of a value, including new types defined in code.
	TypeOf runtime.TypeOfFunc
	// DynamicMacros maps template paths to compiled functions for dynamic includes.
	// This map is populated at compile time and used at runtime for {% include variable %}.
	DynamicMacros map[string]*runtime.Function
	// ProfilingEnabled indicates whether profiling was enabled at build time.
	ProfilingEnabled bool
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
	pkg := &Code{
		Globals:   e.varStore.getGlobals(),
		Functions: functions,
		Main:      main,
		TypeOf:    e.types.TypeOf,
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
	return &Code{Main: e.fb.fn, TypeOf: e.types.TypeOf, Globals: e.varStore.getGlobals()}, nil
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
func internalError(format string, a ...interface{}) string {
	return fmt.Sprintf("scriggo: internal error: "+format, a...)
}
