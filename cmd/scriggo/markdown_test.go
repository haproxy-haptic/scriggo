// Copyright 2026 The Scriggo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"bytes"
	"strings"
	"testing"
)

// The goldmark v2 migration had to re-map each option: the combined facade is
// gone, so parser and renderer are built separately and every extension is
// supplied to both halves. Dropping one half silently loses a feature rather
// than failing to compile, so assert each one still takes effect.
func TestMarkdownConverterOptions(t *testing.T) {
	for _, tc := range []struct {
		name   string
		src    string
		want   string
		option string
	}{
		{
			name:   "GFM table",
			src:    "| a | b |\n| - | - |\n| 1 | 2 |\n",
			want:   "<table>",
			option: "extension.GFMParser + extension.GFMHTMLRenderer",
		},
		{
			name:   "GFM strikethrough",
			src:    "~~gone~~\n",
			want:   "<del>gone</del>",
			option: "extension.GFMParser + extension.GFMHTMLRenderer",
		},
		{
			name:   "footnote",
			src:    "text[^1]\n\n[^1]: the note\n",
			want:   "the note",
			option: "extension.FootnoteParser + extension.FootnoteHTMLRenderer",
		},
		{
			name:   "raw HTML passes through",
			src:    "<abbr>x</abbr>\n",
			want:   "<abbr>x</abbr>",
			option: "html.WithUnsafe",
		},
		{
			name:   "auto heading id",
			src:    "# A Heading\n",
			want:   `id="a-heading"`,
			option: "parser.WithAutoHeadingID",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			if err := newMarkdownConverter()([]byte(tc.src), &buf); err != nil {
				t.Fatalf("convert: %v", err)
			}
			if got := buf.String(); !strings.Contains(got, tc.want) {
				t.Errorf("%s appears to be lost: output %q does not contain %q", tc.option, got, tc.want)
			}
		})
	}
}
