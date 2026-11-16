package rke2

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/goccy/go-yaml"

	"github.com/nebari-dev/nebari-infrastructure-core/pkg/config"
	"github.com/nebari-dev/nebari-infrastructure-core/pkg/storage/longhorn"
)

func boolPtr(b bool) *bool { return &b }

func validConfig() *Config {
	return &Config{
		Host:     "203.0.113.10",
		SSH:      SSHConfig{PrivateKeyPath: "/path/to/key"},
		Settings: Settings{Version: "v1.37.2+rke2r1"},
	}
}

func TestConfigValidate(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(c *Config)
		wantErr string // empty means no error; otherwise an exact message or substring
		exact   bool
	}{
		{name: "valid minimal config", mutate: func(c *Config) {}},
		{name: "empty host", mutate: func(c *Config) { c.Host = "" }, wantErr: "cluster.rke2.host is required", exact: true},
		{name: "empty version", mutate: func(c *Config) { c.Version = "" }, wantErr: "cluster.rke2.version is required", exact: true},
		{
			name:    "version latest",
			mutate:  func(c *Config) { c.Version = "latest" },
			wantErr: `cluster.rke2.version "latest" is not an RKE2 release tag (expected a form like v1.37.2+rke2r1)`,
			exact:   true,
		},
		{
			name:    "empty private key path",
			mutate:  func(c *Config) { c.SSH.PrivateKeyPath = "" },
			wantErr: "cluster.rke2.ssh.private_key_path is required",
			exact:   true,
		},
		{name: "port too large", mutate: func(c *Config) { c.SSH.Port = 70000 }, wantErr: "cluster.rke2.ssh.port 70000 is out of range", exact: true},
		{name: "port negative", mutate: func(c *Config) { c.SSH.Port = -1 }, wantErr: "cluster.rke2.ssh.port -1 is out of range", exact: true},
		{name: "port max is valid", mutate: func(c *Config) { c.SSH.Port = 65535 }},
		{
			name:    "longhorn disabled without storage class",
			mutate:  func(c *Config) { c.Longhorn = &longhorn.Config{Enabled: boolPtr(false)} },
			wantErr: "cluster.rke2.storage_class is required when longhorn is disabled",
			exact:   true,
		},
		{
			name: "longhorn disabled with storage class",
			mutate: func(c *Config) {
				c.Longhorn = &longhorn.Config{Enabled: boolPtr(false)}
				c.StorageClass = "local-path"
			},
		},
		{name: "invalid rke2 config value", mutate: func(c *Config) { c.Config = map[string]any{"tls-san": 5} }, wantErr: "cluster.rke2.config"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := validConfig()
			tt.mutate(c)
			err := c.Validate()
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate() unexpected error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("Validate() = nil, want error %q", tt.wantErr)
			}
			if tt.exact && err.Error() != tt.wantErr {
				t.Errorf("Validate() error = %q, want %q", err.Error(), tt.wantErr)
			}
			if !tt.exact && !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("Validate() error = %q, want it to contain %q", err.Error(), tt.wantErr)
			}
		})
	}
}

func TestConfigUnmarshal(t *testing.T) {
	const doc = `
host: 203.0.113.10
version: v1.37.2+rke2r1
ssh:
  user: root
  port: 22
  private_key_path: /path/to/key
  known_hosts_path: /path/to/known_hosts
  host_key_alias: ""
api_server_address: "127.0.0.1"
start_on_boot: false
config:
  cni: cilium
storage_class: local-path
longhorn:
  replica_count: 1
`
	var raw map[string]any
	if err := yaml.Unmarshal([]byte(doc), &raw); err != nil {
		t.Fatalf("yaml.Unmarshal: %v", err)
	}
	var got Config
	if err := config.UnmarshalProviderConfig(context.Background(), raw, &got); err != nil {
		t.Fatalf("UnmarshalProviderConfig: %v", err)
	}

	if got.Host != "203.0.113.10" {
		t.Errorf("Host = %q", got.Host)
	}
	if got.Version != "v1.37.2+rke2r1" {
		t.Errorf("Version = %q", got.Version)
	}
	if got.APIServerAddress != "127.0.0.1" {
		t.Errorf("APIServerAddress = %q", got.APIServerAddress)
	}
	if got.StartOnBoot == nil || *got.StartOnBoot {
		t.Errorf("StartOnBoot = %v, want pointer to false", got.StartOnBoot)
	}
	if !reflect.DeepEqual(got.Config, map[string]any{"cni": "cilium"}) {
		t.Errorf("Config = %#v", got.Config)
	}
	wantSSH := SSHConfig{User: "root", Port: 22, PrivateKeyPath: "/path/to/key", KnownHostsPath: "/path/to/known_hosts"}
	if got.SSH != wantSSH {
		t.Errorf("SSH = %+v, want %+v", got.SSH, wantSSH)
	}
	if got.StorageClass != "local-path" {
		t.Errorf("StorageClass = %q", got.StorageClass)
	}
	if got.Longhorn == nil || got.Longhorn.ReplicaCount != 1 {
		t.Errorf("Longhorn = %+v", got.Longhorn)
	}
}

func TestConfigDefaults(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("no home directory: %v", err)
	}

	t.Run("ssh options", func(t *testing.T) {
		got, err := validConfig().SSHOptions()
		if err != nil {
			t.Fatalf("SSHOptions() error: %v", err)
		}
		if got.Host != "203.0.113.10" || got.User != "root" || got.Port != 22 ||
			got.PrivateKeyPath != "/path/to/key" ||
			got.KnownHostsPath != filepath.Join(home, ".ssh", "known_hosts") {
			t.Errorf("SSHOptions() = %+v", got)
		}
	})

	t.Run("ssh options keep explicit values", func(t *testing.T) {
		c := validConfig()
		c.SSH = SSHConfig{User: "ubuntu", Port: 2222, PrivateKeyPath: "~/k", KnownHostsPath: "~/kh", HostKeyAlias: "lima-colima-rke2"}
		got, err := c.SSHOptions()
		if err != nil {
			t.Fatalf("SSHOptions() error: %v", err)
		}
		if got.User != "ubuntu" || got.Port != 2222 || got.PrivateKeyPath != "~/k" ||
			got.KnownHostsPath != "~/kh" || got.HostKeyAlias != "lima-colima-rke2" {
			t.Errorf("SSHOptions() = %+v", got)
		}
	})

	tests := []struct {
		name        string
		mutate      func(c *Config)
		wantAddress string
		wantStart   bool
	}{
		{name: "defaults", mutate: func(c *Config) {}, wantAddress: "203.0.113.10", wantStart: true},
		{name: "explicit address", mutate: func(c *Config) { c.APIServerAddress = "127.0.0.1" }, wantAddress: "127.0.0.1", wantStart: true},
		{name: "start on boot false", mutate: func(c *Config) { c.StartOnBoot = boolPtr(false) }, wantAddress: "203.0.113.10", wantStart: false},
		{name: "start on boot true", mutate: func(c *Config) { c.StartOnBoot = boolPtr(true) }, wantAddress: "203.0.113.10", wantStart: true},
	}
	for _, tt := range tests {
		t.Run("rke2 options "+tt.name, func(t *testing.T) {
			c := validConfig()
			c.Config = map[string]any{"cni": "cilium"}
			tt.mutate(c)
			got := c.RKE2Options()
			if got.Version != "v1.37.2+rke2r1" || got.APIServerAddress != tt.wantAddress ||
				got.StartOnBoot != tt.wantStart || !reflect.DeepEqual(got.ExtraConfig, c.Config) {
				t.Errorf("RKE2Options() = %+v", got)
			}
		})
	}
}

func TestLonghornConfig(t *testing.T) {
	tests := []struct {
		name         string
		longhorn     *longhorn.Config
		storageClass string
		wantEnabled  bool
		wantReplicas int
		wantClass    string
	}{
		{name: "nil block", wantEnabled: true, wantReplicas: 1, wantClass: "longhorn"},
		{name: "empty block", longhorn: &longhorn.Config{}, wantEnabled: true, wantReplicas: 1, wantClass: "longhorn"},
		{name: "replica count kept", longhorn: &longhorn.Config{ReplicaCount: 3}, wantEnabled: true, wantReplicas: 3, wantClass: "longhorn"},
		{
			name:         "disabled",
			longhorn:     &longhorn.Config{Enabled: boolPtr(false)},
			storageClass: "local-path",
			wantEnabled:  false,
			wantReplicas: 1,
			wantClass:    "local-path",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := validConfig()
			c.Longhorn = tt.longhorn
			c.StorageClass = tt.storageClass

			var before longhorn.Config
			if tt.longhorn != nil {
				before = *tt.longhorn
			}

			if got := c.LonghornEnabled(); got != tt.wantEnabled {
				t.Errorf("LonghornEnabled() = %v, want %v", got, tt.wantEnabled)
			}
			lc := c.LonghornConfig()
			if lc == nil || lc.ReplicaCount != tt.wantReplicas {
				t.Errorf("LonghornConfig() = %+v, want ReplicaCount %d", lc, tt.wantReplicas)
			}
			if got := c.GetStorageClass(); got != tt.wantClass {
				t.Errorf("GetStorageClass() = %q, want %q", got, tt.wantClass)
			}

			// The returned copy must not alias or mutate the original.
			if tt.longhorn != nil {
				if lc == tt.longhorn {
					t.Error("LonghornConfig() returned the original pointer, want a copy")
				}
				if !reflect.DeepEqual(*tt.longhorn, before) {
					t.Errorf("original Longhorn mutated: got %+v, want %+v", *tt.longhorn, before)
				}
			} else if c.Longhorn != nil {
				t.Error("LonghornConfig() set c.Longhorn")
			}
		})
	}
}

// TestConfigUnmarshal_SpecExample parses the cluster block from the design's
// "Config block" section verbatim, including the empty forms.
func TestConfigUnmarshal_SpecExample(t *testing.T) {
	const doc = `
cluster:
  rke2:
    host: 203.0.113.10 # address to open the SSH connection to
    version: v1.37.2+rke2r1 # required; an exact RKE2 release
    ssh:
      user: root # default root
      port: 22 # default 22
      private_key_path: /path/to/key # required
      known_hosts_path: /path/to/known_hosts # default ~/.ssh/known_hosts
      host_key_alias: "" # optional; name to look up in known_hosts
    api_server_address: "" # default: host
    start_on_boot: true # default true
    config: {} # extra keys for /etc/rancher/rke2/config.yaml
    longhorn: # NIC's existing longhorn block
      replica_count: 1
`
	var top struct {
		Cluster map[string]any `yaml:"cluster"`
	}
	if err := yaml.Unmarshal([]byte(doc), &top); err != nil {
		t.Fatalf("yaml.Unmarshal: %v", err)
	}
	var got Config
	if err := config.UnmarshalProviderConfig(context.Background(), top.Cluster["rke2"], &got); err != nil {
		t.Fatalf("UnmarshalProviderConfig: %v", err)
	}

	if got.Host != "203.0.113.10" || got.Version != "v1.37.2+rke2r1" {
		t.Errorf("Host/Version = %q/%q", got.Host, got.Version)
	}
	wantSSH := SSHConfig{User: "root", Port: 22, PrivateKeyPath: "/path/to/key", KnownHostsPath: "/path/to/known_hosts"}
	if got.SSH != wantSSH {
		t.Errorf("SSH = %+v, want %+v", got.SSH, wantSSH)
	}
	if got.APIServerAddress != "" {
		t.Errorf("APIServerAddress = %q, want empty", got.APIServerAddress)
	}
	if len(got.Config) != 0 {
		t.Errorf("Config = %#v, want empty", got.Config)
	}
	if err := got.Validate(); err != nil {
		t.Errorf("Validate() = %v, want nil", err)
	}
	opts := got.RKE2Options()
	if opts.APIServerAddress != "203.0.113.10" {
		t.Errorf("RKE2Options().APIServerAddress = %q, want host", opts.APIServerAddress)
	}
	if !opts.StartOnBoot {
		t.Error("RKE2Options().StartOnBoot = false, want true")
	}
	if got.StartOnBoot == nil || !*got.StartOnBoot {
		t.Errorf("StartOnBoot = %v, want pointer to true", got.StartOnBoot)
	}
	if rc := got.LonghornConfig().ReplicaCount; rc != 1 {
		t.Errorf("LonghornConfig().ReplicaCount = %d, want 1", rc)
	}
}
