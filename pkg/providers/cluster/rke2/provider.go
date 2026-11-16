package rke2

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"sync"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"

	"github.com/nebari-dev/nebari-infrastructure-core/pkg/config"
	"github.com/nebari-dev/nebari-infrastructure-core/pkg/providers/cluster"
	rke2host "github.com/nebari-dev/nebari-infrastructure-core/pkg/rke2"
	"github.com/nebari-dev/nebari-infrastructure-core/pkg/status"
	"github.com/nebari-dev/nebari-infrastructure-core/pkg/storage/longhorn"
)

// ProviderName is the key of this provider's block under cluster.
const ProviderName = "rke2"

// LonghornDataDir is where Longhorn keeps volume data on the host.
const LonghornDataDir = "/var/lib/longhorn"

// Default budgets for Deploy and Destroy when the caller sets no timeout.
// Without one, a service stuck starting, a stalled download or a dead SSH
// connection would block NIC forever.
const (
	// defaultDeployTimeout covers the install script, starting RKE2 (which
	// includes its first image pulls), the 10-minute node wait and about 13
	// minutes for Longhorn's install.
	defaultDeployTimeout = 45 * time.Minute
	// defaultDestroyTimeout covers RKE2's uninstall script and the 10-minute
	// Longhorn uninstall.
	defaultDestroyTimeout = 30 * time.Minute
)

// ErrRKE2NotRunning is wrapped by GetKubeconfig's error when the RKE2
// service on the host is not running, so the cluster's API cannot answer.
var ErrRKE2NotRunning = errors.New("RKE2 is not running")

// Provider is the NIC cluster provider for one existing host on which NIC
// installs and manages RKE2 over SSH. Its function fields are the seams to the
// host, the cluster and Longhorn; NewProvider wires the real ones.
type Provider struct {
	dial              func(context.Context, rke2host.SSHOptions) (rke2host.Runner, error)
	installer         rke2host.Installer
	waitReady         func(ctx context.Context, kubeconfig []byte) error
	apiReachable      func(ctx context.Context, kubeconfig []byte) bool
	longhornInstall   func(ctx context.Context, kubeconfig []byte, cfg *longhorn.Config) error
	longhornUninstall func(ctx context.Context, kubeconfig []byte) error

	// kubeconfigs caches each project's kubeconfig for the life of the
	// process, because NIC asks for it several times per run.
	mu          sync.Mutex
	kubeconfigs map[string][]byte
}

// NewProvider returns a Provider that reaches the host over SSH, installs
// RKE2 with the upstream install script, and talks to the cluster with
// client-go and NIC's Longhorn installer.
func NewProvider() *Provider {
	return &Provider{
		dial:              rke2host.Dial,
		installer:         rke2host.OnlineInstaller{},
		waitReady:         waitNodesReady,
		apiReachable:      apiAnswers,
		longhornInstall:   longhorn.Install,
		longhornUninstall: longhorn.Uninstall,
	}
}

// Name returns "rke2".
func (p *Provider) Name() string { return ProviderName }

// parseConfig decodes the cluster.rke2 block without validating it.
func parseConfig(ctx context.Context, clusterConfig *config.ClusterConfig) (*Config, error) {
	raw := clusterConfig.ProviderConfig()
	if raw == nil {
		return nil, fmt.Errorf("missing cluster.%s configuration block", ProviderName)
	}
	var cfg Config
	if err := config.UnmarshalProviderConfig(ctx, raw, &cfg); err != nil {
		return nil, fmt.Errorf("failed to parse cluster.%s config: %w", ProviderName, err)
	}
	return &cfg, nil
}

// loadConfig decodes and validates the cluster.rke2 block. It touches no
// files and no network.
func loadConfig(ctx context.Context, clusterConfig *config.ClusterConfig) (*Config, error) {
	cfg, err := parseConfig(ctx, clusterConfig)
	if err != nil {
		return nil, err
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// Validate checks the config and that the SSH key and known_hosts files
// exist. It never connects to the host. NIC's orchestrator does not call it,
// so Deploy and Destroy validate the config themselves.
func (p *Provider) Validate(ctx context.Context, projectName string, clusterConfig *config.ClusterConfig) error {
	tracer := otel.Tracer("nebari-infrastructure-core")
	ctx, span := tracer.Start(ctx, "rke2.Validate")
	defer span.End()

	span.SetAttributes(
		attribute.String("provider", ProviderName),
		attribute.String("project_name", projectName),
	)

	status.Send(ctx, status.NewUpdate(status.LevelInfo, "Validating RKE2 provider configuration").
		WithResource("provider").WithAction("validate"))

	cfg, err := loadConfig(ctx, clusterConfig)
	if err != nil {
		span.RecordError(err)
		return err
	}

	sshOpts, err := cfg.SSHOptions()
	if err != nil {
		span.RecordError(err)
		return err
	}
	if _, err := os.Stat(sshOpts.PrivateKeyPath); err != nil {
		err = fmt.Errorf("cluster.rke2.ssh.private_key_path: %w", err)
		span.RecordError(err)
		return err
	}
	if _, err := os.Stat(sshOpts.KnownHostsPath); err != nil {
		err = fmt.Errorf("cluster.rke2.ssh.known_hosts_path: %w", err)
		span.RecordError(err)
		return err
	}

	status.Send(ctx, status.NewUpdate(status.LevelInfo, "RKE2 configuration validated successfully").
		WithResource("provider").WithAction("validate"))
	return nil
}

// Deploy brings the host to the configured RKE2 state, waits for the node to
// be Ready, and installs Longhorn unless it is disabled. Running it again with
// the same config changes nothing on the host. On a dry run it reports what
// would change on the host and stops before modifying anything.
func (p *Provider) Deploy(ctx context.Context, projectName string, clusterConfig *config.ClusterConfig, opts cluster.DeployOptions) error {
	tracer := otel.Tracer("nebari-infrastructure-core")
	ctx, span := tracer.Start(ctx, "rke2.Deploy")
	defer span.End()

	span.SetAttributes(
		attribute.String("provider", ProviderName),
		attribute.String("project_name", projectName),
		attribute.Bool("dry_run", opts.DryRun),
	)

	fail := func(err error) error {
		span.RecordError(err)
		return err
	}

	cfg, err := loadConfig(ctx, clusterConfig)
	if err != nil {
		return fail(err)
	}
	span.SetAttributes(
		attribute.String("host", cfg.Host),
		attribute.String("rke2_version", cfg.Version),
	)

	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = defaultDeployTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	sshOpts, err := cfg.SSHOptions()
	if err != nil {
		return fail(err)
	}

	status.Send(ctx, status.NewUpdate(status.LevelInfo, fmt.Sprintf("Connecting to RKE2 host %s", cfg.Host)).
		WithResource("provider").WithAction("deploy"))

	r, err := p.dial(ctx, sshOpts)
	if err != nil {
		return fail(fmt.Errorf("connect to RKE2 host %s: %w", cfg.Host, err))
	}
	// A failed Close cannot undo or invalidate work already done on the host.
	defer func() { _ = r.Close() }()

	if err := rke2host.Check(ctx, r); err != nil {
		return fail(fmt.Errorf("check RKE2 host %s: %w", cfg.Host, err))
	}
	state, err := rke2host.Inspect(ctx, r)
	if err != nil {
		return fail(fmt.Errorf("inspect RKE2 host %s: %w", cfg.Host, err))
	}
	plan, err := rke2host.PlanInstall(state, cfg.RKE2Options())
	if err != nil {
		return fail(fmt.Errorf("plan RKE2 on host %s: %w", cfg.Host, err))
	}

	if opts.DryRun {
		if plan.Empty() {
			status.Send(ctx, status.NewUpdate(status.LevelInfo, fmt.Sprintf("Dry run: RKE2 on %s already matches the config", cfg.Host)).
				WithResource("provider").WithAction("deploy"))
		}
		for _, line := range plan.Describe() {
			status.Send(ctx, status.NewUpdate(status.LevelInfo, "Dry run: would "+line).
				WithResource("provider").WithAction("deploy"))
		}
		if cfg.LonghornEnabled() {
			status.Send(ctx, status.NewUpdate(status.LevelInfo,
				fmt.Sprintf("Dry run: would install Longhorn with %d replica(s)", cfg.LonghornConfig().ReplicaCount)).
				WithResource("provider").WithAction("deploy"))
		}
		return nil
	}

	for _, line := range plan.Describe() {
		status.Send(ctx, status.NewUpdate(status.LevelProgress, fmt.Sprintf("RKE2 on %s: %s", cfg.Host, line)).
			WithResource("provider").WithAction("deploy"))
	}
	if err := rke2host.Apply(ctx, r, p.installer, plan); err != nil {
		return fail(fmt.Errorf("apply RKE2 on host %s: %w", cfg.Host, err))
	}

	kubeconfig, err := rke2host.FetchKubeconfig(ctx, r, cfg.apiServerAddress())
	if err != nil {
		return fail(fmt.Errorf("fetch kubeconfig from RKE2 host %s: %w", cfg.Host, err))
	}
	p.storeKubeconfig(projectName, kubeconfig)

	status.Send(ctx, status.NewUpdate(status.LevelProgress, "Waiting for the RKE2 node to be Ready").
		WithResource("provider").WithAction("deploy"))
	if err := p.waitReady(ctx, kubeconfig); err != nil {
		return fail(fmt.Errorf("wait for the RKE2 node to be Ready: %w", err))
	}

	if cfg.LonghornEnabled() {
		status.Send(ctx, status.NewUpdate(status.LevelInfo, "Installing Longhorn").
			WithResource("provider").WithAction("deploy"))
		if err := p.longhornInstall(ctx, kubeconfig, cfg.LonghornConfig()); err != nil {
			return fail(fmt.Errorf("install Longhorn: %w", err))
		}
	}

	status.Send(ctx, status.NewUpdate(status.LevelSuccess, fmt.Sprintf("RKE2 %s is running on %s", cfg.Version, cfg.Host)).
		WithResource("provider").WithAction("deploy"))
	return nil
}

// Destroy returns the host to a state with no RKE2 and no cluster data. It
// uninstalls Longhorn first when the cluster is running, runs RKE2's own
// uninstall script, and removes the Longhorn data directory, the only
// directory it ever removes. A host that was never deployed, or is already
// destroyed, succeeds. It never starts RKE2 to reach the API. If Longhorn
// cannot be uninstalled it stops before touching the host, unless opts.Force
// is set, in which case it carries on and returns that error at the end. On a
// dry run it reports what would be removed and changes nothing.
func (p *Provider) Destroy(ctx context.Context, projectName string, clusterConfig *config.ClusterConfig, opts cluster.DestroyOptions) error {
	tracer := otel.Tracer("nebari-infrastructure-core")
	ctx, span := tracer.Start(ctx, "rke2.Destroy")
	defer span.End()

	span.SetAttributes(
		attribute.String("provider", ProviderName),
		attribute.String("project_name", projectName),
		attribute.Bool("dry_run", opts.DryRun),
		attribute.Bool("force", opts.Force),
	)

	fail := func(err error) error {
		span.RecordError(err)
		return err
	}

	cfg, err := loadConfig(ctx, clusterConfig)
	if err != nil {
		return fail(err)
	}
	span.SetAttributes(attribute.String("host", cfg.Host))

	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = defaultDestroyTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	sshOpts, err := cfg.SSHOptions()
	if err != nil {
		return fail(err)
	}

	status.Send(ctx, status.NewUpdate(status.LevelInfo, fmt.Sprintf("Connecting to RKE2 host %s", cfg.Host)).
		WithResource("provider").WithAction("destroy"))

	r, err := p.dial(ctx, sshOpts)
	if err != nil {
		return fail(fmt.Errorf("connect to RKE2 host %s: %w", cfg.Host, err))
	}
	// A failed Close cannot undo or invalidate work already done on the host.
	defer func() { _ = r.Close() }()

	if opts.DryRun {
		if cfg.LonghornEnabled() {
			status.Send(ctx, status.NewUpdate(status.LevelInfo, "Dry run: would uninstall Longhorn if the cluster is running").
				WithResource("provider").WithAction("destroy"))
		}
		status.Send(ctx, status.NewUpdate(status.LevelInfo, fmt.Sprintf("Dry run: would run RKE2's uninstall script on %s", cfg.Host)).
			WithResource("provider").WithAction("destroy"))
		status.Send(ctx, status.NewUpdate(status.LevelInfo, fmt.Sprintf("Dry run: would remove %s on %s", LonghornDataDir, cfg.Host)).
			WithResource("provider").WithAction("destroy"))
		return nil
	}

	// From here the host may change, so whatever was cached no longer holds.
	defer p.forgetKubeconfig(projectName)

	// Longhorn is the only reason to read the kubeconfig, so without it the
	// host is not asked. The kubeconfig is read fresh: the cache may predate
	// the host's last state. A missing one means a host never deployed or
	// already destroyed; an unreadable one is treated like an API that does
	// not answer. Neither stops the destroy.
	var longhornErr error
	if cfg.LonghornEnabled() {
		kubeconfig, err := rke2host.FetchKubeconfig(ctx, r, cfg.apiServerAddress())
		switch {
		case errors.Is(err, fs.ErrNotExist):
			// Nothing to uninstall Longhorn from.
		case err != nil:
			status.Send(ctx, status.NewUpdate(status.LevelWarning,
				fmt.Sprintf("could not read the kubeconfig, skipping Longhorn uninstall: %v", err)).
				WithResource("provider").WithAction("destroy"))
		case p.apiReachable(ctx, kubeconfig):
			status.Send(ctx, status.NewUpdate(status.LevelProgress, "Uninstalling Longhorn").
				WithResource("provider").WithAction("destroy"))
			if err := p.longhornUninstall(ctx, kubeconfig); err != nil {
				longhornErr = fmt.Errorf("failed to uninstall Longhorn: %w", err)
				if !opts.Force {
					return fail(longhornErr)
				}
				status.Send(ctx, status.NewUpdate(status.LevelWarning, longhornErr.Error()+"; continuing because of --force").
					WithResource("provider").WithAction("destroy"))
			}
		}
	}

	status.Send(ctx, status.NewUpdate(status.LevelProgress, fmt.Sprintf("Uninstalling RKE2 and removing %s on %s", LonghornDataDir, cfg.Host)).
		WithResource("provider").WithAction("destroy"))
	uninstallErr := rke2host.Uninstall(ctx, r, []string{LonghornDataDir})
	if uninstallErr != nil {
		uninstallErr = fmt.Errorf("uninstall RKE2 on host %s: %w", cfg.Host, uninstallErr)
	}

	if err := errors.Join(longhornErr, uninstallErr); err != nil {
		return fail(err)
	}
	status.Send(ctx, status.NewUpdate(status.LevelSuccess, fmt.Sprintf("RKE2 removed from %s", cfg.Host)).
		WithResource("provider").WithAction("destroy"))
	return nil
}

// forgetKubeconfig drops the project's cached kubeconfig.
func (p *Provider) forgetKubeconfig(projectName string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.kubeconfigs, projectName)
}

// GetKubeconfig returns the cluster's admin kubeconfig, pointed at
// https://<api_server_address>:6443. It reads the kubeconfig from the host
// over SSH the first time and caches it per project for the life of the
// process. Before reading it, it checks the RKE2 service over SSH, not the
// API: when the service is not running it returns an error wrapping
// ErrRKE2NotRunning and caches nothing, so NIC treats the cluster as
// unreachable. A cached kubeconfig, including the one Deploy stores, is
// returned without contacting the host.
func (p *Provider) GetKubeconfig(ctx context.Context, projectName string, clusterConfig *config.ClusterConfig) ([]byte, error) {
	tracer := otel.Tracer("nebari-infrastructure-core")
	ctx, span := tracer.Start(ctx, "rke2.GetKubeconfig")
	defer span.End()

	span.SetAttributes(
		attribute.String("provider", ProviderName),
		attribute.String("project_name", projectName),
	)

	cfg, err := loadConfig(ctx, clusterConfig)
	if err != nil {
		span.RecordError(err)
		return nil, err
	}

	if kubeconfig, ok := p.cachedKubeconfig(projectName); ok {
		span.SetAttributes(attribute.Bool("cached", true))
		return kubeconfig, nil
	}

	sshOpts, err := cfg.SSHOptions()
	if err != nil {
		span.RecordError(err)
		return nil, err
	}
	r, err := p.dial(ctx, sshOpts)
	if err != nil {
		err = fmt.Errorf("connect to RKE2 host %s: %w", cfg.Host, err)
		span.RecordError(err)
		return nil, err
	}
	// The kubeconfig has been read in full; a failed Close does not affect it.
	defer func() { _ = r.Close() }()

	// rke2.yaml outlives a stopped service, so its presence says nothing about
	// whether the API can answer.
	state, err := rke2host.Inspect(ctx, r)
	if err != nil {
		err = fmt.Errorf("inspect RKE2 host %s: %w", cfg.Host, err)
		span.RecordError(err)
		return nil, err
	}
	if !state.ServiceActive {
		err = fmt.Errorf("%w on %s; run nic deploy to start it", ErrRKE2NotRunning, cfg.Host)
		span.RecordError(err)
		return nil, err
	}

	kubeconfig, err := rke2host.FetchKubeconfig(ctx, r, cfg.apiServerAddress())
	if err != nil {
		err = fmt.Errorf("fetch kubeconfig from RKE2 host %s: %w", cfg.Host, err)
		span.RecordError(err)
		return nil, err
	}
	p.storeKubeconfig(projectName, kubeconfig)
	return kubeconfig, nil
}

// cachedKubeconfig returns a copy of the project's cached kubeconfig.
func (p *Provider) cachedKubeconfig(projectName string) ([]byte, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	kubeconfig, ok := p.kubeconfigs[projectName]
	return bytes.Clone(kubeconfig), ok
}

// storeKubeconfig caches a copy of the project's kubeconfig.
func (p *Provider) storeKubeconfig(projectName string, kubeconfig []byte) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.kubeconfigs == nil {
		p.kubeconfigs = map[string][]byte{}
	}
	p.kubeconfigs[projectName] = bytes.Clone(kubeconfig)
}

// InfraSettings reports the storage class and whether Longhorn is installed.
// Every other setting is left at its zero value: the gateway gets a
// LoadBalancer Service, and there is no local GitOps and no dry-run plan. A
// nil or unparsable config yields the defaults. It does no I/O.
func (p *Provider) InfraSettings(clusterConfig *config.ClusterConfig) cluster.InfraSettings {
	cfg, err := parseConfig(context.Background(), clusterConfig)
	if err != nil {
		return cluster.InfraSettings{StorageClass: longhorn.StorageClassName, LonghornEnabled: true}
	}
	return cluster.InfraSettings{
		StorageClass:    cfg.GetStorageClass(),
		LonghornEnabled: cfg.LonghornEnabled(),
	}
}

// Summary describes the target for confirmation prompts, including what
// destroy deletes. A nil or unparsable config yields only the fixed entries.
// It does no I/O.
func (p *Provider) Summary(clusterConfig *config.ClusterConfig) map[string]string {
	result := map[string]string{
		"Provider":        "RKE2 (single host over SSH)",
		"Destroy deletes": "RKE2 and all cluster data on the host, including Longhorn volume data in " + LonghornDataDir,
	}
	cfg, err := parseConfig(context.Background(), clusterConfig)
	if err != nil {
		return result
	}
	result["Host"] = cfg.Host
	result["RKE2 Version"] = cfg.Version
	return result
}
