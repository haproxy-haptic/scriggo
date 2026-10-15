// Copyright 2019 The Scriggo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime

import (
	"bytes"
	"errors"
	"fmt"
	"html"
	"io"
	"reflect"
	goruntime "runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/haproxy-haptic/scriggo/ast"
	"github.com/haproxy-haptic/scriggo/native"
	"golang.org/x/sync/errgroup"
)

var byteSliceType = reflect.TypeFor[[]byte]()

var errInvalidTextFragmentWriteCount = errors.New("text fragment returned an invalid write count")

// stringBuilderPool is a pool of strings.Builder for reuse in rendering operations.
var stringBuilderPool = newRetainedPool(256, func() *strings.Builder {
	return &strings.Builder{}
})

// putStringBuilder returns a builder to the pool holding nothing.
//
// Builder.Reset nils the buffer rather than truncating it, so the array is
// dropped on the next get either way and resetting here costs no extra
// allocation. What it changes is who holds the array in between: this pool is
// deliberately not GC-evictable, so a builder parked with its contents keeps a
// whole render's output reachable for the process lifetime. String() aliases
// the array, so any result already taken from the builder stays valid.
func putStringBuilder(b *strings.Builder) {
	b.Reset()
	stringBuilderPool.put(b)
}

// SourceFrame is one level of the template include stack: the template path and
// the source line active in it.
type SourceFrame struct {
	Path string
	Line int
}

// SourceSpan attributes a contiguous run of rendered output to the template
// include stack that produced it (see renderer.collectSrc). Frames is innermost
// first: Frames[0] is where the text literally is, and the rest are the render /
// include callers up to the entry template. Length is the number of output
// bytes; IsText distinguishes literal template text (Frames[0].Line advances per
// '\n') from a show-expression value (all its lines map to the same frames).
type SourceSpan struct {
	Frames []SourceFrame
	Length int
	IsText bool
}

// valueSource is the per-output-line include stack of a rendered-to-string value
// ("{{ render \"x\" | filter }}"), captured while the sub-render ran and consumed
// by the show that outputs the (whitespace-filtered) value. Each entry keeps the
// buffer line's text so the show can align by trimmed content even when the
// filter drops or indents lines (strip / indent), and so an unrelated value
// won't match.
type valueLine struct {
	frames []SourceFrame
	text   string
}

type valueSource []valueLine

type outputSegmentKind uint8

const (
	outputSegmentLiteral outputSegmentKind = iota
	outputSegmentFuture
	outputSegmentTextFragment
)

// outputSegment represents ordered output held while parallel renders finish.
type outputSegment struct {
	kind     outputSegmentKind
	literal  string
	future   *renderFuture
	fragment native.TextFragment
	// srcSpans holds the source-map spans for a literal segment. For a future,
	// goFrames attributes its output to the "{{ go … }}" call site.
	srcSpans []SourceSpan
	goFrames []SourceFrame
}

// defaultMaxParallelRenders returns the default maximum number of concurrent go render operations.
// Uses 2*GOMAXPROCS to adapt to the machine while limiting goroutine contention.
// Caps at a reasonable maximum to prevent excessive concurrency.
func defaultMaxParallelRenders() int {
	const maxConcurrency = 256 // reasonable upper bound
	n := 2 * goruntime.GOMAXPROCS(0)
	if n > maxConcurrency {
		return maxConcurrency
	}
	if n < 1 {
		return 1
	}
	return n
}

// renderer is used by the Show and Text instructions to render template files.
type renderer struct {

	// out is the io.Writer to write to.
	out io.Writer

	// inURL reports whether it is in a URL.
	inURL bool

	// query reports whether it is in the query string of an URL.
	query bool

	// addAmpersand reports whether an ampersand must be added before the next
	// written text. It can be true only if it is in a URL.
	addAmpersand bool

	// removeQuestionMark reports whether a question mark must be removed
	// before the next written text. It can be true only if it is in a URL.
	removeQuestionMark bool

	// segmentsMu protects segments and pendingBuf from concurrent access.
	// This is necessary because nested go render operations may add segments
	// concurrently with the main VM thread.
	segmentsMu sync.Mutex

	// segments holds ordered output segments for parallel rendering.
	// When non-nil, parallel rendering is active.
	// Protected by segmentsMu.
	segments []outputSegment

	// pendingBuf collects output between future slots when parallel rendering is active.
	// Protected by segmentsMu.
	pendingBuf *strings.Builder

	// eg is the errgroup used to limit concurrent go render operations.
	// Lazily initialized on first go render.
	eg *errgroup.Group

	// wg tracks all spawned parallel renders (including those waiting for slots).
	wg sync.WaitGroup

	// parallelDepth tracks the current nesting depth of parallel renders.
	// Used to prevent unbounded recursion (DoS protection).
	parallelDepth int32

	// maxParallelRenders is the maximum number of concurrent go render operations.
	// If zero, uses defaultMaxParallelRenders().
	maxParallelRenders int

	// maxParallelDepth is the maximum nesting depth for parallel renders.
	// If zero, defaults to 100.
	maxParallelDepth int32

	retainTextFragments bool

	// --- source-map collection (opt-in via CollectSourceMap) ---
	// collectSrc enables recording of SourceSpans. When false (production) the
	// added code is a single branch per write and carries no overhead.
	collectSrc bool
	// curFrames is the include stack (innermost first) of the output currently
	// being written; curIsText marks literal template text. Set by the VM.
	curFrames []SourceFrame
	curIsText bool
	// pendingSpans mirror writes into the current out/pendingBuf region;
	// finalSpans is the reassembled result (prefix + segments) after futures.
	pendingSpans []SourceSpan
	finalSpans   []SourceSpan
	// recW records each write's span, then forwards to out/pendingBuf.
	recW recordingWriter
	// pendingValueSources is a LIFO stack of per-line include stacks for
	// rendered-to-string values ("{{ render \"x\" | filter }}") awaiting their
	// show. consumeNext is set by a show opcode so the next write attributes the
	// value's output per line instead of to the show's single frame.
	pendingValueSources []valueSource
	consumeNext         bool
}

// newRenderer returns a new renderer.
func newRenderer(out io.Writer) *renderer {
	r := &renderer{out: out}
	r.recW = recordingWriter{r}
	return r
}

func (r *renderer) reset(out io.Writer) {
	r.out = out
	r.inURL = false
	r.query = false
	r.addAmpersand = false
	r.removeQuestionMark = false
	r.segments = nil
	r.pendingBuf = nil
	r.eg = nil
	atomic.StoreInt32(&r.parallelDepth, 0)
	r.maxParallelRenders = 0
	r.maxParallelDepth = 0
	r.retainTextFragments = false
	r.collectSrc = false
	r.curFrames = nil
	r.curIsText = false
	r.pendingSpans = nil
	r.finalSpans = nil
	r.recW = recordingWriter{r}
	r.pendingValueSources = nil
	r.consumeNext = false
}

// recordingWriter records a SourceSpan for each write (attributed to the
// renderer's current curPath/curLine/curIsText) and forwards to the underlying
// out/pendingBuf. Used only while collectSrc is true.
type recordingWriter struct{ r *renderer }

func (w recordingWriter) Write(p []byte) (int, error) {
	n, err := w.r.rawWrite(p)
	if n > 0 {
		w.r.record(string(p[:n]))
	}
	return n, err
}

func (w recordingWriter) WriteString(s string) (int, error) {
	n, err := w.r.rawWriteString(s)
	if n > 0 {
		w.r.record(s[:n])
	}
	return n, err
}

// record appends span(s) for one write. Normally one span attributed to the
// current include stack; but when a show has flagged consumeNext and a
// rendered-to-string value is pending that this write derives from, the value's
// per-line include stacks are used instead (so a "{{ render \"x\" | indent }}"
// line resolves to x's own source, not the show line).
func (r *renderer) record(content string) {
	n := len(content)
	if n == 0 {
		return
	}
	if r.consumeNext {
		r.consumeNext = false // one-shot: only the first write of the show consumes
		if r.consumeValueSources(content) {
			return
		}
	}
	r.pendingSpans = append(r.pendingSpans, SourceSpan{Frames: r.curFrames, Length: n, IsText: r.curIsText})
}

// consumeValueSources attributes content per output line from the pending value
// sources. It first tries the whole pending group concatenated in render order (a
// "render_glob \"x-*\" | filter" that matched several snippets, whose outputs are
// concatenated then filtered), then just the most recent (a single
// "render \"x\" | filter"). On success it records per-line spans and drops the
// used sources; returns false if content doesn't derive from them (then the
// caller records a single fallback span). Each source line carries its own text +
// frames, and recordValue requires an exact trimmed-content match per non-blank
// line, so a wrong concatenation simply fails to match and falls back.
func (r *renderer) consumeValueSources(content string) bool {
	k := len(r.pendingValueSources)
	if k == 0 {
		return false
	}
	// Grow a top-suffix of the source stack until its concatenation matches the
	// content, then consume exactly that suffix. A single "render x | filter"
	// matches the top source (n=1); a "render_glob | filter" matching several
	// snippets matches the top n sources concatenated. Earlier orphaned sources —
	// from value-renders in the same renderer whose result was never filtered —
	// stay below the matched suffix and don't interfere, and empty sources between
	// real ones contribute no lines so a suffix spanning them still matches. This
	// is why we grow from the top rather than trying the whole stack at once.
	combined := valueSource{}
	for n := 1; n <= k; n++ {
		src := r.pendingValueSources[k-n]
		grown := make(valueSource, 0, len(src)+len(combined))
		grown = append(grown, src...)
		grown = append(grown, combined...)
		combined = grown
		if r.recordValue(content, combined) {
			r.pendingValueSources = r.pendingValueSources[:k-n]
			return true
		}
	}
	return false
}

// recordValue attributes a filtered rendered-to-string value per output line,
// aligning each non-blank output line to the next non-blank source line by
// trimmed content (blank lines skipped on both sides). This survives
// whitespace-only filters that drop or indent lines (strip, indent), and rejects
// unrelated values: if any non-blank output line doesn't match the next source
// line, it records nothing and returns false so the caller falls back.
func (r *renderer) recordValue(content string, src valueSource) bool {
	segs := splitKeepNL(content)
	assigned := make([][]SourceFrame, len(segs))
	si := 0
	for ci, seg := range segs {
		if strings.TrimSpace(seg) == "" {
			continue
		}
		for si < len(src) && strings.TrimSpace(src[si].text) == "" {
			si++
		}
		if si >= len(src) || strings.TrimSpace(src[si].text) != strings.TrimSpace(seg) {
			return false // not a whitespace-only transform of this value
		}
		assigned[ci] = src[si].frames
		si++
	}
	for i, seg := range segs {
		r.pendingSpans = append(r.pendingSpans, SourceSpan{Frames: assigned[i], Length: len(seg), IsText: true})
	}
	return true
}

// splitKeepNL splits s into lines, each retaining its trailing '\n' (the last
// has none if s doesn't end in '\n').
func splitKeepNL(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			out = append(out, s[start:i+1])
			start = i + 1
		}
	}
	if start < len(s) {
		out = append(out, s[start:])
	}
	return out
}

// perLineFrames converts a rendered string and its spans into one include stack
// per output line (innermost line advanced within multi-line text spans), keeping
// each line's text for later trimmed-content alignment.
func perLineFrames(raw string, spans []SourceSpan) valueSource {
	segs := splitKeepNL(raw)
	out := make(valueSource, len(segs))
	for i, seg := range segs {
		out[i].text = seg
	}
	line, pos := 0, 0
	for _, sp := range spans {
		end := pos + sp.Length
		if end > len(raw) {
			end = len(raw)
		}
		sub := 0
		for i := pos; i < end; i++ {
			if line < len(out) && out[line].frames == nil {
				out[line].frames = adjustFrames(sp.Frames, sp.IsText, sub)
			}
			if raw[i] == '\n' {
				line++
				if sp.IsText {
					sub++
				}
			}
		}
		pos = end
	}
	return out
}

// adjustFrames copies frames, advancing the innermost line by sub for text spans.
func adjustFrames(frames []SourceFrame, isText bool, sub int) []SourceFrame {
	if len(frames) == 0 {
		return nil
	}
	cp := make([]SourceFrame, len(frames))
	copy(cp, frames)
	if isText && sub > 0 {
		cp[0].Line += sub
	}
	return cp
}

// rawWrite / rawWriteString write to the pending buffer (parallel active) or
// directly to out, without recording.
func (r *renderer) rawWrite(p []byte) (int, error) {
	if r.pendingBuf != nil {
		return r.pendingBuf.Write(p)
	}
	return r.out.Write(p)
}

func (r *renderer) rawWriteString(s string) (int, error) {
	if r.pendingBuf != nil {
		return r.pendingBuf.WriteString(s)
	}
	return io.WriteString(r.out, s)
}

// takePendingSpans returns and clears the accumulated pending spans.
func (r *renderer) takePendingSpans() []SourceSpan {
	if !r.collectSrc {
		return nil
	}
	s := r.pendingSpans
	r.pendingSpans = nil
	return s
}

// collectSpans returns the final output-ordered source map after a run.
func (r *renderer) collectSpans() []SourceSpan {
	if len(r.finalSpans) == 0 {
		return r.pendingSpans // no parallel render: everything is sequential
	}
	if len(r.pendingSpans) == 0 {
		return r.finalSpans
	}
	return append(r.finalSpans, r.pendingSpans...)
}

// outputWriter returns the writer to use for output - either the pending
// buffer for parallel rendering or the main output writer.
func (r *renderer) outputWriter() io.Writer {
	if r.collectSrc {
		return r.recW
	}
	if r.pendingBuf != nil {
		return r.pendingBuf
	}
	return r.out
}

// Show shows v in the given context.
func (r *renderer) Show(env *nativeEnv, v any, context Context) error {

	ctx, inURL, _ := decodeRenderContext(context)
	if ctx != ast.ContextText {
		if _, ok := v.(native.TextFragment); ok {
			return fmt.Errorf("cannot show type %T as %s", v, ctx)
		}
	}

	// Check and eventually change the URL state.
	if r.inURL != inURL {
		if !inURL {
			r.endURL()
		}
		r.inURL = inURL
	}

	if inURL {
		return r.showInURL(env, v, ctx)
	}

	var err error
	out := r.outputWriter()

	switch ctx {
	case ast.ContextText:
		if fragment, ok := v.(native.TextFragment); ok {
			err = r.showTextFragment(fragment)
		} else {
			err = showInText(env, out, v)
		}
	case ast.ContextHTML:
		err = showInHTML(env, out, v)
	case ast.ContextTag:
		err = showInTag(env, out, v)
	case ast.ContextQuotedAttr:
		err = showInAttribute(env, out, v, true)
	case ast.ContextUnquotedAttr:
		err = showInAttribute(env, out, v, false)
	case ast.ContextCSS:
		err = showInCSS(env, out, v)
	case ast.ContextCSSString:
		err = showInCSSString(env, out, v)
	case ast.ContextJS:
		err = showInJS(env, out, v)
	case ast.ContextJSString:
		err = showInJSString(env, out, v)
	case ast.ContextJSON:
		err = showInJSON(env, out, v)
	case ast.ContextJSONString:
		err = showInJSONString(env, out, v)
	case ast.ContextMarkdown:
		err = showInMarkdown(env, out, v)
	case ast.ContextTabCodeBlock:
		err = showInMarkdownCodeBlock(env, out, v, false)
	case ast.ContextSpacesCodeBlock:
		err = showInMarkdownCodeBlock(env, out, v, true)
	default:
		panic("scriggo: unknown context")
	}

	return err
}

// Out returns the out writer.
func (r *renderer) Out() io.Writer {
	return r.out
}

// Text shows txt in the given context.
func (r *renderer) Text(txt []byte, inURL, isSet bool) error {

	// Check and eventually change the URL state.
	if r.inURL != inURL {
		if !inURL {
			r.endURL()
		}
		r.inURL = inURL
	}

	if inURL {
		if isSet && bytes.ContainsRune(txt, ',') {
			r.query = false
		} else if r.query {
			if r.removeQuestionMark && txt[0] == '?' {
				txt = txt[1:]
			}
			if r.addAmpersand && len(txt) > 0 && txt[0] != '&' {
				_, err := r.writeStringForParallel("&amp;")
				if err != nil {
					return err
				}
			}
			r.removeQuestionMark = false
			r.addAmpersand = false
		} else {
			r.query = bytes.ContainsAny(txt, "?#")
		}
		_, err := r.writeForParallel(txt)
		return err
	}

	_, err := r.writeForParallel(txt)
	return err
}

// showInURL shows v in a URL in the given context.
func (r *renderer) showInURL(env *nativeEnv, v any, ctx ast.Context) error {

	b := stringBuilderPool.get()
	b.Reset()
	err := showInHTML(env, b, v)
	if err != nil {
		putStringBuilder(b)
		return err
	}

	s := html.UnescapeString(b.String())
	putStringBuilder(b)
	out := newStringWriter(r.outputWriter())

	if r.query {
		if r.removeQuestionMark {
			c := s[len(s)-1]
			r.addAmpersand = c != '&'
			_, err := pathEscape(out, s, ctx == ast.ContextQuotedAttr)
			return err
		}
		_, err := queryEscape(out, s)
		return err
	}

	if strings.Contains(s, "?") {
		r.query = true
		r.removeQuestionMark = true
		if c := s[len(s)-1]; c != '&' && c != '?' {
			r.addAmpersand = true
		}
	}
	_, err = pathEscape(out, s, ctx == ast.ContextQuotedAttr)

	return err
}

// endURL is called when an URL ends.
func (r *renderer) endURL() {
	r.inURL = false
	r.query = false
	r.addAmpersand = false
	r.removeQuestionMark = false
}

type strWriterWrapper struct {
	w io.Writer
}

func (wr strWriterWrapper) Write(b []byte) (int, error) {
	return wr.w.Write(b)
}

func (wr strWriterWrapper) WriteString(s string) (int, error) {
	return wr.w.Write([]byte(s))
}

func newStringWriter(wr io.Writer) strWriter {
	if sw, ok := wr.(strWriter); ok {
		return sw
	}
	return strWriterWrapper{wr}
}

func toString(env *nativeEnv, i any) (string, error) {
	v := valueOf(env, i)
	switch v.Kind() {
	case reflect.Invalid:
		return "", nil
	case reflect.Bool:
		if v.Bool() {
			return "true", nil
		}
		return "false", nil
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return strconv.FormatInt(v.Int(), 10), nil
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return strconv.FormatUint(v.Uint(), 10), nil
	case reflect.Float32:
		return strconv.FormatFloat(v.Float(), 'f', -1, 32), nil
	case reflect.Float64:
		return strconv.FormatFloat(v.Float(), 'f', -1, 64), nil
	case reflect.String:
		return v.String(), nil
	case reflect.Complex64:
		c := v.Complex()
		if c == 0 {
			return "0", nil
		}
		var s string
		if r := real(c); r != 0 {
			s = strconv.FormatFloat(r, 'f', -1, 32)
		}
		if i := imag(c); i != 0 {
			if s != "" && i > 0 {
				s += " "
			}
			s = strconv.FormatFloat(i, 'f', -1, 32) + "i"
		}
		return s, nil
	case reflect.Complex128:
		c := v.Complex()
		if c == 0 {
			return "0", nil
		}
		var s string
		if r := real(c); r != 0 {
			s = strconv.FormatFloat(r, 'f', -1, 64)
		}
		if i := imag(c); i != 0 {
			if s != "" && i > 0 {
				s += "+"
			}
			s = strconv.FormatFloat(i, 'f', -1, 64) + "i"
		}
		return s, nil
	case reflect.Ptr:
		// Auto-deref pointers (feat F). The checker accepts `*T` if
		// either the pointer type itself satisfies an existing
		// showable rule (preserves Stringer-on-pointer behavior,
		// handled by the per-context showers' interface fast-paths
		// before they reach toString) or the element type does. By
		// the time toString sees a pointer, the pointer-as-interface
		// fast-paths have all missed, so dereference and recurse.
		// A nil pointer renders as empty — matches the chart's
		// `dig() | fallback("")` semantic for absent fields.
		if v.IsNil() {
			return "", nil
		}
		return toString(env, v.Elem().Interface())
	default:
		return "", fmt.Errorf("cannot show value of type %s", env.TypeOf(reflect.ValueOf(i)))
	}
}

// showInText shows value in the Text context.
func showInText(env *nativeEnv, out io.Writer, value any) error {
	var s string
	switch v := value.(type) {
	case fmt.Stringer:
		s = v.String()
	case native.EnvStringer:
		s = v.String(env)
	case error:
		s = v.Error()
	default:
		var err error
		s, err = toString(env, value)
		if err != nil {
			return err
		}
	}
	w := newStringWriter(out)
	_, err := w.WriteString(s)
	return err
}

type textFragmentCountingWriter struct {
	out     io.Writer
	written int64
	err     error
}

func (w *textFragmentCountingWriter) Write(p []byte) (int, error) {
	if w.err != nil {
		return 0, w.err
	}
	n, err := w.out.Write(p)
	return w.record(len(p), n, err)
}

func (w *textFragmentCountingWriter) WriteString(s string) (int, error) {
	if w.err != nil {
		return 0, w.err
	}
	var n int
	var err error
	if out, ok := w.out.(io.StringWriter); ok {
		n, err = out.WriteString(s)
	} else {
		n, err = w.out.Write([]byte(s))
	}
	return w.record(len(s), n, err)
}

func (w *textFragmentCountingWriter) record(length, n int, err error) (int, error) {
	if n < 0 || n > length {
		w.err = fmt.Errorf("%w: writer returned %d for %d bytes", errInvalidTextFragmentWriteCount, n, length)
		return 0, w.err
	}
	const maxInt64 = int64(^uint64(0) >> 1)
	if w.written > maxInt64-int64(n) {
		w.err = fmt.Errorf("%w: byte count overflow", errInvalidTextFragmentWriteCount)
		return 0, w.err
	}
	w.written += int64(n)
	if err == nil && n != length {
		err = io.ErrShortWrite
	}
	if err != nil {
		w.err = err
	}
	return n, err
}

func writeTextFragment(out io.Writer, fragment native.TextFragment) error {
	counter := &textFragmentCountingWriter{out: out}
	reported, err := fragment.WriteTo(counter)
	if reported < 0 || reported != counter.written {
		return fmt.Errorf(
			"%w: fragment reported %d for %d bytes",
			errInvalidTextFragmentWriteCount,
			reported,
			counter.written,
		)
	}
	if counter.err != nil {
		return counter.err
	}
	return err
}

func emitTextFragment(out io.Writer, fragment native.TextFragment) error {
	if writer, ok := out.(native.TextFragmentWriter); ok {
		return writer.WriteTextFragment(fragment)
	}
	return writeTextFragment(out, fragment)
}

func (r *renderer) showTextFragment(fragment native.TextFragment) error {
	if r.collectSrc || !r.retainTextFragments {
		return writeTextFragment(r.outputWriter(), fragment)
	}

	r.segmentsMu.Lock()
	if r.pendingBuf != nil {
		r.flushPendingLiteralLocked()
		r.segments = append(r.segments, outputSegment{
			kind:     outputSegmentTextFragment,
			fragment: fragment,
		})
		r.segmentsMu.Unlock()
		return nil
	}
	out := r.out
	r.segmentsMu.Unlock()
	return emitTextFragment(out, fragment)
}

// showInHTML shows value in HTML context.
func showInHTML(env *nativeEnv, out io.Writer, value any) error {
	w := newStringWriter(out)
	switch v := value.(type) {
	// Fast paths for basic types - avoid reflection in toString()
	case int:
		_, err := w.WriteString(strconv.Itoa(v))
		return err
	case int64:
		_, err := w.WriteString(strconv.FormatInt(v, 10))
		return err
	case int32:
		_, err := w.WriteString(strconv.FormatInt(int64(v), 10))
		return err
	case string:
		return htmlEscape(w, v)
	case bool:
		if v {
			_, err := w.WriteString("true")
			return err
		}
		_, err := w.WriteString("false")
		return err
	case float64:
		_, err := w.WriteString(strconv.FormatFloat(v, 'f', -1, 64))
		return err
	case float32:
		_, err := w.WriteString(strconv.FormatFloat(float64(v), 'f', -1, 32))
		return err
	case native.HTML:
		_, err := w.WriteString(string(v))
		return err
	case native.HTMLStringer:
		_, err := w.WriteString(string(v.HTML()))
		return err
	case native.HTMLEnvStringer:
		_, err := w.WriteString(string(v.HTML(env)))
		return err
	case fmt.Stringer:
		return htmlEscape(w, v.String())
	case native.EnvStringer:
		return htmlEscape(w, v.String(env))
	case []byte:
		_, err := out.Write(v)
		return err
	case error:
		return htmlEscape(w, v.Error())
	case native.Markdown:
		if env.env.conv != nil {
			return env.env.conv([]byte(v), out)
		}
	}
	s, err := toString(env, value)
	if err != nil {
		return err
	}
	return htmlEscape(w, s)
}

// showInTag show value in Tag context.
func showInTag(env *nativeEnv, out io.Writer, value any) error {
	var s string
	switch v := value.(type) {
	case fmt.Stringer:
		s = v.String()
	case native.EnvStringer:
		s = v.String(env)
	case error:
		s = v.Error()
	default:
		var err error
		s, err = toString(env, value)
		if err != nil {
			return err
		}
	}
	i := 0
	var s2 *strings.Builder
	for j, c := range s {
		if (c == utf8.RuneError && j == i+1) ||
			c <= 0x1F || c == '"' || c == '\'' || c == '>' || c == '/' || c == '=' ||
			0x7F <= c && c <= 0x9F || unicode.Is(unicode.Noncharacter_Code_Point, c) {
			if s2 == nil {
				s2 = &strings.Builder{}
				s2.WriteString(s[0:j])
			}
			c = unicode.ReplacementChar
		}
		if s2 != nil {
			s2.WriteRune(c)
		}
	}
	if s2 != nil {
		s = s2.String()
	}
	w := newStringWriter(out)
	_, err := w.WriteString(s)
	return err
}

// showInAttribute shows value in the attribute context quoted or unquoted
// depending on quoted value.
func showInAttribute(env *nativeEnv, out io.Writer, value any, quoted bool) error {
	var s string
	var escapeEntities bool
	switch v := value.(type) {
	case fmt.Stringer:
		s = v.String()
		escapeEntities = true
	case native.EnvStringer:
		s = v.String(env)
		escapeEntities = true
	case native.HTML:
		s = string(v)
	case native.HTMLStringer:
		s = string(v.HTML())
	case native.HTMLEnvStringer:
		s = string(v.HTML(env))
	case error:
		s = v.Error()
		escapeEntities = true
	default:
		var err error
		s, err = toString(env, value)
		if err != nil {
			return err
		}
		escapeEntities = true
	}
	return attributeEscape(newStringWriter(out), s, escapeEntities, quoted)
}

// showInCSS shows value in CSS context.
func showInCSS(env *nativeEnv, out io.Writer, value any) error {
	w := newStringWriter(out)
	switch v := value.(type) {
	case native.CSS:
		_, err := w.WriteString(string(v))
		return err
	case native.CSSStringer:
		_, err := w.WriteString(string(v.CSS()))
		return err
	case native.CSSEnvStringer:
		_, err := w.WriteString(string(v.CSS(env)))
		return err
	case fmt.Stringer:
		value = v.String()
	case native.EnvStringer:
		value = v.String(env)
	case []byte:
		return escapeBytes(w, v, false)
	case error:
		value = v.Error()
	}
	v := valueOf(env, value)
	switch v.Kind() {
	case reflect.String:
		_, err := w.WriteString(`"`)
		if err == nil {
			err = cssStringEscape(w, v.String())
		}
		if err == nil {
			_, err = w.WriteString(`"`)
		}
		return err
	default:
		s, err := toString(env, value)
		if err != nil {
			return err
		}
		_, err = w.WriteString(s)
		return err
	}
}

// showInCSSString shows value in CSSString context.
func showInCSSString(env *nativeEnv, out io.Writer, value any) error {
	var s string
	switch value := value.(type) {
	case fmt.Stringer:
		s = value.String()
	case native.EnvStringer:
		s = value.String(env)
	case error:
		s = value.Error()
	default:
		v := reflect.ValueOf(value)
		if v.Type() == byteSliceType {
			w := newStringWriter(out)
			return escapeBytes(w, v.Interface().([]byte), false)
		}
		var err error
		s, err = toString(env, value)
		if err != nil {
			return err
		}
	}
	return cssStringEscape(newStringWriter(out), s)
}

// showInJS shows value in JavaScript context.
func showInJS(env *nativeEnv, out io.Writer, value any) error {

	w := newStringWriter(out)

	switch v := value.(type) {
	case nil:
		_, err := w.WriteString("null")
		return err
	case native.JS:
		_, err := w.WriteString(string(v))
		return err
	case native.JSStringer:
		_, err := w.WriteString(string(v.JS()))
		return err
	case native.JSEnvStringer:
		_, err := w.WriteString(string(v.JS(env)))
		return err
	case time.Time:
		_, err := w.WriteString(showTimeInJS(v))
		return err
	case error:
		value = v.Error()
	}

	v := valueOf(env, value)

	var s string

	switch v.Kind() {
	case reflect.Bool:
		s = "false"
		if v.Bool() {
			s = "true"
		}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		s = strconv.FormatInt(v.Int(), 10)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		s = strconv.FormatUint(v.Uint(), 10)
	case reflect.Float32:
		s = strconv.FormatFloat(v.Float(), 'f', -1, 32)
	case reflect.Float64:
		s = strconv.FormatFloat(v.Float(), 'f', -1, 64)
	case reflect.String:
		_, err := w.WriteString("\"")
		if err == nil {
			err = jsStringEscape(w, v.String())
		}
		if err == nil {
			_, err = w.WriteString("\"")
		}
		return err
	case reflect.Slice:
		if b, ok := value.([]byte); ok {
			w := newStringWriter(out)
			return escapeBytes(w, b, true)
		}
		if v.IsNil() {
			s = "null"
			break
		}
		fallthrough
	case reflect.Array:
		if v.Len() == 0 {
			s = "[]"
			break
		}
		_, err := w.WriteString("[")
		for i := 0; i < v.Len(); i++ {
			if err != nil {
				return err
			}
			if i > 0 {
				_, err = w.WriteString(",")
			}
			if err == nil {
				err = showInJS(env, out, v.Index(i).Interface())
			}
		}
		if err == nil {
			_, err = w.WriteString("]")
		}
		return err
	case reflect.Pointer, reflect.UnsafePointer:
		if v.IsNil() {
			s = "null"
			break
		}
		return showInJS(env, out, v.Elem().Interface())
	case reflect.Struct:
		t := v.Type()
		n := t.NumField()
		first := true
		_, err := w.WriteString(`{`)
		for i := range n {
			if err != nil {
				return err
			}
			if field := t.Field(i); field.PkgPath == "" {
				name := field.Name
				value := v.Field(i)
				if tag := field.Tag.Get("json"); tag != "" {
					if tag == "-" {
						continue
					}
					tagName, omitempty := parseTagValue(tag)
					if omitempty && isEmptyValue(value) {
						continue
					}
					if tagName != "" {
						name = tagName
					}
				}
				if first {
					_, err = w.WriteString(`"`)
				} else {
					_, err = w.WriteString(`,"`)
				}
				if err == nil {
					err = jsStringEscape(w, name)
				}
				if err == nil {
					_, err = w.WriteString(`":`)
				}
				if err == nil {
					err = showInJS(env, w, value.Interface())
				}
				first = false
			}
		}
		if err == nil {
			_, err = w.WriteString(`}`)
		}
		return err
	case reflect.Map:
		if v.IsNil() {
			s = "null"
			break
		}
		type keyPair struct {
			key string
			val any
		}
		keyPairs := make([]keyPair, v.Len())
		iter := v.MapRange()
		for i := 0; iter.Next(); i++ {
			key := iter.Key()
			switch k := key.Interface().(type) {
			case fmt.Stringer:
				keyPairs[i].key = k.String()
			case native.EnvStringer:
				keyPairs[i].key = k.String(env)
			default:
				s, err := toString(env, k)
				if err != nil {
					return err
				}
				keyPairs[i].key = s
			}
			keyPairs[i].val = iter.Value().Interface()
		}
		sort.Slice(keyPairs, func(i, j int) bool {
			return keyPairs[i].key < keyPairs[j].key
		})
		_, err := w.WriteString("{")
		for i, keyPair := range keyPairs {
			if err != nil {
				return err
			}
			if i == 0 {
				_, err = w.WriteString(`"`)
			} else {
				_, err = w.WriteString(`,"`)
			}
			if err == nil {
				err = jsStringEscape(w, keyPair.key)
			}
			if err == nil {
				_, err = w.WriteString(`":`)
			}
			if err == nil {
				err = showInJS(env, out, keyPair.val)
			}
		}
		if err == nil {
			_, err = w.WriteString("}")
		}
		return err
	default:
		t := env.TypeOf(reflect.ValueOf(value))
		s = fmt.Sprintf("undefined/* scriggo: cannot represent a %s value */", t)
	}

	_, err := w.WriteString(s)
	return err
}

// showInJSON shows value in JSON context.
func showInJSON(env *nativeEnv, out io.Writer, value any) error {

	w := newStringWriter(out)

	switch v := value.(type) {
	case nil:
		_, err := w.WriteString("null")
		return err
	case native.JSON:
		_, err := w.WriteString(string(v))
		return err
	case native.JSONStringer:
		_, err := w.WriteString(string(v.JSON()))
		return err
	case native.JSONEnvStringer:
		_, err := w.WriteString(string(v.JSON(env)))
		return err
	case time.Time:
		_, err := w.WriteString("\"")
		if err == nil {
			_, err = w.WriteString(v.Format(time.RFC3339))
		}
		if err == nil {
			_, err = w.WriteString("\"")
		}
		return err
	case error:
		value = v.Error()
	}

	v := reflect.ValueOf(value)

	var s string

	switch v.Kind() {
	case reflect.Bool:
		s = "false"
		if v.Bool() {
			s = "true"
		}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		s = strconv.FormatInt(v.Int(), 10)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		s = strconv.FormatUint(v.Uint(), 10)
	case reflect.Float32:
		s = strconv.FormatFloat(v.Float(), 'f', -1, 32)
	case reflect.Float64:
		s = strconv.FormatFloat(v.Float(), 'f', -1, 64)
	case reflect.String:
		_, err := w.WriteString("\"")
		if err == nil {
			err = jsonStringEscape(w, v.String())
		}
		if err == nil {
			_, err = w.WriteString("\"")
		}
		return err
	case reflect.Slice:
		if b, ok := value.([]byte); ok {
			w := newStringWriter(out)
			return escapeBytes(w, b, true)
		}
		if v.IsNil() {
			s = "null"
			break
		}
		fallthrough
	case reflect.Array:
		if v.Len() == 0 {
			s = "[]"
			break
		}
		_, err := w.WriteString("[")
		for i := 0; i < v.Len(); i++ {
			if err != nil {
				return err
			}
			if i > 0 {
				_, err = w.WriteString(",")
			}
			if err == nil {
				err = showInJSON(env, out, v.Index(i).Interface())
			}
		}
		if err == nil {
			_, err = w.WriteString("]")
		}
		return err
	case reflect.Pointer, reflect.UnsafePointer:
		if v.IsNil() {
			s = "null"
			break
		}
		return showInJSON(env, out, v.Elem().Interface())
	case reflect.Struct:
		t := v.Type()
		n := t.NumField()
		first := true
		_, err := w.WriteString(`{`)
		for i := range n {
			if err != nil {
				return err
			}
			if field := t.Field(i); field.PkgPath == "" {
				name := field.Name
				value := v.Field(i)
				if tag := field.Tag.Get("json"); tag != "" {
					if tag == "-" {
						continue
					}
					tagName, omitempty := parseTagValue(tag)
					if omitempty && isEmptyValue(value) {
						continue
					}
					if tagName != "" {
						name = tagName
					}
				}
				if first {
					_, err = w.WriteString(`"`)
				} else {
					_, err = w.WriteString(`,"`)
				}
				if err == nil {
					err = jsonStringEscape(w, name)
				}
				if err == nil {
					_, err = w.WriteString(`":`)
				}
				if err == nil {
					err = showInJSON(env, w, value.Interface())
				}
				first = false
			}
		}
		if err == nil {
			_, err = w.WriteString(`}`)
		}
		return err
	case reflect.Map:
		if v.IsNil() {
			s = "null"
			break
		}
		type keyPair struct {
			key string
			val any
		}
		keyPairs := make([]keyPair, v.Len())
		iter := v.MapRange()
		for i := 0; iter.Next(); i++ {
			key := iter.Key()
			switch k := key.Interface().(type) {
			case fmt.Stringer:
				keyPairs[i].key = k.String()
			case native.EnvStringer:
				keyPairs[i].key = k.String(env)
			default:
				s, err := toString(env, k)
				if err != nil {
					return err
				}
				keyPairs[i].key = s
			}
			keyPairs[i].val = iter.Value().Interface()
		}
		sort.Slice(keyPairs, func(i, j int) bool {
			return keyPairs[i].key < keyPairs[j].key
		})
		_, err := w.WriteString("{")
		for i, keyPair := range keyPairs {
			if err != nil {
				return err
			}
			if i == 0 {
				_, err = w.WriteString(`"`)
			} else {
				_, err = w.WriteString(`,"`)
			}
			if err == nil {
				err = jsonStringEscape(w, keyPair.key)
			}
			if err == nil {
				_, err = w.WriteString(`":`)
			}
			if err == nil {
				err = showInJSON(env, out, keyPair.val)
			}
		}
		if err == nil {
			_, err = w.WriteString("}")
		}
		return err
	default:
		s = "null"
	}

	_, err := w.WriteString(s)
	return err
}

// showInJSString shows value in JSString context.
func showInJSString(env *nativeEnv, out io.Writer, value any) error {
	var s string
	switch v := value.(type) {
	case fmt.Stringer:
		s = v.String()
	case native.EnvStringer:
		s = v.String(env)
	case error:
		s = v.Error()
	default:
		var err error
		s, err = toString(env, value)
		if err != nil {
			return err
		}
	}
	return jsStringEscape(newStringWriter(out), s)
}

// showInJSONString shows value in JSONString context.
func showInJSONString(env *nativeEnv, out io.Writer, value any) error {
	return showInJSString(env, out, value)
}

// showInMarkdown shows value in the Markdown context.
func showInMarkdown(env *nativeEnv, out io.Writer, value any) error {
	w := newStringWriter(out)
	switch v := value.(type) {
	case native.Markdown:
		_, err := w.WriteString(string(v))
		return err
	case native.MarkdownStringer:
		_, err := w.WriteString(string(v.Markdown()))
		return err
	case native.MarkdownEnvStringer:
		_, err := w.WriteString(string(v.Markdown(env)))
		return err
	case native.HTML:
		return markdownEscape(w, string(v), true)
	case native.HTMLStringer:
		return markdownEscape(w, string(v.HTML()), true)
	case native.HTMLEnvStringer:
		return markdownEscape(w, string(v.HTML(env)), true)
	case fmt.Stringer:
		return markdownEscape(w, v.String(), false)
	case native.EnvStringer:
		return markdownEscape(w, v.String(env), false)
	case error:
		return markdownEscape(w, v.Error(), false)
	default:
		s, err := toString(env, value)
		if err != nil {
			return err
		}
		return markdownEscape(w, s, false)
	}
}

// showInMarkdownCodeBlock shows value in the Markdown code block context.
func showInMarkdownCodeBlock(env *nativeEnv, out io.Writer, value any, spaces bool) error {
	var s string
	switch v := value.(type) {
	case fmt.Stringer:
		s = v.String()
	case native.EnvStringer:
		s = v.String(env)
	case error:
		s = v.Error()
	default:
		var err error
		s, err = toString(env, value)
		if err != nil {
			return err
		}
	}
	w := newStringWriter(out)
	return markdownCodeBlockEscape(w, s, spaces)
}

// showTimeInJS shows a value of type time.Time in a JavaScript context.
func showTimeInJS(tt time.Time) string {
	y := tt.Year()
	if y < -999999 || y > 999999 {
		panic("not representable year in JavaScript")
	}
	ms := int64(tt.Nanosecond()) / int64(time.Millisecond)
	name, offset := tt.Zone()
	if name == "UTC" {
		format := `new Date("%0.4d-%0.2d-%0.2dT%0.2d:%0.2d:%0.2d.%0.3dZ")`
		if y < 0 || y > 9999 {
			format = `new Date("%+0.6d-%0.2d-%0.2dT%0.2d:%0.2d:%0.2d.%0.3dZ")`
		}
		return fmt.Sprintf(format, y, tt.Month(), tt.Day(), tt.Hour(), tt.Minute(), tt.Second(), ms)
	}
	zone := offset / 60
	h, m := zone/60, zone%60
	if m < 0 {
		m = -m
	}
	format := `new Date("%0.4d-%0.2d-%0.2dT%0.2d:%0.2d:%0.2d.%0.3d%+0.2d:%0.2d")`
	if y < 0 || y > 9999 {
		format = `new Date("%+0.6d-%0.2d-%0.2dT%0.2d:%0.2d:%0.2d.%0.3d%+0.2d:%0.2d")`
	}
	return fmt.Sprintf(format, y, tt.Month(), tt.Day(), tt.Hour(), tt.Minute(), tt.Second(), ms, h, m)
}

// parseTagValue parses a 'json' tag value and returns its name and whether
// its options contain 'omitempty'.
func parseTagValue(tag string) (name string, omitempty bool) {
	i := strings.Index(tag, ",")
	if i == -1 {
		return tag, false
	}
	name, tag = tag[:i], tag[i+1:]
	for tag != "" {
		i = strings.Index(tag, ",")
		if i == -1 {
			return name, tag == "omitempty"
		}
		if tag[:i] == "omitempty" {
			return name, true
		}
		tag = tag[i+1:]
	}
	return name, false
}

// isEmptyValue reports whether v is an empty value for JS and JSON.
func isEmptyValue(v reflect.Value) (empty bool) {
	switch v.Kind() {
	case reflect.Bool:
		empty = !v.Bool()
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		empty = v.Int() == 0
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		empty = v.Uint() == 0
	case reflect.Float32, reflect.Float64:
		empty = v.Float() == 0
	case reflect.String, reflect.Map, reflect.Slice, reflect.Array:
		empty = v.Len() == 0
	case reflect.Interface, reflect.Pointer, reflect.UnsafePointer:
		empty = v.IsNil()
	}
	return
}

// valueOf returns the reflect value of i. If i is a proxy, it returns the
// underlying value.
func valueOf(env *nativeEnv, i any) reflect.Value {
	if i == nil {
		return reflect.Value{}
	}
	v := reflect.ValueOf(i)
	t := env.TypeOf(v)
	if w, ok := t.(ScriggoType); ok {
		v, _ = w.Unwrap(v)
	}
	return v
}

// decodeRenderContext decodes a runtime.Context.
// Keep in sync with the compiler.decodeRenderContext.
func decodeRenderContext(c Context) (ast.Context, bool, bool) {
	ctx := ast.Context(c & 0b00001111)
	inURL := c&0b10000000 != 0
	isURLSet := false
	if inURL {
		isURLSet = c&0b01000000 != 0
	}
	return ctx, inURL, isURLSet
}

func (r *renderer) flushPendingLiteralLocked() {
	if r.pendingBuf == nil || r.pendingBuf.Len() == 0 {
		return
	}
	r.segments = append(r.segments, outputSegment{
		kind:     outputSegmentLiteral,
		literal:  r.pendingBuf.String(),
		srcSpans: r.takePendingSpans(),
	})
	r.pendingBuf.Reset()
}

// addFutureSlot adds a placeholder for a parallel render's output.
// This method flushes any pending output to segments first.
// Returns the errgroup for spawning work, or an error if the parallel render depth limit is exceeded.
func (r *renderer) addFutureSlot(future *renderFuture) (*errgroup.Group, error) {
	// Check depth limit to prevent unbounded recursion (DoS protection)
	maxDepth := r.maxParallelDepth
	if maxDepth <= 0 {
		maxDepth = 100 // Default limit
	}
	depth := atomic.AddInt32(&r.parallelDepth, 1)
	if depth > maxDepth {
		atomic.AddInt32(&r.parallelDepth, -1)
		return nil, errors.New("parallel render depth limit exceeded")
	}

	r.segmentsMu.Lock()
	defer r.segmentsMu.Unlock()

	// Initialize errgroup on first parallel render
	if r.eg == nil {
		r.eg = new(errgroup.Group)
		limit := r.maxParallelRenders
		if limit <= 0 {
			limit = defaultMaxParallelRenders()
		}
		// Validate limit is within reasonable bounds
		if limit > 65536 {
			limit = 65536
		}
		r.eg.SetLimit(limit)
	}

	// If this is the first future, we need to start capturing output to pendingBuf
	if r.pendingBuf == nil {
		r.pendingBuf = stringBuilderPool.get()
		r.pendingBuf.Reset()
		// The prefix written before any future went straight to out, so its
		// spans open finalSpans; segments (below) are appended after it.
		r.finalSpans = append(r.finalSpans, r.takePendingSpans()...)
	} else {
		r.flushPendingLiteralLocked()
	}

	// Add the future slot, attributing its whole output to the go-render site.
	r.segments = append(r.segments, outputSegment{
		kind:     outputSegmentFuture,
		future:   future,
		goFrames: r.curFrames,
	})

	// Return the errgroup for the caller to spawn work on
	return r.eg, nil
}

// spawnParallelRender spawns a parallel render using the errgroup.
// The work function should send its result to the future's channels.
// This method returns immediately - a wrapper goroutine waits for the errgroup slot.
// The depth counter is decremented when the work completes.
// The errgroup must be passed from addFutureSlot to ensure proper synchronization.
func (r *renderer) spawnParallelRender(eg *errgroup.Group, work func()) {
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		defer atomic.AddInt32(&r.parallelDepth, -1)
		// This blocks until a slot is available, but doesn't block the main VM
		eg.Go(func() error {
			work()
			return nil // errors are communicated via future.err channel
		})
	}()
}

// flushWithFutures waits for all pending parallel renders and writes
// all output segments to the underlying writer in order.
// Returns the first error encountered from any parallel render.
func (r *renderer) flushWithFutures() error {
	r.segmentsMu.Lock()
	if r.segments == nil {
		r.segmentsMu.Unlock()
		return nil
	}

	r.flushPendingLiteralLocked()

	// Take ownership of segments and pendingBuf for cleanup
	segments := r.segments
	pendingBuf := r.pendingBuf
	eg := r.eg

	// Reset renderer state while holding the lock
	r.segments = nil
	r.pendingBuf = nil
	r.eg = nil

	r.segmentsMu.Unlock()

	// Wait for all spawned goroutines to finish submitting work to errgroup
	r.wg.Wait()

	// Wait for all errgroup work to complete
	// This ensures all work functions have finished before we read from channels
	if eg != nil {
		_ = eg.Wait() // errors are communicated via future.err channels
	}

	// Collect futures for cleanup - pre-allocate based on segment count
	futures := make([]*renderFuture, 0, len(segments))
	for _, seg := range segments {
		if seg.kind == outputSegmentFuture {
			futures = append(futures, seg.future)
		}
	}

	// Cleanup function to return resources to pools
	cleanup := func() {
		if pendingBuf != nil {
			putStringBuilder(pendingBuf)
		}
		for _, f := range futures {
			putRenderFuture(f)
		}
	}

	// Collect results from all futures (all work has completed after eg.Wait())
	results := make([]string, len(segments))
	for i, seg := range segments {
		switch seg.kind {
		case outputSegmentLiteral:
			results[i] = seg.literal
			r.finalSpans = append(r.finalSpans, seg.srcSpans...)
		case outputSegmentFuture:
			// Work has completed, so exactly one channel has a value
			select {
			case err := <-seg.future.err:
				cleanup()
				return err
			case result := <-seg.future.result:
				results[i] = result
				// Attribute the whole parallel-rendered block to its go-render
				// call site (one span; per-line within is not tracked).
				if r.collectSrc {
					r.finalSpans = append(r.finalSpans, SourceSpan{Frames: seg.goFrames, Length: len(result), IsText: false})
				}
			}
		case outputSegmentTextFragment:
			if r.collectSrc {
				cleanup()
				return errors.New("text fragment was retained while collecting a source map")
			}
		default:
			cleanup()
			return errors.New("invalid renderer output segment")
		}
	}

	// Write all segments to output in order
	for i, seg := range segments {
		var err error
		if seg.kind == outputSegmentTextFragment {
			err = emitTextFragment(r.out, seg.fragment)
		} else {
			_, err = io.WriteString(r.out, results[i])
		}
		if err != nil {
			cleanup()
			return err
		}
	}

	cleanup()
	return nil
}

// writeForParallel writes to either the pending buffer (if parallel rendering
// is active) or directly to out.
func (r *renderer) writeForParallel(p []byte) (int, error) {
	if r.collectSrc {
		return r.recW.Write(p)
	}
	if r.pendingBuf != nil {
		return r.pendingBuf.Write(p)
	}
	return r.out.Write(p)
}

// writeStringForParallel writes a string to either the pending buffer (if
// parallel rendering is active) or directly to out.
func (r *renderer) writeStringForParallel(s string) (int, error) {
	if r.collectSrc {
		return r.recW.WriteString(s)
	}
	if r.pendingBuf != nil {
		return r.pendingBuf.WriteString(s)
	}
	return io.WriteString(r.out, s)
}
