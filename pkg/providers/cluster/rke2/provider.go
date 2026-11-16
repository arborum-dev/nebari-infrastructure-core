package rke2

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"sync"

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

	if opts.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, opts.Timeout)
		defer cancel()
	}

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

// Destroy is not implemented yet.
//
// TODO(task 8): replace this placeholder with the RKE2 destroy path.
func (p *Provider) Destroy(_ context.Context, _ string, _ *config.ClusterConfig, _ cluster.DestroyOptions) error {
	return errors.New("rke2 provider: destroy is not implemented yet")
}

// GetKubeconfig returns the cluster's admin kubeconfig, pointed at
// https://<api_server_address>:6443. It reads the kubeconfig from the host
// over SSH the first time and caches it per project for the life of the
// process.
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
