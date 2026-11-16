package rke2test

import (
	"context"
	"errors"
	"io/fs"
	"testing"

	"github.com/nebari-dev/nebari-infrastructure-core/pkg/rke2"
)

var _ rke2.Runner = (*FakeRunner)(nil)

func TestFakeRunner_ReadFileMissing(t *testing.T) {
	f := &FakeRunner{}
	_, err := f.ReadFile(context.Background(), "/nope")
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("err = %v, want fs.ErrNotExist", err)
	}
}

func TestFakeRunner_WriteThenRead(t *testing.T) {
	f := &FakeRunner{}
	ctx := context.Background()
	if err := f.WriteFile(ctx, "/a", []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := f.ReadFile(ctx, "/a")
	if err != nil || string(got) != "x" {
		t.Fatalf("ReadFile = %q, %v", got, err)
	}
	if len(f.Writes) != 1 || f.Writes[0] != "/a" {
		t.Fatalf("Writes = %v", f.Writes)
	}
	if got := f.Modes["/a"]; got != 0o600 {
		t.Errorf("Modes[/a] = %o, want 600", got)
	}
}

func TestFakeRunner_ResponsesMatchInOrder(t *testing.T) {
	boom := errors.New("boom")
	f := &FakeRunner{Responses: []Response{
		{Match: "systemctl", Stdout: "first"},
		{Match: "systemctl is-active", Stdout: "second"},
		{Match: "fail", Err: boom},
	}}
	ctx := context.Background()

	tests := []struct {
		script  string
		want    string
		wantErr error
	}{
		{"systemctl is-active rke2", "first", nil},
		{"do fail now", "", boom},
		{"unmatched", "", nil},
	}
	for _, tt := range tests {
		got, err := f.Run(ctx, tt.script)
		if got != tt.want || !errors.Is(err, tt.wantErr) {
			t.Errorf("Run(%q) = %q, %v; want %q, %v", tt.script, got, err, tt.want, tt.wantErr)
		}
	}
	if len(f.Calls) != 3 {
		t.Errorf("Calls = %v", f.Calls)
	}
	if !f.Ran("is-active") || f.Ran("absent") {
		t.Error("Ran gave wrong answer")
	}
}
