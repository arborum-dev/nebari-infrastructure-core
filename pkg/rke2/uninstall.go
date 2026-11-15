package rke2

import (
	"context"
	"fmt"
	"path"
	"strings"
)

// uninstallScripts are the places RKE2 installs its uninstall script: the
// tarball layout first, then the RPM layout.
var uninstallScripts = []string{
	"/usr/local/bin/rke2-uninstall.sh",
	"/usr/bin/rke2-uninstall.sh",
}

// minDataDirDepth is the fewest path segments a data directory may have, so
// that "/" or "/var" can never be removed.
const minDataDirDepth = 3

// Uninstall runs RKE2's uninstall script when present, then removes each of
// dataDirs. A host with nothing to remove is a success.
func Uninstall(ctx context.Context, r Runner, dataDirs []string) error {
	for _, dir := range dataDirs {
		if err := validateDataDir(dir); err != nil {
			return err
		}
	}

	script, err := findUninstallScript(ctx, r)
	if err != nil {
		return err
	}
	if script != "" {
		if _, err := r.Run(ctx, singleQuote(script)); err != nil {
			return fmt.Errorf("run %s: %w", script, err)
		}
	}

	for _, dir := range dataDirs {
		if _, err := r.Run(ctx, "rm -rf -- "+singleQuote(dir)); err != nil {
			return fmt.Errorf("remove %s: %w", dir, err)
		}
	}
	return nil
}

// findUninstallScript returns the first uninstall script present on the host,
// or "" when none is. The probe prints the path and exits 0 either way.
func findUninstallScript(ctx context.Context, r Runner) (string, error) {
	probe := "for p in " + strings.Join(uninstallScripts, " ") +
		`; do if [ -x "$p" ]; then echo "$p"; break; fi; done`
	out, err := r.Run(ctx, probe)
	if err != nil {
		return "", fmt.Errorf("look for the uninstall script: %w", err)
	}
	found := strings.TrimSpace(out)
	for _, s := range uninstallScripts {
		if found == s {
			return s, nil
		}
	}
	return "", nil
}

// validateDataDir accepts only clean absolute paths at least minDataDirDepth
// segments deep.
func validateDataDir(dir string) error {
	if !path.IsAbs(dir) || dir != path.Clean(dir) {
		return fmt.Errorf("unsafe data directory %q: must be a clean absolute path", dir)
	}
	if len(strings.Split(strings.TrimPrefix(dir, "/"), "/")) < minDataDirDepth {
		return fmt.Errorf("unsafe data directory %q: must be at least %d segments deep", dir, minDataDirDepth)
	}
	return nil
}

// singleQuote wraps s in POSIX single quotes.
func singleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
