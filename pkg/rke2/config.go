package rke2

import (
	"errors"
	"fmt"

	"sigs.k8s.io/yaml"
)

const tlsSANKey = "tls-san"

// RenderConfig returns the contents of /etc/rancher/rke2/config.yaml. Provider
// defaults are overridden by extra, except that apiServerAddress always leads
// the tls-san list. The output is deterministic and extra is not modified.
func RenderConfig(apiServerAddress string, extra map[string]any) ([]byte, error) {
	if apiServerAddress == "" {
		return nil, errors.New("api server address must not be empty")
	}

	cfg := map[string]any{
		"ingress-controller": "none",
		"enable-servicelb":   true,
	}
	for k, v := range extra {
		if k != tlsSANKey {
			cfg[k] = v
		}
	}

	sans := []string{apiServerAddress}
	if raw, ok := extra[tlsSANKey]; ok {
		more, err := sanList(raw)
		if err != nil {
			return nil, err
		}
		seen := map[string]bool{apiServerAddress: true}
		for _, s := range more {
			if !seen[s] {
				seen[s] = true
				sans = append(sans, s)
			}
		}
	}
	cfg[tlsSANKey] = sans

	out, err := yaml.Marshal(cfg)
	if err != nil {
		return nil, fmt.Errorf("render RKE2 config: %w", err)
	}
	return out, nil
}

func sanList(v any) ([]string, error) {
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
				return nil, fmt.Errorf("%s entries must be strings, got %T", tlsSANKey, e)
			}
			out = append(out, s)
		}
		return out, nil
	default:
		return nil, fmt.Errorf("%s must be a string or a list of strings, got %T", tlsSANKey, v)
	}
}
