package rke2_test

import (
	"context"
	"strings"
	"testing"

	"github.com/nebari-dev/nebari-infrastructure-core/pkg/rke2"
	"github.com/nebari-dev/nebari-infrastructure-core/pkg/rke2/rke2test"
)

const (
	localScript = "/usr/local/bin/rke2-uninstall.sh"
	rpmScript   = "/usr/bin/rke2-uninstall.sh"
	probeMatch  = "[ -x"
	rmLonghorn  = "rm -rf -- '/var/lib/longhorn'"
)

// scriptRuns counts calls that execute path but are not the detection probe.
func scriptRuns(r *rke2test.FakeRunner, path string) int {
	n := 0
	for _, c := range r.Calls {
		if !strings.Contains(c, probeMatch) && strings.Contains(c, path) {
			n++
		}
	}
	return n
}

func TestUninstall(t *testing.T) {
	dirs := []string{"/var/lib/longhorn"}
	tests := []struct {
		name       string
		responses  []rke2test.Response
		wantScript string // the only script expected to run; "" means none
		wantRm     bool
		wantErr    bool
	}{
		{
			name:       "installed",
			responses:  []rke2test.Response{{Match: probeMatch, Stdout: localScript + "\n"}},
			wantScript: localScript,
			wantRm:     true,
		},
		{
			name:       "RPM layout",
			responses:  []rke2test.Response{{Match: probeMatch, Stdout: rpmScript + "\n"}},
			wantScript: rpmScript,
			wantRm:     true,
		},
		{
			name:   "never installed",
			wantRm: true,
		},
		{
			name: "script fails",
			responses: []rke2test.Response{
				{Match: probeMatch, Stdout: localScript + "\n"},
				{Match: "'" + localScript + "'", Err: &rke2.ExitError{Code: 1, Stderr: "boom"}},
			},
			wantScript: localScript,
			wantErr:    true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := &rke2test.FakeRunner{Responses: tt.responses}
			err := rke2.Uninstall(context.Background(), r, dirs)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			for _, s := range []string{localScript, rpmScript} {
				want := 0
				if s == tt.wantScript {
					want = 1
				}
				if got := scriptRuns(r, s); got != want {
					t.Errorf("%s ran %d times, want %d; calls: %q", s, got, want, r.Calls)
				}
			}
			if got := r.Ran(rmLonghorn); got != tt.wantRm {
				t.Errorf("rm ran = %v, want %v; calls: %q", got, tt.wantRm, r.Calls)
			}
			if tt.wantRm && tt.wantScript != "" {
				if last := r.Calls[len(r.Calls)-1]; !strings.Contains(last, "rm -rf") {
					t.Errorf("rm should run after the script; calls: %q", r.Calls)
				}
			}
		})
	}
}

func TestUninstallRejectsUnsafeDirs(t *testing.T) {
	bad := []string{"/", "/var", "var/lib/x", "/var/lib/../..", "", "/var/lib/longhorn/", "/var//lib/x", "/var/lib/.", "/a/b"}
	for _, dir := range bad {
		t.Run(dir, func(t *testing.T) {
			r := &rke2test.FakeRunner{}
			if err := rke2.Uninstall(context.Background(), r, []string{"/var/lib/longhorn", dir}); err == nil {
				t.Fatal("expected error")
			}
			if len(r.Calls) != 0 {
				t.Errorf("commands ran before validation: %q", r.Calls)
			}
		})
	}
}

func TestUninstallQuotesPaths(t *testing.T) {
	r := &rke2test.FakeRunner{}
	if err := rke2.Uninstall(context.Background(), r, []string{"/var/lib/it's here"}); err != nil {
		t.Fatal(err)
	}
	if !r.Ran(`rm -rf -- '/var/lib/it'\''s here'`) {
		t.Errorf("path not quoted; calls: %q", r.Calls)
	}
}
