# Scriggo Generics — Design

Status: **Draft, awaiting review.** Phase 1 implementation begins after the open
decision points in §6 are resolved.

## 1. Background

Schema-driven template engines (chart libraries, K8s configuration renderers,
GitOps tools) declare runtime data as strongly-typed Go values exposed through
`native.Declarations`. Templates iterate the data and emit configuration.
Today, every filter that operates on a slice of unknown element type erases
the static type to `[]any`:

```go
// Today's shape, per pkg/templating/filters_collection.go (haptic):
func scriggoShardSlice(items any, shardIndex, totalShards int) []any
```

A call site like `shard_slice(resources.gateways.List(), i, n)` — where the
first argument is statically `[]*Gateway` — produces a result whose static
type is `[]any`. Downstream template code can no longer use typed field
access (`gw.Metadata.Name`); it must fall back to reflection-based dig
helpers or per-element type assertions. Field access goes from a typed
bytecode dispatch to a reflection call, and the benchmark in MR !92
quantifies the cost: **~6× slower, ~12× more allocations** at 100 items.

## 2. What we don't need from Go generics

Go's generics design (proposal 43651) is a complete language feature with
type parameters, type sets, constraint type inference, generic types,
generic methods, and a runtime instantiation strategy (GC-shape stenciling).
Scriggo's use case is narrower:

| Go feature | Need? | Why not |
|---|---|---|
| Type parameter declarations in templates (`macro Foo[T any]`) | **Phase 2 only**, and even then input parameters only | Macros return text/format types, not values; output never needs type-parameterisation |
| Type sets / union constraints (`~int \| ~string`) | No | Template filters never branch on numeric / underlying type |
| Approximation (`~T`) | No | No use case |
| Constraint type inference | No | No constraints means no inference between them |
| Generic types (`type Stack[T any] struct`) | No | Templates don't declare types |
| Generic methods | No | Same as above |
| Specialisation | No | One implementation per generic function, no per-type override |
| Runtime introspection of type parameters | No | Reflection in the native function body already handles polymorphism |
| Monomorphisation / GC-shape stenciling / dictionary passing | No | The native function uses reflection internally; the bytecode never sees the type parameter at runtime |

The reduction is large: most of Go generics' surface area, design complexity,
and implementation cost can be cut.

## 3. What we do need

| Feature | Justification |
|---|---|
| Native Go functions can declare themselves as generic | The Go-side native bridge needs to carry "my return type is `[]T` where T is the element type of arg 0" — expressed as a parametric signature, not a hardcoded "first-arg" annotation |
| Type-checker performs function-argument type inference at call sites | When the template writes `shard_slice(typedSlice, i, n)`, the type-checker unifies the call args with the parametric signature and solves T |
| Substitution produces a statically-known return type | The call expression's static type is `[]*Gateway`, so downstream macro calls accept it |
| Restrict constraints to `any` | Sufficient for every filter use case identified so far. Trivial inference (no type-set reasoning) |
| Single type parameter per filter for Phase 1 | Every chart filter we identified is shaped "T flows from input slice to output slice"; multi-parameter filters are out of scope until a real case appears |
| Generic macros (Phase 2) take type parameters on input only | Macros emit text; type-parameterising the input parameter list is enough |

## 4. Phased plan

### Phase 1 — Native generic functions

**Scope:** Go-side native functions can be declared as generic. Scriggo's
type-checker honours their parametric signatures at call sites. Templates
themselves do not gain generic syntax in this phase.

**Surface area changed:**
- `native/native.go`: new declaration shape `GenericFunc`.
- A small parametric signature parser (`internal/genericsig`): produces a typed
  signature tree from a Go string like `(s []T, i int, n int) []T`.
- `internal/compiler/checker_expressions.go::checkCall`: branch for
  `GenericFunc` declarations — invoke the unifier, substitute, set the call
  expression's static return type.
- `internal/genericsig/unify.go`: unification algorithm.
- Downstream consumers (e.g. haptic): convert their existing filter
  declarations from concrete `func(any, ...) []any` to `native.GenericFunc`.

**Out of scope for Phase 1:**
- Template-side generic declaration syntax (Phase 2)
- Constraints beyond `any` (no future phase — not needed)
- Generic types (not on roadmap)

### Phase 2 — Generic macros

**Scope:** Macros can declare type parameters between the macro name and the
parameter list:

```scriggo
{% macro RenderRows[T any](items []*T) %}
  {%- for _, it := range items %}
    <tr><td>{{ it.Name }}</td></tr>
  {%- end %}
{% end %}
```

**Surface area changed:**
- Parser: accept `[TypeParamList]` between macro name and `(`.
- AST: `TypeParams []TypeParam` field on macro / func nodes.
- Type-checker: enter type-parameter scope when checking the macro body;
  substitute at call sites. Reuses the Phase 1 unifier.
- Macro return type stays format-typed (text / html / markdown). Templates
  don't gain value-returning generic macros.

## 5. Phase 1 implementation sketch

### 5.1 Native declaration shape

```go
// native/generic.go (new file)

// GenericFunc declares a native function whose signature contains type
// parameters. Scriggo's type-checker substitutes the parameters at every
// call site by unifying the call arguments against the declared signature.
//
// The Impl is invoked at runtime with arguments boxed as reflect.Values.
// Type information is erased at the call boundary — the Impl uses
// reflection on its inputs to handle polymorphism. The type-checker
// guarantees that the static return type matches what the template
// downstream expects.
type GenericFunc struct {
    // TypeParams lists the type parameter names introduced by this
    // function. Each parameter is implicitly constrained to `any`.
    TypeParams []string

    // Signature is a string in the parametric signature mini-language
    // (see internal/genericsig for the grammar). Example:
    //
    //   "(s []T, i int, n int) []T"
    //
    // T must appear in TypeParams. The signature is parsed once at
    // engine init and cached.
    Signature string

    // Impl is the runtime implementation. Arguments arrive boxed in
    // reflect.Value (since the bytecode passes typed values through
    // interface). The Impl returns the appropriate reflect.Value(s).
    Impl func(args []reflect.Value) []reflect.Value
}
```

### 5.2 Parametric signature mini-language

Grammar:

```
Signature  = "(" ParamList ")" [ ResultType ]
ParamList  = Param ("," Param)*
Param      = ident " " TypeRef
TypeRef    = ident
           | "[" "]" TypeRef
           | "*" TypeRef
           | "map" "[" TypeRef "]" TypeRef
ResultType = TypeRef
```

Type-parameter identifiers (those in `TypeParams`) are leaf names. Built-in
type names (`int`, `string`, `bool`, etc.) are also leaf names.

Deliberately omitted vs Go: no function types in signatures, no channels,
no interfaces, no struct literals, no qualified types. The filter use case
doesn't need them.

### 5.3 Unification algorithm

Input: call's static argument types + parsed parametric signature.
Output: substitution map `{T → static type}` for each type parameter.

Algorithm:

```
unify(formal, actual, subst):
    if formal is a type parameter T:
        if T already in subst:
            if subst[T] != actual: fail
        else:
            subst[T] = actual
    elif formal is a slice of FormalElem and actual is a slice of ActualElem:
        unify(FormalElem, ActualElem, subst)
    elif formal is a pointer to FormalElem and actual is a pointer to ActualElem:
        unify(FormalElem, ActualElem, subst)
    elif formal is a map[FormalKey]FormalVal and actual is a map[ActualKey]ActualVal:
        unify(FormalKey, ActualKey, subst)
        unify(FormalVal, ActualVal, subst)
    elif formal == actual (built-in or named):
        ok
    else:
        fail
```

Single pass (no iteration to a fixed point — Go's constraint type inference
isn't needed because we have no constraints to derive types from).

### 5.4 Substitution

Walk the parsed signature's result type, replacing each type parameter with
its solved binding. Build a `reflect.Type` for the substituted result type.

### 5.5 Type-checker integration

In `checker_expressions.go`'s call-expression branch:

```
if callee is a GenericFunc declaration:
    formalSig = lookup parsed signature
    for i, arg := range call.Args:
        actualType = checkExpr(arg).Type
        unify(formalSig.Params[i].Type, actualType, subst)
    callExpr.StaticType = substitute(formalSig.Result, subst)
    return
```

### 5.6 Runtime

No bytecode changes. The Impl is invoked via the existing native-function
calling convention. Arguments are passed as `reflect.Value`; the Impl uses
`reflect.Type.Elem()` etc. to handle the actual types.

### 5.7 Filter conversions (downstream)

A representative conversion in haptic:

```go
// Before
func scriggoShardSlice(items any, shardIndex, totalShards int) []any {
    itemsSlice, ok := toSlice(items)
    if !ok || len(itemsSlice) == 0 {
        return []any{}
    }
    // ... partitioning logic, returns []any of original items ...
}

// After
var shardSliceGeneric = native.GenericFunc{
    TypeParams: []string{"T"},
    Signature:  "(s []T, shardIndex int, totalShards int) []T",
    Impl: func(args []reflect.Value) []reflect.Value {
        items := args[0]                              // reflect.Value wrapping []T
        shardIndex := int(args[1].Int())
        totalShards := int(args[2].Int())
        sliceType := items.Type()                      // []T (concrete element type)
        // ... reflection-based partitioning, return reflect.Value wrapping []T ...
        return []reflect.Value{result}
    },
}
```

The downstream `decl["shard_slice"] = shardSliceGeneric` declaration replaces
the existing `decl["shard_slice"] = scriggoShardSlice`.

## 6. Open decision points

Surface to user before implementation:

1. **Parametric signature language: string-parsed vs Go-API-built?**
   - String-parsed (proposed): `Signature: "(s []T, i int, n int) []T"` — tiny
     mini-language, reads naturally, parser is ~150 lines.
   - Go-API-built: `Params: []native.Param{{Name:"s", Type: native.SliceOf(native.TypeParam("T"))}, ...}` —
     verbose at declaration site, type-safe at Go compile time.

2. **Multiple type parameters per function — needed for Phase 1 or defer?**
   The chart's filter set today is uniformly single-parameter. `coalesce(a T,
   b T) T` is single-parameter. `convertMapValues[K, V, V2 any](m map[K]V,
   f func(V) V2) map[K]V2` would need two — but no chart code does this.
   Phase 1 could restrict to single parameter; Phase 1.5 (later) adds
   multi-parameter when a real case appears.

3. **Type-parameter implicit `any` constraint — is that the right default?**
   Phase 1 has no constraint vocabulary. Every type parameter is implicitly
   `any`. Is that a sustainable default if Phase 2 / future needs introduce
   constrained type parameters? Risk: code written against the implicit
   default would need updating. Mitigation: introduce explicit constraint
   syntax in Phase 2 only when needed; existing declarations keep their
   implicit `any` until then.

4. **Error messages when unification fails.** Proposed: surface "cannot
   infer T: shard_slice expects `[]T` as first argument, got `map[string]int`".
   Anything else?

5. **Generic macros (Phase 2) — required, or stretch?** Phase 1 unblocks the
   filter pipeline. Phase 2 unblocks polymorphic chart macros like
   `GenerateRouteStatusPatches(routes []*T)` where T is `HTTPRoute` or
   `GRPCRoute` or `TLSRoute`. Worth doing immediately after Phase 1, or
   defer until evidence the chart's polymorphic macros are worth typing?

6. **Variadic generic parameters?** `coalesce(values ...T) T` is the
   archetype. Phase 1 proposes to skip; if a chart has a real case, Phase 1.5
   adds it.

## 7. Benchmark gates

Each phase merges only if benchmarks show improvement or zero regression
against the baseline in MR !92:

- `TypedFieldAccess100` — should stay flat or improve (no change to typed
  bytecode path).
- `AssertFieldAccess100` — should stay flat (no change to type-assertion
  path).
- `ReflectFieldAccess100` — speedup expected after downstream conversions
  push filter outputs onto the typed path. **Target: approach the
  TypedFieldAccess100 floor.**

A new benchmark case `FilteredTypedFieldAccess100` lands with the first
filter conversion (Phase 1's haptic-side bump). It feeds a typed slice
through `shard_slice` and accesses fields on the typed result. Goal:
indistinguishable from `TypedFieldAccess100` within noise.

Regression on any case triggers rollback, not "merge anyway".

## 8. References

- Go generics proposal: <https://go.googlesource.com/proposal/+/refs/heads/master/design/43651-type-parameters.md>
- Go spec, type parameter declarations: <https://go.dev/ref/spec#Type_parameter_declarations>
- Scriggo benchmark baseline: MR !92 (commit `172edcda`).
- The selector-chain-as-type extension (MR !91) is independent and lands first;
  generics build on top of it for the `*resources.X.T` syntax in chart macros.
