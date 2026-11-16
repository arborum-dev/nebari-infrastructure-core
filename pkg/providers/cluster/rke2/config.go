// Package rke2 is the NIC cluster provider for a single host that runs RKE2.
//
// The host package that installs RKE2 over SSH is also named rke2. This file
// imports it as rke2host; code outside this package should import this package
// as clusterrke2.
package rke2

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"

	rke2host "github.com/nebari-dev/nebari-infrastructure-core/pkg/rke2"
	"github.com/nebari-dev/nebari-infrastructure-core/pkg/storage/longhorn"
)

const (
	defaultSSHUser = "root"
	defaultSSHPort = 22
)

// Config is the cluster.rke2 block: one existing host, reached over SSH, on
// which NIC installs and manages RKE2.
type Config struct {
	// Host is the address NIC opens the SSH connection to, such as an IP
	// address or a hostname. Required.
	Host string `yaml:"host"`

	// SSH describes how to log in to Host. Required.
	SSH SSHConfig `yaml:"ssh"`

	// Settings are the RKE2 options. They are written directly under
	// cluster.rke2, not under a key of their own.
	Settings `yaml:",inline"`

	// StorageClass is the default Kubernetes StorageClass for persistent
	// volumes. Required when Longhorn is disabled, and names a StorageClass
	// you provide some other way. Ignored while Longhorn is enabled, which
	// uses the "longhorn" class.
	StorageClass string `yaml:"storage_class,omitempty"`

	// Longhorn configures NIC's shared Longhorn storage block. Longhorn is
	// enabled when this block is omitted and is disabled only by
	// "longhorn: {enabled: false}". When replica_count is unset the provider
	// uses 1, since there is a single node.
	Longhorn *longhorn.Config `yaml:"longhorn,omitempty"`
}

// SSHConfig describes the SSH login to the host. NIC always verifies the host
// key against known_hosts; there is no way to skip that check.
type SSHConfig struct {
	// User is the login user: root, or a user with passwordless sudo.
	// Password logins and sudo password prompts are not supported.
	// Defaults to "root".
	User string `yaml:"user,omitempty"`

	// Port is the SSH port. Defaults to 22.
	Port int `yaml:"port,omitempty"`

	// PrivateKeyPath is the path to the private key used to log in. The key
	// must not need a passphrase. The path is used as written; "~" is not
	// expanded. Required.
	PrivateKeyPath string `yaml:"private_key_path"`

	// KnownHostsPath is the known_hosts file that must already hold the
	// host's key. The path is used as written; "~" is not expanded.
	// Defaults to $HOME/.ssh/known_hosts.
	KnownHostsPath string `yaml:"known_hosts_path,omitempty"`

	// HostKeyAlias is a name to look up in known_hosts instead of the host
	// and port. Use it when the host is reached through a forwarded port
	// and known_hosts lists it under another name. Optional.
	HostKeyAlias string `yaml:"host_key_alias,omitempty"`
}

// Settings holds the RKE2 options that a provider creating its own host can
// reuse. In cluster.rke2 these keys sit directly under the block, not under a
// settings: key; a provider that creates hosts can expose them as a named
// sub-block.
type Settings struct {
	// Version is the exact RKE2 release tag to run, such as v1.37.2+rke2r1.
	// There is no "latest". Required.
	Version string `yaml:"version"`

	// APIServerAddress is the address the returned kubeconfig points at. It
	// is also added to RKE2's tls-san. Set it when it differs from the SSH
	// host, for example when SSH uses a forwarded port. Defaults to the host.
	APIServerAddress string `yaml:"api_server_address,omitempty"`

	// StartOnBoot enables the RKE2 service at boot when true and disables it
	// when false. Defaults to true.
	StartOnBoot *bool `yaml:"start_on_boot,omitempty"`

	// Config holds extra keys written into RKE2's own config file
	// (/etc/rancher/rke2/config.yaml), such as a CIS profile, the CNI or
	// kubelet arguments. A key here replaces the provider's default of the
	// same name, except tls-san, which always also contains
	// api_server_address.
	Config map[string]any `yaml:"config,omitempty"`
}

// Validate checks the config without touching files or the network.
func (c *Config) Validate() error {
	if c.Host == "" {
		return fmt.Errorf("cluster.rke2.host is required")
	}
	if err := checkAddress("cluster.rke2.host", c.Host); err != nil {
		return err
	}
	if c.APIServerAddress != "" {
		if err := checkAddress("cluster.rke2.api_server_address", c.APIServerAddress); err != nil {
			return err
		}
	}
	if c.Version == "" {
		return fmt.Errorf("cluster.rke2.version is required")
	}
	if _, err := rke2host.ParseVersion(c.Version); err != nil {
		return fmt.Errorf("cluster.rke2.version %q is not an RKE2 release tag (expected a form like v1.37.2+rke2r1)", c.Version)
	}
	if c.SSH.PrivateKeyPath == "" {
		return fmt.Errorf("cluster.rke2.ssh.private_key_path is required")
	}
	if c.SSH.Port != 0 && (c.SSH.Port < 1 || c.SSH.Port > 65535) {
		return fmt.Errorf("cluster.rke2.ssh.port %d is out of range", c.SSH.Port)
	}
	if !c.LonghornEnabled() && c.StorageClass == "" {
		return fmt.Errorf("cluster.rke2.storage_class is required when longhorn is disabled")
	}
	if _, err := rke2host.RenderConfig(c.apiServerAddress(), c.Config); err != nil {
		return fmt.Errorf("cluster.rke2.config: %w", err)
	}
	return nil
}

// checkAddress rejects a value that is not a bare hostname or IP address,
// such as a URL or host:port. A colon is accepted only in an IPv6 literal.
func checkAddress(field, value string) error {
	if strings.Contains(value, "/") || (strings.Contains(value, ":") && net.ParseIP(value) == nil) {
		return fmt.Errorf("%s %q must be a hostname or IP address, without scheme, port or path", field, value)
	}
	return nil
}

// LonghornEnabled reports whether Longhorn is installed. It is true when the
// longhorn block is omitted.
func (c *Config) LonghornEnabled() bool {
	return c.Longhorn == nil || c.Longhorn.IsEnabled()
}

// LonghornConfig returns a copy of the Longhorn config with the single-node
// default applied: ReplicaCount is 1 when unset. It never modifies c.
func (c *Config) LonghornConfig() *longhorn.Config {
	if c.Longhorn == nil {
		return &longhorn.Config{ReplicaCount: 1}
	}
	lc := *c.Longhorn
	if lc.ReplicaCount == 0 {
		lc.ReplicaCount = 1
	}
	return &lc
}

// GetStorageClass returns "longhorn" when Longhorn is enabled, and the
// configured StorageClass otherwise.
func (c *Config) GetStorageClass() string {
	if c.LonghornEnabled() {
		return longhorn.StorageClassName
	}
	return c.StorageClass
}

// SSHOptions returns the SSH connection options with defaults applied. It
// fails only when the default known_hosts path is needed and $HOME cannot be
// resolved.
func (c *Config) SSHOptions() (rke2host.SSHOptions, error) {
	opts := rke2host.SSHOptions{
		Host:           c.Host,
		Port:           c.SSH.Port,
		User:           c.SSH.User,
		PrivateKeyPath: c.SSH.PrivateKeyPath,
		KnownHostsPath: c.SSH.KnownHostsPath,
		HostKeyAlias:   c.SSH.HostKeyAlias,
	}
	if opts.Port == 0 {
		opts.Port = defaultSSHPort
	}
	if opts.User == "" {
		opts.User = defaultSSHUser
	}
	if opts.KnownHostsPath == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return rke2host.SSHOptions{}, fmt.Errorf("cluster.rke2.ssh.known_hosts_path is unset and the home directory is unknown: %w", err)
		}
		opts.KnownHostsPath = filepath.Join(home, ".ssh", "known_hosts")
	}
	return opts, nil
}

// RKE2Options returns the host package's install options with defaults
// applied: APIServerAddress is the host and StartOnBoot is true.
func (c *Config) RKE2Options() rke2host.Options {
	return rke2host.Options{
		Version:          c.Version,
		APIServerAddress: c.apiServerAddress(),
		StartOnBoot:      c.StartOnBoot == nil || *c.StartOnBoot,
		ExtraConfig:      c.Config,
	}
}

func (c *Config) apiServerAddress() string {
	if c.APIServerAddress != "" {
		return c.APIServerAddress
	}
	return c.Host
}
