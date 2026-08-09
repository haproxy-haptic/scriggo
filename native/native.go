// Copyright 2019 The Scriggo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package native provides types to implement native variables, constants,
// functions, types and packages that can be imported or used as builtins in
// programs and templates.
package native

import (
	"context"
	"io"
	"reflect"
)

type (

	// HTML is the html type in templates.
	HTML string

	// CSS is the css type in templates.
	CSS string

	// JS is the js type in templates.
	JS string

	// JSON is the json type in templates.
	JSON string

	// Markdown is the markdown type in templates.
	Markdown string
)

// Converter is implemented by format converters.
type Converter = func(src []byte, out io.Writer) error

// Env represents an execution environment.
//
// Each execution creates an [Env] value. This value is passed as the first
// argument to calls to native functions and methods that have [Env] as the type
// of the first parameter.
type Env interface {

	// CallPath returns the path, relative to the root, of the call site of
	// the caller function. If it is not called by the main goroutine, the
	// returned value is not significant.
	CallPath() string

	// CallLine returns the 1-based line, within CallPath's file, of the call
	// site of the caller function, or 0 if unknown. Like CallPath, it is only
	// significant when called by the main goroutine.
	CallLine() int

	// Context returns the context of the execution.
	// It is the context passed as an option for execution.
	Context() context.Context

	// Fatal exits the execution and then panics with value v. Deferred
	// functions are not called and started goroutines are not terminated.
	Fatal(v any)

	// MarkdownConverter returns the Markdown converter provided to the
	// BuildTemplate function, if one was set.
	MarkdownConverter() Converter

	// Print calls the print built-in function with args as argument.
	Print(args ...any)

	// Println calls the println built-in function with args as argument.
	Println(args ...any)

	// Stop stops the execution with the given error. Deferred functions are
	// not called and started goroutines are not terminated.
	Stop(err error)

	// TypeOf is like reflect.TypeOf but if v has a Scriggo type it returns
	// its Scriggo reflect type instead of the reflect type of the proxy.
	TypeOf(v reflect.Value) reflect.Type
}

type (

	// EnvStringer is like fmt.Stringer where the String method takes an [Env]
	// parameter.
	EnvStringer interface {
		String(Env) string
	}

	// HTMLStringer is implemented by values that are not escaped in HTML context.
	HTMLStringer interface {
		HTML() HTML
	}

	// HTMLEnvStringer is like [HTMLStringer] where the HTML method takes a
	// [Env] parameter.
	HTMLEnvStringer interface {
		HTML(Env) HTML
	}

	// CSSStringer is implemented by values that are not escaped in CSS context.
	CSSStringer interface {
		CSS() CSS
	}

	// CSSEnvStringer is like [CSSStringer] where the CSS method takes an [Env]
	// parameter.
	CSSEnvStringer interface {
		CSS(Env) CSS
	}

	// JSStringer is implemented by values that are not escaped in JavaScript
	// context.
	JSStringer interface {
		JS() JS
	}

	// JSEnvStringer is like [JSStringer] where the JS method takes an [Env]
	// parameter.
	JSEnvStringer interface {
		JS(Env) JS
	}

	// JSONStringer is implemented by values that are not escaped in JSON
	// context.
	JSONStringer interface {
		JSON() JSON
	}

	// JSONEnvStringer is like [JSONStringer] where the JSON method takes an
	// [Env] parameter.
	JSONEnvStringer interface {
		JSON(Env) JSON
	}

	// MarkdownStringer is implemented by values that are not escaped in
	// Markdown context.
	MarkdownStringer interface {
		Markdown() Markdown
	}

	// MarkdownEnvStringer is like [MarkdownStringer] where the Markdown method
	// takes a [Env] parameter.
	MarkdownEnvStringer interface {
		Markdown(Env) Markdown
	}
)

// Declaration represents a declaration.
//
//   - for a variable: a pointer to the value of the variable
//   - for a function: the function
//   - for a type: its reflect.Type value
//   - for a typed constant: its value as a string, boolean or numeric value
//   - for an untyped constant: an [UntypedStringConst], [UntypedBooleanConst] or [UntypedNumericConst] value
//   - for a package: an [ImportablePackage] value (used only for template globals)
type Declaration any

// Declarations represents a set of variables, constants, functions, types and
// packages declarations and can be used for template globals and package
// declarations.
//
// The key is the declaration's name and the element is its value.
type Declarations map[string]Declaration

type (
	// UntypedStringConst represents an untyped string constant.
	UntypedStringConst string

	// UntypedBooleanConst represents an untyped boolean constant.
	UntypedBooleanConst bool

	// UntypedNumericConst represents an untyped numeric constant.
	UntypedNumericConst string
)

// AdaptiveFunc declares a native function whose static return type at
// each call site is computed by a closure from the static types of the
// call's arguments. Use this for slice-operation filters that should
// preserve the element type of their input — without this, the
// declared concrete return type (typically `[]any` for polymorphic
// filters) erases useful type information at the call boundary.
//
// Behaviour:
//
//   - Argument count and concrete assignability are checked against
//     Impl's reflect.Func signature, exactly as for a plain native
//     function. The closure does NOT influence argument validation.
//   - The static return type at each call site is the value returned
//     by ReturnType(argTypes), where argTypes is the list of the
//     call's actual argument static types. Impl's declared return
//     type is ignored for this purpose.
//   - At render time Impl is invoked with the call's arguments boxed
//     in `any`. Impl should use reflection to construct a return
//     value matching the static type ReturnType promised at type-check
//     time. Returning a mismatched type at runtime triggers a panic
//     in Scriggo's call-result handling.
//   - If ReturnType returns a non-nil error, the type-check fails at
//     the call site with that error as the diagnostic.
//
// Example:
//
//	// shard_slice — return type matches the input slice's static type.
//	var ShardSlice = native.AdaptiveFunc{
//	    Impl: func(items any, shardIndex, totalShards int) any { /* … */ },
//	    ReturnType: func(argTypes []reflect.Type) (reflect.Type, error) {
//	        return argTypes[0], nil
//	    },
//	}
type AdaptiveFunc struct {
	// Impl is the runtime implementation. Must be a reflect.Func.
	// Argument count and concrete assignability are validated against
	// its declared signature; its declared return type is replaced by
	// what ReturnType produces.
	Impl any

	// LambdaParams supplies the parameter types of a lambda argument, so a
	// caller can write `filter(items, e => e.Ready)` and have `e` typed from
	// what `items` is.
	//
	// It is called once per call site, for each argument that is a lambda with
	// no declared parameter types, with the static types of the arguments
	// already resolved to its left (later ones are nil). Return nil to leave
	// the lambda uninferable, which surfaces as a "cannot infer" diagnostic at
	// the lambda rather than a confusing error inside its body.
	//
	// The lambda's RESULT type is never supplied here — the checker infers it
	// from the returned expression.
	LambdaParams func(argIndex int, resolved []reflect.Type) []reflect.Type

	// ReturnType is the static-type computation. Called once per call
	// site by the type-checker. argTypes contains the static types
	// of the call's actual arguments, in order. The returned
	// reflect.Type becomes the call expression's static return type.
	// Return (nil, err) to surface err as the call site's compile
	// diagnostic.
	ReturnType func(argTypes []reflect.Type) (reflect.Type, error)
}
