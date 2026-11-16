package rke2

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"k8s.io/client-go/tools/clientcmd"

	"github.com/nebari-dev/nebari-infrastructure-core/pkg/config"
	"github.com/nebari-dev/nebari-infrastructure-core/pkg/providers/cluster"
	rke2host "github.com/nebari-dev/nebari-infrastructure-core/pkg/rke2"
	"github.com/nebari-dev/nebari-infrastructure-core/pkg/rke2/rke2test"
	"github.com/nebari-dev/nebari-infrastructure-core/pkg/status"
	"github.com/nebari-dev/nebari-infrastructure-core/pkg/storage/longhorn"
)

const (
	testProject   = "demo"
	testHost      = "192.0.2.10"
	testAPIServer = "198.51.100.7"
	testVersion   = "v1.37.2+rke2r1"
	newerVersion  = "v1.37.3+rke2r1"
)

// testKubeconfig stands in for /etc/rancher/rke2/rke2.yaml, which points at
// the loopback address as RKE2 writes it.
const testKubeconfig = `apiVersion: v1
kind: Config
clusters:
- name: default
  cluster:
    server: https://127.0.0.1:6443
contexts:
- name: default
  context:
    cluster: default
    user: default
current-context: default
users:
- name: default
  user:
    token: fake
`

// rke2Block returns a valid cluster.rke2 block after applying mutate. Deploy
// never reads the key files, so they need not exist.
func rke2Block(mutate ...func(map[string]any)) map[string]any {
	block := map[string]any{
		"host":               testHost,
		"version":            testVersion,
		"api_server_address": testAPIServer,
		"ssh": map[string]any{
			"private_key_path": "/nonexistent/id_ed25519",
			"known_hosts_path": "/nonexistent/known_hosts",
		},
	}
	for _, m := range mutate {
		m(block)
	}
	return block
}

func clusterConfig(block map[string]any) *config.ClusterConfig {
	return &config.ClusterConfig{Providers: map[string]any{ProviderName: block}}
}

// renderedConfig is the RKE2 config Deploy writes for rke2Block().
func renderedConfig(t *testing.T) []byte {
	t.Helper()
	cfg, err := rke2host.RenderConfig(testAPIServer, nil)
	if err != nil {
		t.Fatalf("RenderConfig: %v", err)
	}
	return cfg
}

// closeCountingHost is a fake host that records Close calls.
type closeCountingHost struct {
	*rke2test.FakeRunner
	closed int
}

func (h *closeCountingHost) Close() error {
	h.closed++
	return nil
}

// fakes records what Deploy and GetKubeconfig did through a Provider's
// function fields.
type fakes struct {
	host      *closeCountingHost
	dials     int
	installs  []string
	waits     int
	waitErr   error
	waitedOn  []byte
	longhorns []*longhorn.Config
}

func newFakes(host *rke2test.FakeRunner) *fakes {
	if host.Files == nil {
		host.Files = map[string][]byte{}
	}
	return &fakes{host: &closeCountingHost{FakeRunner: host}}
}

// Install makes the host report version, as a real install would, and puts
// the kubeconfig in place, as RKE2's first start would.
func (f *fakes) Install(_ context.Context, _ rke2host.Runner, version string) error {
	f.installs = append(f.installs, version)
	f.host.SimulateInstalled(version, false, false, nil)
	f.host.Files[rke2host.KubeconfigPath] = []byte(testKubeconfig)
	return nil
}

func (f *fakes) provider() *Provider {
	return &Provider{
		dial: func(context.Context, rke2host.SSHOptions) (rke2host.Runner, error) {
			f.dials++
			return f.host, nil
		},
		installer: f,
		waitReady: func(_ context.Context, kubeconfig []byte) error {
			f.waits++
			f.waitedOn = kubeconfig
			return f.waitErr
		},
		apiReachable: func(context.Context, []byte) bool { return false },
		longhornInstall: func(_ context.Context, _ []byte, cfg *longhorn.Config) error {
			f.longhorns = append(f.longhorns, cfg)
			return nil
		},
		longhornUninstall: func(context.Context, []byte) error {
			return errors.New("longhornUninstall must not be called")
		},
	}
}

// ranServiceChange reports any systemctl call that changes the service.
func ranServiceChange(h *rke2test.FakeRunner) []string {
	var got []string
	for _, action := range []string{"start", "restart", "enable", "disable"} {
		if h.Ran("systemctl " + action + " ") {
			got = append(got, action)
		}
	}
	return got
}

// installedHost is a host already running the configured RKE2.
func installedHost(t *testing.T, version string) *rke2test.FakeRunner {
	t.Helper()
	h := rke2test.NewHost("Linux")
	h.SimulateInstalled(version, true, true, renderedConfig(t))
	h.Files[rke2host.KubeconfigPath] = []byte(testKubeconfig)
	return h
}

func serverOf(t *testing.T, kubeconfig []byte) string {
	t.Helper()
	cfg, err := clientcmd.Load(kubeconfig)
	if err != nil {
		t.Fatalf("parse kubeconfig: %v", err)
	}
	for _, c := range cfg.Clusters {
		return c.Server
	}
	t.Fatal("kubeconfig has no cluster")
	return ""
}

func TestProvider_Conformance(t *testing.T) {
	var _ cluster.Provider = (*Provider)(nil)

	p := NewProvider()
	if got := p.Name(); got != "rke2" {
		t.Errorf("Name() = %q, want %q", got, "rke2")
	}
	if p.dial == nil || p.installer == nil || p.waitReady == nil || p.apiReachable == nil ||
		p.longhornInstall == nil || p.longhornUninstall == nil {
		t.Errorf("NewProvider left a dependency unset: %+v", p)
	}
}

func TestDeploy_BareHost(t *testing.T) {
	f := newFakes(rke2test.NewHost("Linux"))
	p := f.provider()

	if err := p.Deploy(context.Background(), testProject, clusterConfig(rke2Block()), cluster.DeployOptions{}); err != nil {
		t.Fatalf("Deploy: %v", err)
	}

	if !reflect.DeepEqual(f.host.Writes, []string{rke2host.ConfigPath}) {
		t.Errorf("Writes = %v, want only %s", f.host.Writes, rke2host.ConfigPath)
	}
	if got := string(f.host.Files[rke2host.ConfigPath]); got != string(renderedConfig(t)) {
		t.Errorf("config written = %q, want %q", got, renderedConfig(t))
	}
	if !reflect.DeepEqual(f.installs, []string{testVersion}) {
		t.Errorf("installer calls = %v, want [%s]", f.installs, testVersion)
	}
	if !f.host.Ran("systemctl start " + rke2host.ServiceName) {
		t.Errorf("systemctl start did not run: %q", f.host.Calls)
	}
	if f.waits != 1 {
		t.Errorf("waitReady called %d times, want 1", f.waits)
	}
	if got, want := serverOf(t, f.waitedOn), "https://"+testAPIServer+":6443"; got != want {
		t.Errorf("waitReady kubeconfig server = %q, want %q", got, want)
	}
	if len(f.longhorns) != 1 {
		t.Fatalf("longhornInstall called %d times, want 1", len(f.longhorns))
	}
	if got := f.longhorns[0].ReplicaCount; got != 1 {
		t.Errorf("longhorn ReplicaCount = %d, want 1", got)
	}

	// Deploy caches the kubeconfig it fetched, so NIC's later call does not
	// open another connection.
	if _, err := p.GetKubeconfig(context.Background(), testProject, clusterConfig(rke2Block())); err != nil {
		t.Fatalf("GetKubeconfig: %v", err)
	}
	if f.dials != 1 {
		t.Errorf("dialled %d times across Deploy and GetKubeconfig, want 1", f.dials)
	}
}

func TestDeploy_NothingChanged(t *testing.T) {
	f := newFakes(installedHost(t, testVersion))

	if err := f.provider().Deploy(context.Background(), testProject, clusterConfig(rke2Block()), cluster.DeployOptions{}); err != nil {
		t.Fatalf("Deploy: %v", err)
	}

	if len(f.host.Writes) != 0 {
		t.Errorf("Writes = %v, want none", f.host.Writes)
	}
	if len(f.installs) != 0 {
		t.Errorf("installer calls = %v, want none", f.installs)
	}
	if got := ranServiceChange(f.host.FakeRunner); len(got) != 0 {
		t.Errorf("ran systemctl %v, want no service change", got)
	}
	if f.waits != 1 {
		t.Errorf("waitReady called %d times, want 1", f.waits)
	}
}

func TestDeploy_DryRun(t *testing.T) {
	f := newFakes(rke2test.NewHost("Linux"))
	updates := make(chan status.Update, 100)
	ctx := status.WithChannel(context.Background(), updates)

	if err := f.provider().Deploy(ctx, testProject, clusterConfig(rke2Block()), cluster.DeployOptions{DryRun: true}); err != nil {
		t.Fatalf("Deploy: %v", err)
	}

	if len(f.host.Writes) != 0 {
		t.Errorf("Writes = %v, want none", f.host.Writes)
	}
	if len(f.installs) != 0 {
		t.Errorf("installer calls = %v, want none", f.installs)
	}
	if got := ranServiceChange(f.host.FakeRunner); len(got) != 0 {
		t.Errorf("ran systemctl %v, want no service change", got)
	}
	if f.waits != 0 {
		t.Errorf("waitReady called %d times, want 0", f.waits)
	}
	if len(f.longhorns) != 0 {
		t.Errorf("longhornInstall called %d times, want 0", len(f.longhorns))
	}

	close(updates)
	reported := false
	for u := range updates {
		if u.Resource != "provider" || u.Action != "deploy" {
			t.Errorf("update %q has resource %q and action %q, want provider and deploy", u.Message, u.Resource, u.Action)
		}
		if strings.Contains(u.Message, "install RKE2 "+testVersion) {
			reported = true
		}
	}
	if !reported {
		t.Error("dry run did not report the planned install")
	}
}

func TestDeploy_InvalidConfig(t *testing.T) {
	f := newFakes(rke2test.NewHost("Linux"))
	block := rke2Block(func(m map[string]any) { delete(m, "host") })

	err := f.provider().Deploy(context.Background(), testProject, clusterConfig(block), cluster.DeployOptions{})
	if err == nil || !strings.Contains(err.Error(), "cluster.rke2.host is required") {
		t.Fatalf("Deploy error = %v, want one containing %q", err, "cluster.rke2.host is required")
	}
	if f.dials != 0 {
		t.Errorf("dialled %d times, want 0", f.dials)
	}
}

func TestDeploy_HostCheckFails(t *testing.T) {
	f := newFakes(rke2test.NewHost("Darwin"))

	err := f.provider().Deploy(context.Background(), testProject, clusterConfig(rke2Block()), cluster.DeployOptions{})
	if err == nil || !strings.Contains(err.Error(), "Darwin") {
		t.Fatalf("Deploy error = %v, want one naming Darwin", err)
	}
	if len(f.host.Writes) != 0 {
		t.Errorf("Writes = %v, want none", f.host.Writes)
	}
	if len(f.installs) != 0 {
		t.Errorf("installer calls = %v, want none", f.installs)
	}
}

func TestDeploy_Downgrade(t *testing.T) {
	f := newFakes(installedHost(t, newerVersion))

	err := f.provider().Deploy(context.Background(), testProject, clusterConfig(rke2Block()), cluster.DeployOptions{})
	if err == nil || !strings.Contains(err.Error(), "refusing to downgrade") {
		t.Fatalf("Deploy error = %v, want one containing %q", err, "refusing to downgrade")
	}
	if len(f.host.Writes) != 0 {
		t.Errorf("Writes = %v, want none", f.host.Writes)
	}
}

func TestDeploy_LonghornDisabled(t *testing.T) {
	f := newFakes(rke2test.NewHost("Linux"))
	block := rke2Block(func(m map[string]any) {
		m["longhorn"] = map[string]any{"enabled": false}
		m["storage_class"] = "local-path"
	})

	if err := f.provider().Deploy(context.Background(), testProject, clusterConfig(block), cluster.DeployOptions{}); err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	if len(f.longhorns) != 0 {
		t.Errorf("longhornInstall called %d times, want 0", len(f.longhorns))
	}
	if f.waits != 1 {
		t.Errorf("waitReady called %d times, want 1", f.waits)
	}
}

func TestDeploy_ClosesConnection(t *testing.T) {
	tests := []struct {
		name    string
		host    func(t *testing.T) *rke2test.FakeRunner
		opts    cluster.DeployOptions
		waitErr error
		wantErr string // "" means Deploy succeeds
	}{
		{name: "success", host: func(*testing.T) *rke2test.FakeRunner { return rke2test.NewHost("Linux") }},
		{name: "dry run", host: func(*testing.T) *rke2test.FakeRunner { return rke2test.NewHost("Linux") }, opts: cluster.DeployOptions{DryRun: true}},
		{name: "host check fails", host: func(*testing.T) *rke2test.FakeRunner { return rke2test.NewHost("Darwin") }, wantErr: "Darwin"},
		{name: "downgrade refused", host: func(t *testing.T) *rke2test.FakeRunner { return installedHost(t, newerVersion) }, wantErr: "refusing to downgrade"},
		{
			name:    "node never Ready",
			host:    func(t *testing.T) *rke2test.FakeRunner { return installedHost(t, testVersion) },
			waitErr: errors.New("node not Ready"),
			wantErr: "node not Ready",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFakes(tt.host(t))
			f.waitErr = tt.waitErr

			err := f.provider().Deploy(context.Background(), testProject, clusterConfig(rke2Block()), tt.opts)
			switch {
			case tt.wantErr == "" && err != nil:
				t.Fatalf("Deploy: %v", err)
			case tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)):
				t.Fatalf("Deploy error = %v, want one containing %q", err, tt.wantErr)
			}
			if f.host.closed != 1 {
				t.Errorf("Close called %d times, want 1", f.host.closed)
			}
		})
	}
}

func TestGetKubeconfig_CachedPerProcess(t *testing.T) {
	f := newFakes(installedHost(t, testVersion))
	p := f.provider()
	ctx := context.Background()
	cfg := clusterConfig(rke2Block())

	first, err := p.GetKubeconfig(ctx, testProject, cfg)
	if err != nil {
		t.Fatalf("GetKubeconfig: %v", err)
	}
	second, err := p.GetKubeconfig(ctx, testProject, cfg)
	if err != nil {
		t.Fatalf("second GetKubeconfig: %v", err)
	}

	if f.dials != 1 {
		t.Errorf("dialled %d times for two calls, want 1", f.dials)
	}
	if f.host.closed != 1 {
		t.Errorf("Close called %d times, want 1", f.host.closed)
	}
	if string(first) != string(second) {
		t.Errorf("second call returned different bytes:\n%s\nwant:\n%s", second, first)
	}
	if got, want := serverOf(t, first), "https://"+testAPIServer+":6443"; got != want {
		t.Errorf("server = %q, want %q", got, want)
	}

	// The cache is per project: another project fetches its own.
	if _, err := p.GetKubeconfig(ctx, "other", cfg); err != nil {
		t.Fatalf("GetKubeconfig for another project: %v", err)
	}
	if f.dials != 2 {
		t.Errorf("dialled %d times after a second project, want 2", f.dials)
	}
}

func TestValidate_MissingKeyFile(t *testing.T) {
	dir := t.TempDir()
	key := filepath.Join(dir, "id_ed25519")
	knownHosts := filepath.Join(dir, "known_hosts")
	for _, path := range []string{key, knownHosts} {
		if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	missing := filepath.Join(dir, "missing")

	withSSH := func(keyPath, knownHostsPath string) func(map[string]any) {
		return func(m map[string]any) {
			m["ssh"] = map[string]any{"private_key_path": keyPath, "known_hosts_path": knownHostsPath}
		}
	}
	tests := []struct {
		name    string
		block   map[string]any
		wantErr string // "" means Validate succeeds
	}{
		{name: "both files exist", block: rke2Block(withSSH(key, knownHosts))},
		{name: "key file missing", block: rke2Block(withSSH(missing, knownHosts)), wantErr: "cluster.rke2.ssh.private_key_path"},
		{name: "known_hosts missing", block: rke2Block(withSSH(key, missing)), wantErr: "cluster.rke2.ssh.known_hosts_path"},
		{
			name:    "invalid config",
			block:   rke2Block(withSSH(key, knownHosts), func(m map[string]any) { delete(m, "host") }),
			wantErr: "cluster.rke2.host is required",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFakes(rke2test.NewHost("Linux"))

			err := f.provider().Validate(context.Background(), testProject, clusterConfig(tt.block))
			switch {
			case tt.wantErr == "" && err != nil:
				t.Fatalf("Validate: %v", err)
			case tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)):
				t.Fatalf("Validate error = %v, want one containing %q", err, tt.wantErr)
			}
			if f.dials != 0 {
				t.Errorf("dialled %d times, want 0", f.dials)
			}
		})
	}
}

func TestInfraSettings(t *testing.T) {
	tests := []struct {
		name string
		cfg  *config.ClusterConfig
		want cluster.InfraSettings
	}{
		{
			name: "nil config",
			cfg:  nil,
			want: cluster.InfraSettings{StorageClass: "longhorn", LonghornEnabled: true},
		},
		{
			name: "default config",
			cfg:  clusterConfig(rke2Block()),
			want: cluster.InfraSettings{StorageClass: "longhorn", LonghornEnabled: true},
		},
		{
			name: "longhorn disabled",
			cfg: clusterConfig(rke2Block(func(m map[string]any) {
				m["longhorn"] = map[string]any{"enabled": false}
				m["storage_class"] = "local-path"
			})),
			want: cluster.InfraSettings{StorageClass: "local-path", LonghornEnabled: false},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := NewProvider().InfraSettings(tt.cfg)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("InfraSettings = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestSummary(t *testing.T) {
	t.Run("default config", func(t *testing.T) {
		got := NewProvider().Summary(clusterConfig(rke2Block()))
		for _, key := range []string{"Provider", "Host", "RKE2 Version", "Destroy deletes"} {
			if got[key] == "" {
				t.Errorf("Summary[%q] is empty; Summary = %v", key, got)
			}
		}
		if got["Host"] != testHost {
			t.Errorf("Host = %q, want %q", got["Host"], testHost)
		}
		if got["RKE2 Version"] != testVersion {
			t.Errorf("RKE2 Version = %q, want %q", got["RKE2 Version"], testVersion)
		}
		if !strings.Contains(got["Destroy deletes"], "/var/lib/longhorn") {
			t.Errorf("Destroy deletes = %q, want it to name /var/lib/longhorn", got["Destroy deletes"])
		}
	})

	t.Run("nil config", func(t *testing.T) {
		got := NewProvider().Summary(nil)
		if got["Provider"] == "" {
			t.Errorf("Summary(nil) = %v, want at least the provider", got)
		}
	})
}
