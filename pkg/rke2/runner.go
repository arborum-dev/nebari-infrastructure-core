// Package rke2 installs and removes RKE2 on a Linux host over SSH.
package rke2

import (
	"context"
	"fmt"
	"io/fs"
	"strings"
)

const (
	// ConfigPath is where RKE2 reads its server configuration.
	ConfigPath = "/etc/rancher/rke2/config.yaml"
	// KubeconfigPath is where RKE2 writes the cluster admin kubeconfig.
	KubeconfigPath = "/etc/rancher/rke2/rke2.yaml"
	// ServiceName is the systemd unit that runs the RKE2 server.
	ServiceName = "rke2-server.service"
)

// Runner executes work as root on the target host. Errors from ReadFile and
// WriteFile name the path, so callers need not repeat it.
type Runner interface {
	// Run executes script with `sh -c`. A non-zero exit returns *ExitError.
	Run(ctx context.Context, script string) (stdout string, err error)
	// ReadFile returns an error satisfying errors.Is(err, fs.ErrNotExist) when path is absent.
	ReadFile(ctx context.Context, path string) ([]byte, error)
	// WriteFile creates parent directories and replaces path atomically with the given mode.
	WriteFile(ctx context.Context, path string, data []byte, mode fs.FileMode) error
	// Close releases the connection to the host. The Runner must not be used
	// afterwards.
	Close() error
}

// ExitError reports a remote command that exited with a non-zero status.
type ExitError struct {
	Code   int
	Stderr string
}

func (e *ExitError) Error() string {
	stderr := strings.TrimSpace(e.Stderr)
	if stderr == "" {
		return fmt.Sprintf("command exited with status %d", e.Code)
	}
	return fmt.Sprintf("command exited with status %d: %s", e.Code, stderr)
}
