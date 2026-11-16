package rke2

import (
	"errors"
	"fmt"

	"sigs.k8s.io/yaml"
)

const (
	tlsSANKey  = "tls-san"
	disableKey = "disable"

	// gatewayAPICRDChart is RKE2's bundled Gateway API CRD chart. NIC installs
	// its own gateway, whose CRDs the bundled chart's admission policy rejects.
	gatewayAPICRDChart = "rke2-gateway-api-crd"
)

// RenderConfig returns the contents of /etc/rancher/rke2/config.yaml. Provider
// defaults are overridden by extra, except that apiServerAddress always leads
// the tls-san list and rke2-gateway-api-crd always leads the disable list. The
// output is deterministic and extra is not modified.
func RenderConfig(apiServerAddress string, extra map[string]any) ([]byte, error) {
	if apiServerAddress == "" {
		return nil, errors.New("api server address must not be empty")
	}

	cfg := map[string]any{
		"ingress-controller": "none",
		"enable-servicelb":   true,
	}
	for k, v := range extra {
		if k != tlsSANKey && k != disableKey {
			cfg[k] = v
		}
	}

	for key, first := range map[string]string{tlsSANKey: apiServerAddress, disableKey: gatewayAPICRDChart} {
		merged, err := mergeList(key, first, extra)
		if err != nil {
			return nil, err
		}
		cfg[key] = merged
	}

	out, err := yaml.Marshal(cfg)
	if err != nil {
		return nil, fmt.Errorf("render RKE2 config: %w", err)
	}
	return out, nil
}

// mergeList returns first followed by the operator's entries for key, without
// duplicates.
func mergeList(key, first string, extra map[string]any) ([]string, error) {
	merged := []string{first}
	raw, ok := extra[key]
	if !ok {
		return merged, nil
	}
	more, err := stringList(key, raw)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{first: true}
	for _, s := range more {
		if !seen[s] {
			seen[s] = true
			merged = append(merged, s)
		}
	}
	return merged, nil
}

func stringList(key string, v any) ([]string, error) {
	switch t := v.(type) {
	case string:
		return []string{t}, nil
	case []string:
		return t, nil
	case []any:
		out := make([]string, 0, len(t))
		for _, e := range t {
			s, ok := e.(string)
			if !ok {
				return nil, fmt.Errorf("%s entries must be strings, got %T", key, e)
			}
			out = append(out, s)
		}
		return out, nil
	default:
		return nil, fmt.Errorf("%s must be a string or a list of strings, got %T", key, v)
	}
}
