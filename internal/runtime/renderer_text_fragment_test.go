// Copyright 2026 The Scriggo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime

import (
	"errors"
	"io"
	"strings"
	"testing"
)

type textFragmentFunc func(io.Writer) (int64, error)

func (f textFragmentFunc) WriteTo(writer io.Writer) (int64, error) {
	return f(writer)
}

type textFragmentFixedWriter struct {
	count int
	err   error
}

func (w textFragmentFixedWriter) Write([]byte) (int, error) {
	return w.count, w.err
}

func TestWriteTextFragmentValidatesReportedCount(t *testing.T) {
	tests := []struct {
		name     string
		fragment textFragmentFunc
		want     string
		wantErr  error
	}{
		{
			name: "valid",
			fragment: func(writer io.Writer) (int64, error) {
				n, err := io.WriteString(writer, "abc")
				return int64(n), err
			},
			want: "abc",
		},
		{
			name: "negative",
			fragment: func(io.Writer) (int64, error) {
				return -1, nil
			},
			wantErr: errInvalidTextFragmentWriteCount,
		},
		{
			name: "underreported",
			fragment: func(writer io.Writer) (int64, error) {
				_, err := io.WriteString(writer, "abc")
				return 2, err
			},
			want:    "abc",
			wantErr: errInvalidTextFragmentWriteCount,
		},
		{
			name: "overreported",
			fragment: func(writer io.Writer) (int64, error) {
				_, err := io.WriteString(writer, "abc")
				return 4, err
			},
			want:    "abc",
			wantErr: errInvalidTextFragmentWriteCount,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var output strings.Builder
			err := writeTextFragment(&output, test.fragment)
			if !errors.Is(err, test.wantErr) {
				t.Fatalf("error = %v, want %v", err, test.wantErr)
			}
			if got := output.String(); got != test.want {
				t.Fatalf("output = %q, want %q", got, test.want)
			}
		})
	}
}

func TestWriteTextFragmentValidatesUnderlyingWriter(t *testing.T) {
	sentinel := errors.New("write failed")
	fragment := textFragmentFunc(func(writer io.Writer) (int64, error) {
		n, err := writer.Write([]byte("abc"))
		return int64(n), err
	})
	tests := []struct {
		name    string
		writer  io.Writer
		wantErr error
	}{
		{name: "short", writer: textFragmentFixedWriter{count: 2}, wantErr: io.ErrShortWrite},
		{name: "negative", writer: textFragmentFixedWriter{count: -1}, wantErr: errInvalidTextFragmentWriteCount},
		{name: "oversize", writer: textFragmentFixedWriter{count: 4}, wantErr: errInvalidTextFragmentWriteCount},
		{name: "error", writer: textFragmentFixedWriter{count: 1, err: sentinel}, wantErr: sentinel},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := writeTextFragment(test.writer, fragment)
			if !errors.Is(err, test.wantErr) {
				t.Fatalf("error = %v, want %v", err, test.wantErr)
			}
		})
	}
}

func TestWriteTextFragmentKeepsSuppressedWriterError(t *testing.T) {
	sentinel := errors.New("write failed")
	fragment := textFragmentFunc(func(writer io.Writer) (int64, error) {
		n, _ := writer.Write([]byte("abc"))
		return int64(n), nil
	})
	err := writeTextFragment(textFragmentFixedWriter{count: 1, err: sentinel}, fragment)
	if !errors.Is(err, sentinel) {
		t.Fatalf("error = %v, want %v", err, sentinel)
	}
}
