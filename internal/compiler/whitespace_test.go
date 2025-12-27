// Copyright 2024 The Scriggo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package compiler

import (
	"bytes"
	"testing"

	"gitlab.com/haproxy-haptic/scriggo/ast"
	"gitlab.com/haproxy-haptic/scriggo/internal/fstest"
)

// TestJinja2WhitespaceControl tests the Jinja2-style whitespace control markers.
func TestJinja2WhitespaceControl(t *testing.T) {
	tests := []struct {
		name     string
		template string
		want     string
	}{
		// Basic trim left tests
		{
			name:     "trim left with statement",
			template: "hello   \n  {%- if true %}world{% end %}",
			want:     "helloworld",
		},
		{
			name:     "trim left with expression",
			template: "hello   \n  {{- \"world\" }}",
			want:     "helloworld",
		},

		// Basic trim right tests
		{
			name:     "trim right with statement",
			template: "{% if true -%}   \n   world{% end %}",
			want:     "world",
		},
		{
			name:     "trim right with expression",
			template: "{{ \"hello\" -}}   \n   world",
			want:     "helloworld",
		},

		// Both trim markers
		{
			name:     "both trim markers on statement",
			template: "a   {%- if true -%}   b{% end %}",
			want:     "ab",
		},
		{
			name:     "both trim markers on expression",
			template: "a   {{- \"b\" -}}   c",
			want:     "abc",
		},

		// Negative numbers should not be treated as trim markers
		{
			name:     "negative number not trim marker",
			template: "{{-5}}",
			want:     "-5",
		},
		{
			name:     "negative number with space",
			template: "{{ -5 }}",
			want:     "-5",
		},

		// Trim with space is a trim marker
		{
			name:     "trim marker with space",
			template: "a  {{- 5 }}",
			want:     "a5",
		},

		// Multiple trims in sequence
		{
			name:     "multiple trims",
			template: "a   \n   {%- if true -%}   \n   b   \n   {%- end -%}   \n   c",
			want:     "abc",
		},

		// Comments with trim
		{
			name:     "trim left with comment",
			template: "hello   \n   {#- this is a comment #}world",
			want:     "helloworld",
		},
		{
			name:     "trim right with comment",
			template: "hello{# comment -#}   \n   world",
			want:     "helloworld",
		},

		// No trim (regular behavior)
		{
			name:     "no trim preserves whitespace",
			template: "hello {{ \"world\" }}",
			want:     "hello world",
		},

		// Edge case: trim marker at start of template
		{
			name:     "trim at start has nothing to trim",
			template: "{{- \"hello\" }}",
			want:     "hello",
		},

		// Edge case: trim marker at end of template
		{
			name:     "trim at end has nothing to trim",
			template: "{{ \"hello\" -}}",
			want:     "hello",
		},

		// ===== Show Statement Tests ({{ }}) =====
		// Bug report case - show between text and {%-
		{
			name:     "trim left after show should not affect previous text",
			template: `check{%- if true %} proto {{ "h2" }}{%- end %}`,
			want:     `check proto h2`,
		},
		// Multiple shows in sequence
		{
			name:     "trim left after multiple shows",
			template: `a{%- if true %}{{ "x" }}{{ "y" }}{{ "z" }}{%- end %}`,
			want:     `axyz`,
		},
		// Show with its own trim markers
		{
			name:     "trim left after show with trim markers",
			template: `a   {%- if true %}b   {{- "c" -}}   d{%- end %}`,
			want:     `abcd`,
		},
		// Show at start of block
		{
			name:     "trim left at start of block with show",
			template: `prefix   {%- if true %}{{ "value" }}{%- end %}`,
			want:     `prefixvalue`,
		},
		// Only show in block
		{
			name:     "trim left with only show in block",
			template: `a   {%- if true %}{{ "b" }}{%- end %}   c`,
			want:     `ab   c`,
		},

		// ===== Comment Tests ({# #}) =====
		// Comment between text and {%-
		{
			name:     "trim left after comment preserves previous text",
			template: `a   {%- if true %}b   {# comment #}{%- end %}`,
			want:     `ab   `,
		},
		// Comment with trim markers
		{
			name:     "comment with trim followed by trim left",
			template: `a   {%- if true %}b   {#- comment -#}   {%- end %}`,
			want:     `ab`,
		},
		// Multiple comments
		{
			name:     "trim left after multiple comments",
			template: `a{%- if true %}{# c1 #}{# c2 #}{# c3 #}{%- end %}`,
			want:     `a`,
		},

		// ===== Nested Control Flow Tests =====
		// Nested if with show
		{
			name:     "trim left with nested if and show",
			template: `outer{%- if true %} inner {{ "value" }}{%- if true %} more{%- end %}{%- end %}`,
			want:     `outer inner value more`,
		},

		// ===== Mixed Node Type Tests =====
		// Show, comment, show sequence
		{
			name:     "show comment show sequence with trim",
			template: `a{%- if true %}{{ "b" }}{# c #}{{ "d" }}{%- end %}`,
			want:     `abd`,
		},
		// Text, show, text, trim pattern
		{
			name:     "text show text trim preserves all spacing",
			template: `{%- if true %}a {{ "b" }} c{%- end %}`,
			want:     `a b c`,
		},
		// Complex real-world pattern
		{
			name:     "haproxy-style config generation",
			template: `server s1 127.0.0.1:8080{%- if true %} check{%- end %}{%- if true %} proto {{ "h2" }}{%- end %}`,
			want:     `server s1 127.0.0.1:8080 check proto h2`,
		},

		// ===== Edge Cases - Verify Existing Behavior Still Works =====
		// Text-only (original behavior)
		{
			name:     "trim left with only text (baseline)",
			template: `a   {%- if true %}b   {%- end %}`,
			want:     `ab`,
		},
		// Empty block
		{
			name:     "trim left with empty block",
			template: `a   {%- if true %}{%- end %}   b`,
			want:     `a   b`,
		},
		// No trim markers (control)
		{
			name:     "no trim markers preserves all spacing",
			template: `a   {% if true %}b {{ "c" }} d{% end %}   e`,
			want:     `a   b c d   e`,
		},
		// Only trim right
		{
			name:     "trim right only should not affect shows",
			template: `{% if true %}a {{ "b" }} c   {%- end %}   d`,
			want:     `a b c   d`,
		},

		// ===== Trim Marker Position Tests =====
		// Trim on opening tag only
		{
			name:     "trim left on opening tag with show",
			template: `a   {%- if true %} b {{ "c" }}{% end %}`,
			want:     `a b c`,
		},
		// Trim on closing tag only
		{
			name:     "trim left on closing tag with show",
			template: `{% if true %}a {{ "b" }} c{%- end %}`,
			want:     `a b c`,
		},
		// Both opening and closing
		{
			name:     "trim on both tags with show",
			template: `a   {%- if true %}b {{ "c" }}{% end -%}   d`,
			want:     `ab cd`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Parse the template
			tree, _, err := ParseTemplateSource([]byte(tt.template), ast.FormatText, false, false)
			if err != nil {
				t.Fatalf("ParseTemplateSource failed: %v", err)
			}

			// Render the tree by walking it and collecting text
			var buf bytes.Buffer
			renderTree(&buf, tree.Nodes)
			got := buf.String()

			if got != tt.want {
				t.Errorf("template %q:\ngot:  %q\nwant: %q", tt.template, got, tt.want)
			}
		})
	}
}

// renderTree walks the AST and renders it to a string.
// This is a simplified renderer that only handles Text nodes and Show nodes.
func renderTree(buf *bytes.Buffer, nodes []ast.Node) {
	for _, node := range nodes {
		switch n := node.(type) {
		case *ast.Text:
			text := n.Text
			// Apply cuts
			start := n.Cut.Left
			end := len(text) - n.Cut.Right
			if start < end {
				buf.WriteString(string(text[start:end]))
			}
		case *ast.Show:
			// For shows, we need to evaluate the expression
			// For test purposes, just handle string literals and identifiers
			if lit, ok := n.Expressions[0].(*ast.BasicLiteral); ok {
				switch lit.Type {
				case ast.StringLiteral:
					// Remove quotes
					s := lit.Value
					if len(s) >= 2 {
						s = s[1 : len(s)-1]
					}
					buf.WriteString(s)
				case ast.IntLiteral:
					buf.WriteString(lit.Value)
				}
			} else if unary, ok := n.Expressions[0].(*ast.UnaryOperator); ok {
				// Handle negative numbers like -5
				if unary.Op == ast.OperatorSubtraction {
					if lit, ok := unary.Expr.(*ast.BasicLiteral); ok && lit.Type == ast.IntLiteral {
						buf.WriteString("-")
						buf.WriteString(lit.Value)
					}
				}
			} else if ident, ok := n.Expressions[0].(*ast.Identifier); ok {
				// Handle identifiers - for test purposes, use simple values
				switch ident.Name {
				case "i", "j":
					// For loop indices - the actual value would come from runtime
					// For testing, we'll just use the index value from context
					buf.WriteString(ident.Name)
				case "x":
					buf.WriteString("val")
				default:
					buf.WriteString("value")
				}
			}
		case *ast.If:
			// Just render the then block for testing
			if n.Then != nil {
				renderTree(buf, n.Then.Nodes)
			}
		case *ast.For:
			// For testing, just render the body once (C-style for loop)
			renderTree(buf, n.Body)
		case *ast.ForRange:
			// For testing, try to determine iteration count from the range expression
			times := 2 // Default
			if n.Assignment != nil && len(n.Assignment.Rhs) > 0 {
				// Check for composite literal in the range expression
				if comp, ok := n.Assignment.Rhs[0].(*ast.CompositeLiteral); ok {
					times = len(comp.KeyValues)
				}
			}
			for i := 0; i < times; i++ {
				renderTree(buf, n.Body)
			}
		case *ast.Comment:
			// Comments are not rendered
			continue
		}
	}
}

// TestLexerTrimMarkers tests that the lexer correctly recognizes trim markers.
func TestLexerTrimMarkers(t *testing.T) {
	tests := []struct {
		name            string
		template        string
		wantTrimLeft    bool
		wantTrimRight   bool
		tokenType       tokenTyp
	}{
		{
			name:          "{{- is trim left",
			template:      "{{- x }}",
			wantTrimLeft:  true,
			wantTrimRight: false,
			tokenType:     tokenLeftBraces,
		},
		{
			name:          "-}} is trim right",
			template:      "{{ x -}}",
			wantTrimLeft:  false,
			wantTrimRight: true,
			tokenType:     tokenRightBraces,
		},
		{
			name:          "{{-5 is NOT trim (negative number)",
			template:      "{{-5}}",
			wantTrimLeft:  false,
			wantTrimRight: false,
			tokenType:     tokenLeftBraces,
		},
		{
			name:          "{%- is trim left",
			template:      "{%- if true %}",
			wantTrimLeft:  true,
			wantTrimRight: false,
			tokenType:     tokenStartStatement,
		},
		{
			name:          "-%} is trim right",
			template:      "{% if true -%}",
			wantTrimLeft:  false,
			wantTrimRight: true,
			tokenType:     tokenEndStatement,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			lex := scanTemplate([]byte(tt.template), ast.FormatText, false)
			tokenCh := lex.Tokens()

			// Collect tokens
			var tokens []token
			for tok := range tokenCh {
				tokens = append(tokens, tok)
			}

			// Find the relevant token
			var found *token
			for i := range tokens {
				if tokens[i].typ == tt.tokenType {
					found = &tokens[i]
					if tt.wantTrimLeft || tt.wantTrimRight {
						// For trim markers, we want the token that has the flag set
						if (tt.wantTrimLeft && tokens[i].trimLeft) || (tt.wantTrimRight && tokens[i].trimRight) {
							break
						}
					} else {
						// For non-trim, the first match is fine
						break
					}
				}
			}

			if found == nil {
				t.Fatalf("token type %v not found", tt.tokenType)
			}

			if found.trimLeft != tt.wantTrimLeft {
				t.Errorf("trimLeft = %v, want %v", found.trimLeft, tt.wantTrimLeft)
			}

			// For trimRight, check the corresponding end token
			if tt.wantTrimRight {
				var endToken *token
				for i := range tokens {
					if (tokens[i].typ == tokenRightBraces || tokens[i].typ == tokenEndStatement) && tokens[i].trimRight {
						endToken = &tokens[i]
						break
					}
				}
				if endToken == nil || !endToken.trimRight {
					t.Errorf("trimRight = false, want true")
				}
			}
		})
	}
}

// TestWhitespaceControlEmitter tests that whitespace control doesn't cause
// slice bounds panics in the emitter.
func TestWhitespaceControlEmitter(t *testing.T) {
	tests := []struct {
		name     string
		template string
	}{
		{
			name:     "trim between macro and for",
			template: "{% macro X() string %}\n  {%- for i in []int{} -%}{%- end -%}\n{% end %}",
		},
		{
			name:     "trim left after newline",
			template: "{% if true %}\n{%- end %}",
		},
		{
			name:     "consecutive trim markers",
			template: "{%- if true -%}\n\n{%- end -%}",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fsys := fstest.Files{"test.txt": tt.template}
			_, err := BuildTemplate(fsys, "test.txt", Options{})
			if err != nil {
				t.Fatalf("BuildTemplate failed: %v", err)
			}
		})
	}
}
