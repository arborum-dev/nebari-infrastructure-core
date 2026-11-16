package rke2

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"reflect"
	"strings"
	"testing"

	"github.com/nebari-dev/nebari-infrastructure-core/pkg/rke2/rke2test"
)

const (
	testV1 = "v1.37.2+rke2r1"
	testV2 = "v1.37.3+rke2r1"
)

func testConfig(t *testing.T) []byte {
	t.Helper()
	cfg, err := RenderConfig("10.0.0.5", nil)
	if err != nil {
		t.Fatalf("RenderConfig: %v", err)
	}
	return cfg
}

func TestPlanInstall(t *testing.T) {
	cfg := testConfig(t)
	opts := func(version string, boot bool) Options {
		return Options{Version: version, APIServerAddress: "10.0.0.5", StartOnBoot: boot}
	}

	tests := []struct {
		name    string
		state   State
		opts    Options
		want    Plan // Config is filled in by the test when WriteConfig is true
		wantErr string
	}{
		{
			name:  "bare host",
			state: State{},
			opts:  opts(testV1, true),
			want:  Plan{WriteConfig: true, Install: true, Enable: true, Start: true},
		},
		{
			name:  "bare host, no boot start",
			state: State{},
			opts:  opts(testV1, false),
			want:  Plan{WriteConfig: true, Install: true, Start: true},
		},
		{
			name:  "nothing changed",
			state: State{InstalledVersion: testV1, Config: cfg, ServiceActive: true, ServiceEnabled: true},
			opts:  opts(testV1, true),
			want:  Plan{},
		},
		{
			name:  "config changed",
			state: State{InstalledVersion: testV1, Config: []byte("old: true\n"), ServiceActive: true, ServiceEnabled: true},
			opts:  opts(testV1, true),
			want:  Plan{WriteConfig: true, Restart: true},
		},
		{
			name:  "config changed while stopped",
			state: State{InstalledVersion: testV1, Config: []byte("old: true\n"), ServiceEnabled: true},
			opts:  opts(testV1, true),
			want:  Plan{WriteConfig: true, Start: true},
		},
		{
			name:  "installed but stopped",
			state: State{InstalledVersion: testV1, Config: cfg},
			opts:  opts(testV1, false),
			want:  Plan{Start: true},
		},
		{
			name:  "upgrade while running",
			state: State{InstalledVersion: testV1, Config: cfg, ServiceActive: true, ServiceEnabled: true},
			opts:  opts(testV2, true),
			want:  Plan{Install: true, Restart: true},
		},
		{
			name:  "upgrade while stopped",
			state: State{InstalledVersion: testV1, Config: cfg, ServiceEnabled: true},
			opts:  opts(testV2, true),
			want:  Plan{Install: true, Start: true},
		},
		{
			name:    "downgrade",
			state:   State{InstalledVersion: testV2, Config: cfg, ServiceActive: true, ServiceEnabled: true},
			opts:    opts(testV1, true),
			wantErr: "refusing to downgrade",
		},
		{
			name:  "boot start turned off",
			state: State{InstalledVersion: testV1, Config: cfg, ServiceActive: true, ServiceEnabled: true},
			opts:  opts(testV1, false),
			want:  Plan{Disable: true},
		},
		{
			name:  "boot start turned on",
			state: State{InstalledVersion: testV1, Config: cfg, ServiceActive: true},
			opts:  opts(testV1, true),
			want:  Plan{Enable: true},
		},
		{
			name:  "colima steady state: running, not started at boot",
			state: State{InstalledVersion: testV1, Config: cfg, ServiceActive: true},
			opts:  opts(testV1, false),
			want:  Plan{},
		},
		{
			name:    "not installed but service active",
			state:   State{ServiceActive: true},
			opts:    opts(testV1, true),
			wantErr: "/opt/rke2/bin/rke2",
		},
		{
			name:    "not installed but service enabled",
			state:   State{ServiceEnabled: true},
			opts:    opts(testV1, true),
			wantErr: "binary was not found",
		},
		{
			name:    "unparsable installed version",
			state:   State{InstalledVersion: "garbage", Config: cfg},
			opts:    opts(testV1, true),
			wantErr: "garbage",
		},
		{
			name:    "unparsable requested version",
			state:   State{},
			opts:    opts("latest", true),
			wantErr: "latest",
		},
		{
			name:    "empty api server address",
			state:   State{},
			opts:    Options{Version: testV1},
			wantErr: "api server address",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := PlanInstall(tt.state, tt.opts)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("PlanInstall error = %v, want containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("PlanInstall: %v", err)
			}
			want := tt.want
			if want.WriteConfig {
				want.Config = cfg
			}
			if want.Install {
				want.Version = tt.opts.Version
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("PlanInstall = %+v, want %+v", got, want)
			}
			if got.Empty() != reflect.DeepEqual(tt.want, Plan{}) {
				t.Errorf("Empty() = %v for %+v", got.Empty(), got)
			}
		})
	}
}

func TestPlanDescribe(t *testing.T) {
	tests := []struct {
		name string
		plan Plan
		want int
	}{
		{"empty", Plan{}, 0},
		{"bare host", Plan{WriteConfig: true, Install: true, Enable: true, Start: true}, 4},
		{"upgrade", Plan{Install: true, Restart: true}, 2},
		{"disable", Plan{Disable: true}, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.plan.Describe()
			if len(got) != tt.want {
				t.Errorf("Describe() = %q, want %d lines", got, tt.want)
			}
			for _, line := range got {
				if strings.TrimSpace(line) == "" {
					t.Errorf("Describe() has a blank line: %q", got)
				}
			}
		})
	}
}

func TestPlanDescribeNamesVersion(t *testing.T) {
	got := Plan{Install: true, Version: testV2, Restart: true}.Describe()
	if len(got) == 0 || got[0] != "install RKE2 "+testV2 {
		t.Errorf("Describe() = %q, want it to start with %q", got, "install RKE2 "+testV2)
	}
}

func TestInspect(t *testing.T) {
	cfg := []byte("tls-san:\n- 10.0.0.5\n")
	tests := []struct {
		name      string
		responses []rke2test.Response
		files     map[string][]byte
		want      State
		wantErr   bool
	}{
		{
			name: "installed, active and enabled",
			responses: []rke2test.Response{
				{Match: "--version", Stdout: "rke2 version " + testV1 + " (abc123)\ngo version go1.24\n"},
				{Match: "is-active", Stdout: "active\n"},
				{Match: "is-enabled", Stdout: "enabled\n"},
			},
			files: map[string][]byte{ConfigPath: cfg},
			want:  State{InstalledVersion: testV1, Config: cfg, ServiceActive: true, ServiceEnabled: true},
		},
		{
			name: "installed, inactive and disabled",
			responses: []rke2test.Response{
				{Match: "--version", Stdout: "rke2 version " + testV1 + " (abc123)\n"},
				{Match: "is-active", Stdout: "inactive\n"},
				{Match: "is-enabled", Stdout: "disabled\n"},
			},
			want: State{InstalledVersion: testV1},
		},
		{
			name: "starting up counts as active",
			responses: []rke2test.Response{
				{Match: "--version", Stdout: "rke2 version " + testV1 + " (abc123)\n"},
				{Match: "is-active", Stdout: "activating\n"},
				{Match: "is-enabled", Stdout: "enabled\n"},
			},
			want: State{InstalledVersion: testV1, ServiceActive: true, ServiceEnabled: true},
		},
		{
			name: "reloading counts as active",
			responses: []rke2test.Response{
				{Match: "--version", Stdout: "rke2 version " + testV1 + " (abc123)\n"},
				{Match: "is-active", Stdout: "reloading\n"},
			},
			want: State{InstalledVersion: testV1, ServiceActive: true},
		},
		{
			name: "failed counts as stopped",
			responses: []rke2test.Response{
				{Match: "--version", Stdout: "rke2 version " + testV1 + " (abc123)\n"},
				{Match: "is-active", Stdout: "failed\n"},
			},
			want: State{InstalledVersion: testV1},
		},
		{
			name: "bare host",
			want: State{},
		},
		{
			name: "binary present but --version fails",
			responses: []rke2test.Response{
				{Match: "--version", Err: &ExitError{Code: 126, Stderr: "exec format error"}},
			},
			wantErr: true,
		},
		{
			name: "unrecognised version output",
			responses: []rke2test.Response{
				{Match: "--version", Stdout: "something odd\n"},
			},
			wantErr: true,
		},
		{
			name: "probe error",
			responses: []rke2test.Response{
				{Match: "is-active", Err: &ExitError{Code: 1}},
			},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := &rke2test.FakeRunner{Responses: tt.responses, Files: tt.files}
			got, err := Inspect(context.Background(), f)
			if (err != nil) != tt.wantErr {
				t.Fatalf("Inspect error = %v, wantErr %v", err, tt.wantErr)
			}
			if err != nil {
				return
			}
			if got.InstalledVersion != tt.want.InstalledVersion ||
				got.ServiceActive != tt.want.ServiceActive ||
				got.ServiceEnabled != tt.want.ServiceEnabled ||
				!bytes.Equal(got.Config, tt.want.Config) || (got.Config == nil) != (tt.want.Config == nil) {
				t.Errorf("Inspect = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestCheck(t *testing.T) {
	sudoErr := &ExitError{Code: 1, Stderr: "sudo: a password is required"}
	ok := func(os, arch, systemd string) []rke2test.Response {
		return []rke2test.Response{
			{Match: "id -u", Stdout: "0\n"},
			{Match: "uname -s", Stdout: os + "\n"},
			{Match: "uname -m", Stdout: arch + "\n"},
			{Match: "/run/systemd/system", Stdout: systemd},
		}
	}
	tests := []struct {
		name      string
		responses []rke2test.Response
		wantErr   []string
	}{
		{"supported host aarch64", ok("Linux", "aarch64", "systemd\n"), nil},
		{"supported host x86_64", ok("Linux", "x86_64", "systemd\n"), nil},
		{"not Linux", ok("Darwin", "arm64", "systemd\n"), []string{"Linux", "Darwin"}},
		{"unsupported arch", ok("Linux", "riscv64", "systemd\n"), []string{"riscv64"}},
		{"no systemd", ok("Linux", "aarch64", ""), []string{"systemd"}},
		{"not root", []rke2test.Response{{Match: "id -u", Stdout: "1000\n"}}, []string{"root"}},
		{
			"no passwordless sudo",
			[]rke2test.Response{{Match: "id -u", Err: sudoErr}},
			[]string{"passwordless sudo", "a password is required"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := &rke2test.FakeRunner{Responses: tt.responses}
			err := Check(context.Background(), f)
			if len(tt.wantErr) == 0 {
				if err != nil {
					t.Fatalf("Check: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("Check returned nil, want an error")
			}
			for _, s := range tt.wantErr {
				if !strings.Contains(err.Error(), s) {
					t.Errorf("Check error = %q, want containing %q", err, s)
				}
			}
		})
	}
}

// recordingInstaller notes how far the fake had got when Install was called.
// Unless noop or err is set, it makes the fake's version probe report the
// installed version afterwards, as a real install would.
type recordingInstaller struct {
	f          *rke2test.FakeRunner
	versions   []string
	writesSeen int
	callsSeen  int
	err        error
	noop       bool // succeed without changing what the version probe reports
}

func (r *recordingInstaller) Install(_ context.Context, _ Runner, version string) error {
	r.versions = append(r.versions, version)
	r.writesSeen = len(r.f.Writes)
	r.callsSeen = len(r.f.Calls)
	if r.err == nil && !r.noop {
		installed := rke2test.Response{Match: "--version", Stdout: "rke2 version " + version + " (abc123)\n"}
		r.f.Responses = append([]rke2test.Response{installed}, r.f.Responses...)
	}
	return r.err
}

func TestApply(t *testing.T) {
	cfg := testConfig(t)
	tests := []struct {
		name          string
		plan          Plan
		wantWrites    []string
		wantInstalled bool
		wantRan       []string // in order
		wantNotRan    []string
	}{
		{
			name:          "bare host",
			plan:          Plan{WriteConfig: true, Config: cfg, Install: true, Version: testV1, Enable: true, Start: true},
			wantWrites:    []string{ConfigPath},
			wantInstalled: true,
			wantRan:       []string{"--version", "systemctl enable", "systemctl start"},
			wantNotRan:    []string{"restart", "disable"},
		},
		{
			name:       "empty plan",
			plan:       Plan{},
			wantNotRan: []string{"systemctl"},
		},
		{
			name:    "installed but stopped",
			plan:    Plan{Start: true},
			wantRan: []string{"systemctl start"},
		},
		{
			name:          "upgrade while running",
			plan:          Plan{Install: true, Version: testV2, Restart: true},
			wantInstalled: true,
			wantRan:       []string{"--version", "systemctl restart"},
			wantNotRan:    []string{"systemctl start"},
		},
		{
			name:       "boot start off",
			plan:       Plan{Disable: true},
			wantRan:    []string{"systemctl disable"},
			wantNotRan: []string{"systemctl enable"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := &rke2test.FakeRunner{}
			inst := &recordingInstaller{f: f}
			if err := Apply(context.Background(), f, inst, tt.plan); err != nil {
				t.Fatalf("Apply: %v", err)
			}
			if !reflect.DeepEqual(f.Writes, tt.wantWrites) {
				t.Errorf("Writes = %q, want %q", f.Writes, tt.wantWrites)
			}
			if tt.wantWrites != nil && !bytes.Equal(f.Files[ConfigPath], tt.plan.Config) {
				t.Errorf("config written = %q, want %q", f.Files[ConfigPath], tt.plan.Config)
			}
			if tt.wantWrites != nil && f.Modes[ConfigPath] != 0o600 {
				t.Errorf("config written with mode %o, want 600", f.Modes[ConfigPath])
			}
			if got := len(inst.versions) > 0; got != tt.wantInstalled {
				t.Errorf("installer called = %v, want %v", got, tt.wantInstalled)
			}
			if tt.wantInstalled && inst.versions[0] != tt.plan.Version {
				t.Errorf("installer version = %q, want %q", inst.versions[0], tt.plan.Version)
			}
			if tt.plan.Empty() && (len(f.Calls) != 0 || len(f.Writes) != 0) {
				t.Errorf("empty plan made calls %q writes %q", f.Calls, f.Writes)
			}
			if len(f.Calls) != len(tt.wantRan) {
				t.Errorf("Calls = %q, want one per %q", f.Calls, tt.wantRan)
			} else {
				for i, s := range tt.wantRan {
					if !strings.Contains(f.Calls[i], s) {
						t.Errorf("Calls[%d] = %q, want containing %q", i, f.Calls[i], s)
					}
				}
			}
			for _, s := range tt.wantNotRan {
				if f.Ran(s) {
					t.Errorf("ran %q, want not", s)
				}
			}
		})
	}
}

func TestApplyOrder(t *testing.T) {
	f := &rke2test.FakeRunner{}
	inst := &recordingInstaller{f: f}
	plan := Plan{WriteConfig: true, Config: []byte("a: b\n"), Install: true, Version: testV1, Enable: true, Start: true}
	if err := Apply(context.Background(), f, inst, plan); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if inst.writesSeen != 1 || inst.callsSeen != 0 {
		t.Errorf("installer saw %d writes and %d calls, want 1 and 0 (config first, then install, then systemctl)", inst.writesSeen, inst.callsSeen)
	}
	if len(f.Calls) != 3 {
		t.Fatalf("Calls = %q, want the version check, then enable, then start", f.Calls)
	}
}

func TestApplyInstallerError(t *testing.T) {
	boom := errors.New("download failed")
	f := &rke2test.FakeRunner{}
	inst := &recordingInstaller{f: f, err: boom}
	plan := Plan{Install: true, Version: testV1, Enable: true, Start: true}
	err := Apply(context.Background(), f, inst, plan)
	if !errors.Is(err, boom) {
		t.Fatalf("Apply error = %v, want %v", err, boom)
	}
	if !strings.HasPrefix(err.Error(), "install RKE2 "+testV1+": ") {
		t.Errorf("Apply error = %q, want it to start with %q", err, "install RKE2 "+testV1+": ")
	}
	if len(f.Calls) != 0 {
		t.Errorf("ran %q after the installer failed", f.Calls)
	}
}

func TestApplyOnlineInstallerErrorWording(t *testing.T) {
	failed := &ExitError{Code: 6, Stderr: "curl: (6) Could not resolve host: get.rke2.io"}
	f := &rke2test.FakeRunner{Responses: []rke2test.Response{{Match: "get.rke2.io", Err: failed}}}
	err := Apply(context.Background(), f, OnlineInstaller{}, Plan{Install: true, Version: testV1, Start: true})
	var exit *ExitError
	if !errors.As(err, &exit) {
		t.Fatalf("Apply error = %v, want an *ExitError inside", err)
	}
	if n := strings.Count(err.Error(), testV1); n != 1 {
		t.Errorf("Apply error = %q names the version %d times, want once", err, n)
	}
}

func TestApplyRejectsInvalidVersion(t *testing.T) {
	for _, version := range []string{"", "latest", "1.37.2"} {
		t.Run(version, func(t *testing.T) {
			f := &rke2test.FakeRunner{}
			inst := &recordingInstaller{f: f}
			plan := Plan{WriteConfig: true, Config: []byte("a: b\n"), Install: true, Version: version, Enable: true, Start: true}
			if err := Apply(context.Background(), f, inst, plan); err == nil {
				t.Fatal("Apply returned nil, want an error")
			}
			if len(f.Calls) != 0 || len(f.Writes) != 0 || len(inst.versions) != 0 {
				t.Errorf("Apply acted before rejecting the plan: calls %q writes %q installs %q", f.Calls, f.Writes, inst.versions)
			}
		})
	}
}

func TestApplyWriteErrorNamesPathOnce(t *testing.T) {
	denied := &fs.PathError{Op: "write", Path: ConfigPath, Err: fs.ErrPermission}
	w := &errWriter{err: denied}
	err := Apply(context.Background(), w, nil, Plan{WriteConfig: true, Config: []byte("a: b\n")})
	if !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("Apply error = %v, want permission error", err)
	}
	if n := strings.Count(err.Error(), ConfigPath); n != 1 {
		t.Errorf("Apply error = %q names %s %d times, want once", err, ConfigPath, n)
	}
}

type errWriter struct {
	rke2test.FakeRunner
	err error
}

func (e *errWriter) WriteFile(context.Context, string, []byte, fs.FileMode) error { return e.err }

func TestApplyMissingInstaller(t *testing.T) {
	f := &rke2test.FakeRunner{}
	if err := Apply(context.Background(), f, nil, Plan{Install: true, Version: testV1}); err == nil {
		t.Fatal("Apply with a nil installer returned nil")
	}
}

func TestOnlineInstaller(t *testing.T) {
	tests := []struct {
		name    string
		version string
		want    string
	}{
		{"release tag", testV1, "INSTALL_RKE2_VERSION='v1.37.2+rke2r1'"},
		{"hostile value is quoted", "v1'; rm -rf /; '", `INSTALL_RKE2_VERSION='v1'\''; rm -rf /; '\'''`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := &rke2test.FakeRunner{}
			if err := (OnlineInstaller{}).Install(context.Background(), f, tt.version); err != nil {
				t.Fatalf("Install: %v", err)
			}
			if len(f.Calls) != 1 {
				t.Fatalf("Calls = %q, want one script", f.Calls)
			}
			script := f.Calls[0]
			// The script is downloaded to a file and run from it, so a failed
			// download fails the install instead of feeding `sh -` nothing.
			for _, s := range []string{"https://get.rke2.io", tt.want, `-o "$tmp"`, `sh "$tmp"`, "exit $rc"} {
				if !strings.Contains(script, s) {
					t.Errorf("script %q does not contain %q", script, s)
				}
			}
			for _, s := range []string{"|", "sh -"} {
				if strings.Contains(script, s) {
					t.Errorf("script %q pipes the download into a shell (contains %q)", script, s)
				}
			}
		})
	}
}

func TestOnlineInstallerError(t *testing.T) {
	failed := &ExitError{Code: 22, Stderr: "curl: (22) The requested URL returned error: 404"}
	f := &rke2test.FakeRunner{Responses: []rke2test.Response{{Match: "get.rke2.io", Err: failed}}}
	err := (OnlineInstaller{}).Install(context.Background(), f, testV1)
	var exit *ExitError
	if !errors.As(err, &exit) || exit.Code != 22 {
		t.Fatalf("Install error = %v, want the *ExitError", err)
	}
}

func TestApplyVerifiesInstalledVersion(t *testing.T) {
	oldVersion := rke2test.Response{Match: "--version", Stdout: "rke2 version " + testV1 + " (abc123)\n"}
	tests := []struct {
		name      string
		responses []rke2test.Response
		plan      Plan
		wantErr   string
	}{
		{
			name:      "upgrade left the old version",
			responses: []rke2test.Response{oldVersion},
			plan:      Plan{Install: true, Version: testV2, Restart: true},
			wantErr:   testV1,
		},
		{
			name:    "fresh install left no binary",
			plan:    Plan{WriteConfig: true, Config: []byte("a: b\n"), Install: true, Version: testV1, Enable: true, Start: true},
			wantErr: "not found",
		},
		{
			name:      "version probe fails after install",
			responses: []rke2test.Response{{Match: "--version", Err: &ExitError{Code: 126, Stderr: "exec format error"}}},
			plan:      Plan{Install: true, Version: testV1, Start: true},
			wantErr:   "exec format error",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := &rke2test.FakeRunner{Responses: tt.responses}
			inst := &recordingInstaller{f: f, noop: true}
			err := Apply(context.Background(), f, inst, tt.plan)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) || !strings.Contains(err.Error(), tt.plan.Version) {
				t.Fatalf("Apply error = %v, want one naming %q and %q", err, tt.plan.Version, tt.wantErr)
			}
			if len(inst.versions) != 1 {
				t.Errorf("installer called %d times, want 1", len(inst.versions))
			}
			if f.Ran("systemctl") {
				t.Errorf("ran systemctl after a failed install: %q", f.Calls)
			}
		})
	}
}

func TestInspectReadFileError(t *testing.T) {
	denied := &fs.PathError{Op: "read", Path: ConfigPath, Err: fs.ErrPermission}
	r := &errReader{err: denied}
	_, err := Inspect(context.Background(), r)
	if !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("Inspect error = %v, want permission error", err)
	}
	if n := strings.Count(err.Error(), ConfigPath); n != 1 {
		t.Errorf("Inspect error = %q names %s %d times, want once", err, ConfigPath, n)
	}
}

type errReader struct {
	rke2test.FakeRunner
	err error
}

func (e *errReader) ReadFile(context.Context, string) ([]byte, error) { return nil, e.err }

func TestInstallPathsSharePrefixes(t *testing.T) {
	if len(binaryPaths) != len(installPrefixes) || len(uninstallScripts) != len(installPrefixes) {
		t.Fatalf("binaryPaths %q and uninstallScripts %q must have one entry per prefix %q", binaryPaths, uninstallScripts, installPrefixes)
	}
	for i, prefix := range installPrefixes {
		if want := prefix + "/bin/rke2"; binaryPaths[i] != want {
			t.Errorf("binaryPaths[%d] = %q, want %q", i, binaryPaths[i], want)
		}
		if want := prefix + "/bin/rke2-uninstall.sh"; uninstallScripts[i] != want {
			t.Errorf("uninstallScripts[%d] = %q, want %q", i, uninstallScripts[i], want)
		}
	}
}
