package rke2

import (
	"context"
	"errors"
	"fmt"
	"net"

	"k8s.io/client-go/tools/clientcmd"
)

// apiServerPort is the port RKE2's Kubernetes API server listens on.
const apiServerPort = "6443"

// FetchKubeconfig reads KubeconfigPath and points every cluster at
// https://<apiServerAddress>:6443. The error satisfies fs.ErrNotExist when
// RKE2 has not written a kubeconfig.
func FetchKubeconfig(ctx context.Context, r Runner, apiServerAddress string) ([]byte, error) {
	if apiServerAddress == "" {
		return nil, errors.New("api server address is empty")
	}
	raw, err := r.ReadFile(ctx, KubeconfigPath)
	if err != nil {
		return nil, fmt.Errorf("read kubeconfig: %w", err)
	}
	cfg, err := clientcmd.Load(raw)
	if err != nil {
		return nil, fmt.Errorf("parse kubeconfig %s: %w", KubeconfigPath, err)
	}
	server := "https://" + net.JoinHostPort(apiServerAddress, apiServerPort)
	for _, cluster := range cfg.Clusters {
		cluster.Server = server
	}
	out, err := clientcmd.Write(*cfg)
	if err != nil {
		return nil, fmt.Errorf("serialize kubeconfig: %w", err)
	}
	return out, nil
}
