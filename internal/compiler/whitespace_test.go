// Copyright 2024 The Scriggo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package compiler

import (
	"bytes"
	"testing"

	"github.com/open2b/scriggo/ast"
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
			// For test purposes, just handle string literals
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
			}
		case *ast.If:
			// Just render the then block for testing
			if n.Then != nil {
				renderTree(buf, n.Then.Nodes)
			}
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
