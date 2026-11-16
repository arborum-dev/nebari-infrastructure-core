package rke2test

import (
	"context"
	"errors"
	"io/fs"
	"reflect"
	"strings"
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

func TestNewHost(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name    string
		osName  string
		wantErr string // "" means rke2.Check passes
	}{
		{"Linux passes the host check", "Linux", ""},
		{"Darwin fails the host check", "Darwin", "Darwin"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := NewHost(tt.osName)
			err := rke2.Check(ctx, f)
			switch {
			case tt.wantErr == "" && err != nil:
				t.Fatalf("Check: %v", err)
			case tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)):
				t.Fatalf("Check error = %v, want one containing %q", err, tt.wantErr)
			}
			state, err := rke2.Inspect(ctx, f)
			if err != nil {
				t.Fatalf("Inspect: %v", err)
			}
			if !reflect.DeepEqual(state, rke2.State{}) {
				t.Errorf("Inspect = %+v, want a host without RKE2", state)
			}
		})
	}
}

func TestFakeRunner_SimulateInstalled(t *testing.T) {
	const v1, v2 = "v1.37.2+rke2r1", "v1.37.3+rke2r1"
	cfg := []byte("tls-san:\n- 10.0.0.5\n")
	tests := []struct {
		name     string
		simulate func(f *FakeRunner)
		want     rke2.State
	}{
		{
			name:     "running, enabled, with config",
			simulate: func(f *FakeRunner) { f.SimulateInstalled(v1, true, true, cfg) },
			want:     rke2.State{InstalledVersion: v1, Config: cfg, ServiceActive: true, ServiceEnabled: true},
		},
		{
			name:     "stopped, disabled, no config",
			simulate: func(f *FakeRunner) { f.SimulateInstalled(v1, false, false, nil) },
			want:     rke2.State{InstalledVersion: v1},
		},
		{
			name: "a later call wins and nil config keeps the file",
			simulate: func(f *FakeRunner) {
				f.SimulateInstalled(v1, true, true, cfg)
				f.SimulateInstalled(v2, false, true, nil)
			},
			want: rke2.State{InstalledVersion: v2, Config: cfg, ServiceEnabled: true},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := NewHost("Linux")
			tt.simulate(f)
			if err := rke2.Check(context.Background(), f); err != nil {
				t.Fatalf("Check: %v", err)
			}
			got, err := rke2.Inspect(context.Background(), f)
			if err != nil {
				t.Fatalf("Inspect: %v", err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("Inspect = %+v, want %+v", got, tt.want)
			}
			if len(f.Writes) != 0 {
				t.Errorf("Writes = %v, want none: simulated state is not a write", f.Writes)
			}
		})
	}
}
