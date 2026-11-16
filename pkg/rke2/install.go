package rke2

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"regexp"
	"strings"
)

// Options is the desired state of the RKE2 server.
type Options struct {
	// Version is the exact RKE2 release tag to run, such as v1.37.2+rke2r1.
	// There is no "latest", and a downgrade is refused.
	Version string
	// APIServerAddress is the address clients use to reach the Kubernetes API.
	// It is always the first tls-san entry in the rendered config.
	APIServerAddress string
	// StartOnBoot enables the service at boot when true and disables it when
	// false.
	StartOnBoot bool
	// ExtraConfig holds extra keys for config.yaml. A key here replaces the
	// provider default of the same name, except tls-san: its entries (a
	// string or a list of strings) are appended after APIServerAddress.
	ExtraConfig map[string]any
}

// State is what Inspect found on the host.
type State struct {
	InstalledVersion string // "" when RKE2 is not installed
	Config           []byte // nil when ConfigPath is absent
	ServiceActive    bool
	ServiceEnabled   bool
}

// Plan is the set of actions that bring a host to the desired state.
type Plan struct {
	WriteConfig bool   // write Config to ConfigPath with mode 0600
	Config      []byte // rendered config; set whenever WriteConfig is true
	Install     bool   // run the installer: fresh install or upgrade
	Version     string // release tag to install; set whenever Install is true
	Enable      bool   // enable ServiceName at boot
	Disable     bool   // disable ServiceName at boot
	Start       bool   // start ServiceName, which is not running
	Restart     bool   // restart the running ServiceName to pick up a new binary or config
}

// Empty reports whether the plan does nothing.
func (p Plan) Empty() bool {
	return !p.WriteConfig && !p.Install && !p.Enable && !p.Disable && !p.Start && !p.Restart
}

// Describe returns one human-readable line per action, in Apply order.
func (p Plan) Describe() []string {
	var lines []string
	if p.WriteConfig {
		lines = append(lines, "write "+ConfigPath)
	}
	if p.Install {
		lines = append(lines, "install RKE2 "+p.Version)
	}
	if p.Enable {
		lines = append(lines, "enable "+ServiceName+" at boot")
	}
	if p.Disable {
		lines = append(lines, "disable "+ServiceName+" at boot")
	}
	if p.Restart {
		lines = append(lines, "restart "+ServiceName)
	}
	if p.Start {
		lines = append(lines, "start "+ServiceName)
	}
	return lines
}

// Installer puts the RKE2 binaries on the host.
type Installer interface {
	Install(ctx context.Context, r Runner, version string) error
}

// OnlineInstaller runs the upstream install script, which needs internet access.
type OnlineInstaller struct{}

// Install runs https://get.rke2.io pinned to version. The script is downloaded
// to a file first: piped into `sh -`, a failed or empty download would exit 0.
func (OnlineInstaller) Install(ctx context.Context, r Runner, version string) error {
	script := `tmp=$(mktemp) && curl -sfL -o "$tmp" https://get.rke2.io && INSTALL_RKE2_VERSION=` +
		shellQuote(version) + ` sh "$tmp"; rc=$?; rm -f "$tmp"; exit $rc`
	if _, err := r.Run(ctx, script); err != nil {
		return fmt.Errorf("run the install script from https://get.rke2.io: %w", err)
	}
	return nil
}

// Check verifies the host can run RKE2 and that commands run as root without
// a password prompt.
func Check(ctx context.Context, r Runner) error {
	// The first command doubles as the privilege probe: over SSH as a non-root
	// user every script runs through `sudo -n`, which fails rather than prompts.
	uid, err := r.Run(ctx, "id -u")
	if err != nil {
		var exit *ExitError
		if errors.As(err, &exit) {
			return fmt.Errorf("commands must run as root, either as root or through passwordless sudo (sudo -n): %w", err)
		}
		return fmt.Errorf("check privileges: %w", err)
	}
	if got := strings.TrimSpace(uid); got != "0" {
		return fmt.Errorf("commands must run as root, got uid %q: connect as root or enable passwordless sudo", got)
	}

	osName, err := r.Run(ctx, "uname -s")
	if err != nil {
		return fmt.Errorf("detect operating system: %w", err)
	}
	if got := strings.TrimSpace(osName); got != "Linux" {
		return fmt.Errorf("RKE2 needs a Linux host, found %q", got)
	}

	arch, err := r.Run(ctx, "uname -m")
	if err != nil {
		return fmt.Errorf("detect architecture: %w", err)
	}
	if got := strings.TrimSpace(arch); got != "x86_64" && got != "aarch64" {
		return fmt.Errorf("unsupported architecture %q: RKE2 supports x86_64 and aarch64", got)
	}

	sd, err := r.Run(ctx, "[ -d /run/systemd/system ] && echo systemd || true")
	if err != nil {
		return fmt.Errorf("detect systemd: %w", err)
	}
	if strings.TrimSpace(sd) != "systemd" {
		return errors.New("systemd is not running on the host; RKE2 is installed as a systemd service")
	}
	return nil
}

// installPrefixes are where RKE2 can be installed: the tarball default, the
// RPM layout, and the tarball fallback get.rke2.io uses when /usr/local is a
// mount point or read-only. Every path the package probes is derived from
// this list, so the binary and uninstall-script lookups cannot drift apart.
var installPrefixes = []string{"/usr/local", "/usr", "/opt/rke2"}

// installedPaths returns <prefix>/bin/<name> for every install prefix.
func installedPaths(name string) []string {
	paths := make([]string, len(installPrefixes))
	for i, p := range installPrefixes {
		paths[i] = p + "/bin/" + name
	}
	return paths
}

// binaryPaths are where the RKE2 install script puts the binary. The probe
// uses absolute paths because sudo's secure_path may not include them.
var binaryPaths = installedPaths("rke2")

// versionProbe prints the version of the first binary found, and nothing (exit
// 0) when there is none. A binary whose --version fails makes the script fail.
var versionProbe = "for b in " + strings.Join(binaryPaths, " ") +
	`; do [ -x "$b" ] && exec "$b" --version; done; true`

var versionOutput = regexp.MustCompile(`^rke2 version (\S+)`)

// installedVersion returns the version the installed rke2 binary reports, or
// "" when no binary is found.
func installedVersion(ctx context.Context, r Runner) (string, error) {
	out, err := r.Run(ctx, versionProbe)
	if err != nil {
		return "", fmt.Errorf("read RKE2 version: %w", err)
	}
	if out = strings.TrimSpace(out); out == "" {
		return "", nil
	}
	m := versionOutput.FindStringSubmatch(strings.SplitN(out, "\n", 2)[0])
	if m == nil {
		return "", fmt.Errorf("unrecognised `rke2 --version` output %q", out)
	}
	return m[1], nil
}

// Inspect reads the current RKE2 state from the host. Absent things are
// reported as zero values, not errors.
func Inspect(ctx context.Context, r Runner) (State, error) {
	var s State

	v, err := installedVersion(ctx, r)
	if err != nil {
		return State{}, err
	}
	s.InstalledVersion = v

	active, err := r.Run(ctx, "systemctl is-active "+ServiceName+" 2>/dev/null || true")
	if err != nil {
		return State{}, fmt.Errorf("read service state: %w", err)
	}
	// RKE2 is a notify service that stays "activating" until it is up, so a
	// service starting or reloading counts as running.
	switch strings.TrimSpace(active) {
	case "active", "activating", "reloading":
		s.ServiceActive = true
	}

	enabled, err := r.Run(ctx, "systemctl is-enabled "+ServiceName+" 2>/dev/null || true")
	if err != nil {
		return State{}, fmt.Errorf("read service boot setting: %w", err)
	}
	s.ServiceEnabled = strings.TrimSpace(enabled) == "enabled"

	cfg, err := r.ReadFile(ctx, ConfigPath)
	switch {
	case err == nil:
		s.Config = cfg
	case errors.Is(err, fs.ErrNotExist):
	default:
		return State{}, fmt.Errorf("read RKE2 config: %w", err)
	}
	return s, nil
}

// PlanInstall computes what must change to bring state to o. It is pure and
// returns an empty Plan when nothing differs.
func PlanInstall(s State, o Options) (Plan, error) {
	want, err := ParseVersion(o.Version)
	if err != nil {
		return Plan{}, err
	}
	cfg, err := RenderConfig(o.APIServerAddress, o.ExtraConfig)
	if err != nil {
		return Plan{}, err
	}

	var p Plan
	installed := s.InstalledVersion != ""
	if !installed && (s.ServiceActive || s.ServiceEnabled) {
		return Plan{}, fmt.Errorf("%s exists but the rke2 binary was not found at %s; refusing to install over it",
			ServiceName, strings.Join(binaryPaths, ", "))
	}
	if installed {
		have, err := ParseVersion(s.InstalledVersion)
		if err != nil {
			return Plan{}, fmt.Errorf("installed RKE2: %w", err)
		}
		switch c := have.Compare(want); {
		case c > 0:
			return Plan{}, fmt.Errorf("refusing to downgrade RKE2 from %s to %s", have, want)
		case c < 0:
			p.Install = true
		}
	} else {
		p.Install = true
	}
	if p.Install {
		p.Version = want.String()
	}

	if !bytes.Equal(s.Config, cfg) {
		p.WriteConfig = true
		p.Config = cfg
	}

	p.Enable = o.StartOnBoot && !s.ServiceEnabled
	p.Disable = !o.StartOnBoot && s.ServiceEnabled

	// A running service must be restarted to pick up a new binary or config;
	// a stopped one, or a fresh install, only needs starting.
	switch {
	case !s.ServiceActive:
		p.Start = true
	case p.Install || p.WriteConfig:
		p.Restart = true
	}
	return p, nil
}

// Apply carries out p: write config, install, enable or disable, then
// restart or start. An empty plan makes no calls. A plan that installs an
// empty or invalid Version is rejected before any call: the install script
// would read an empty version as "latest".
func Apply(ctx context.Context, r Runner, inst Installer, p Plan) error {
	if p.Install {
		if _, err := ParseVersion(p.Version); err != nil {
			return fmt.Errorf("plan installs RKE2: %w", err)
		}
	}
	if p.WriteConfig {
		if err := r.WriteFile(ctx, ConfigPath, p.Config, 0o600); err != nil {
			return fmt.Errorf("write RKE2 config: %w", err)
		}
	}
	if p.Install {
		if inst == nil {
			return errors.New("plan requires an install but no installer was provided")
		}
		if err := inst.Install(ctx, r, p.Version); err != nil {
			return fmt.Errorf("install RKE2 %s: %w", p.Version, err)
		}
		// Trust the binary, not the installer's exit status, before touching
		// the service: restarting an old binary would hide a failed upgrade.
		if err := verifyInstalled(ctx, r, p.Version); err != nil {
			return fmt.Errorf("install RKE2 %s: %w", p.Version, err)
		}
	}
	if p.Enable {
		if err := systemctl(ctx, r, "enable"); err != nil {
			return err
		}
	}
	if p.Disable {
		if err := systemctl(ctx, r, "disable"); err != nil {
			return err
		}
	}
	if p.Restart {
		if err := systemctl(ctx, r, "restart"); err != nil {
			return err
		}
	}
	if p.Start {
		if err := systemctl(ctx, r, "start"); err != nil {
			return err
		}
	}
	return nil
}

// verifyInstalled returns an error unless the rke2 binary on the host reports
// version.
func verifyInstalled(ctx context.Context, r Runner, version string) error {
	got, err := installedVersion(ctx, r)
	if err != nil {
		return err
	}
	if got == "" {
		return fmt.Errorf("rke2 binary not found at %s after the installer ran", strings.Join(binaryPaths, ", "))
	}
	if got != version {
		return fmt.Errorf("the host reports RKE2 %s after the installer ran", got)
	}
	return nil
}

func systemctl(ctx context.Context, r Runner, action string) error {
	if _, err := r.Run(ctx, "systemctl "+action+" "+ServiceName); err != nil {
		return fmt.Errorf("systemctl %s %s: %w", action, ServiceName, err)
	}
	return nil
}
