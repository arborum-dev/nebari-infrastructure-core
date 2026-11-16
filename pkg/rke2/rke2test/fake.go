// Package rke2test provides a fake rke2.Runner for tests.
package rke2test

import (
	"context"
	"io/fs"
	"strings"
)

// Response is a canned result for scripts containing Match.
type Response struct {
	Match  string // substring of the script
	Stdout string
	Err    error
}

// FakeRunner is an in-memory rke2.Runner. It never touches a host.
type FakeRunner struct {
	Files     map[string][]byte      // backs ReadFile and WriteFile
	Modes     map[string]fs.FileMode // the mode of the last WriteFile to each path
	Responses []Response             // first whose Match is contained in the script wins; no match returns "", nil
	Calls     []string               // every script passed to Run, in order
	Writes    []string               // every path passed to WriteFile, in order
}

// Run records script and returns the first matching Response.
func (f *FakeRunner) Run(_ context.Context, script string) (string, error) {
	f.Calls = append(f.Calls, script)
	for _, r := range f.Responses {
		if strings.Contains(script, r.Match) {
			return r.Stdout, r.Err
		}
	}
	return "", nil
}

// ReadFile returns the stored file or a *fs.PathError wrapping fs.ErrNotExist.
func (f *FakeRunner) ReadFile(_ context.Context, path string) ([]byte, error) {
	data, ok := f.Files[path]
	if !ok {
		return nil, &fs.PathError{Op: "read", Path: path, Err: fs.ErrNotExist}
	}
	return append([]byte(nil), data...), nil
}

// WriteFile stores a copy of data at path and records the write and its mode.
func (f *FakeRunner) WriteFile(_ context.Context, path string, data []byte, mode fs.FileMode) error {
	if f.Files == nil {
		f.Files = map[string][]byte{}
	}
	if f.Modes == nil {
		f.Modes = map[string]fs.FileMode{}
	}
	f.Files[path] = append([]byte(nil), data...)
	f.Modes[path] = mode
	f.Writes = append(f.Writes, path)
	return nil
}

// Close does nothing.
func (f *FakeRunner) Close() error { return nil }

// Ran reports whether any recorded script contains substr.
func (f *FakeRunner) Ran(substr string) bool {
	for _, c := range f.Calls {
		if strings.Contains(c, substr) {
			return true
		}
	}
	return false
}
