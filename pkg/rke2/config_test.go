package rke2

import (
	"bytes"
	"reflect"
	"testing"

	"sigs.k8s.io/yaml"
)

func TestRenderConfig(t *testing.T) {
	gw := []any{"rke2-gateway-api-crd"}
	tests := []struct {
		name    string
		extra   map[string]any
		want    map[string]any
		wantErr bool
	}{
		{"defaults", nil, map[string]any{"ingress-controller": "none", "enable-servicelb": true, "tls-san": []any{"10.0.0.5"}, "disable": gw}, false},
		{"extra key", map[string]any{"profile": "cis"}, map[string]any{"ingress-controller": "none", "enable-servicelb": true, "tls-san": []any{"10.0.0.5"}, "disable": gw, "profile": "cis"}, false},
		{"override a default", map[string]any{"enable-servicelb": false}, map[string]any{"ingress-controller": "none", "enable-servicelb": false, "tls-san": []any{"10.0.0.5"}, "disable": gw}, false},
		{"tls-san list", map[string]any{"tls-san": []any{"a.example"}}, map[string]any{"ingress-controller": "none", "enable-servicelb": true, "tls-san": []any{"10.0.0.5", "a.example"}, "disable": gw}, false},
		{"tls-san string list", map[string]any{"tls-san": []string{"a.example"}}, map[string]any{"ingress-controller": "none", "enable-servicelb": true, "tls-san": []any{"10.0.0.5", "a.example"}, "disable": gw}, false},
		{"tls-san string", map[string]any{"tls-san": "a.example"}, map[string]any{"ingress-controller": "none", "enable-servicelb": true, "tls-san": []any{"10.0.0.5", "a.example"}, "disable": gw}, false},
		{"tls-san duplicate", map[string]any{"tls-san": []any{"10.0.0.5"}}, map[string]any{"ingress-controller": "none", "enable-servicelb": true, "tls-san": []any{"10.0.0.5"}, "disable": gw}, false},
		{"disable string", map[string]any{"disable": "rke2-metrics-server"}, map[string]any{"ingress-controller": "none", "enable-servicelb": true, "tls-san": []any{"10.0.0.5"}, "disable": []any{"rke2-gateway-api-crd", "rke2-metrics-server"}}, false},
		{"disable list", map[string]any{"disable": []any{"rke2-metrics-server", "rke2-coredns"}}, map[string]any{"ingress-controller": "none", "enable-servicelb": true, "tls-san": []any{"10.0.0.5"}, "disable": []any{"rke2-gateway-api-crd", "rke2-metrics-server", "rke2-coredns"}}, false},
		{"disable string list", map[string]any{"disable": []string{"rke2-metrics-server"}}, map[string]any{"ingress-controller": "none", "enable-servicelb": true, "tls-san": []any{"10.0.0.5"}, "disable": []any{"rke2-gateway-api-crd", "rke2-metrics-server"}}, false},
		{"disable duplicate", map[string]any{"disable": []any{"rke2-metrics-server", "rke2-gateway-api-crd", "rke2-metrics-server"}}, map[string]any{"ingress-controller": "none", "enable-servicelb": true, "tls-san": []any{"10.0.0.5"}, "disable": []any{"rke2-gateway-api-crd", "rke2-metrics-server"}}, false},
		{"disable wrong type", map[string]any{"disable": 5}, nil, true},
		{"disable wrong element type", map[string]any{"disable": []any{5}}, nil, true},
		{"tls-san wrong type", map[string]any{"tls-san": 5}, nil, true},
		{"tls-san wrong element type", map[string]any{"tls-san": []any{5}}, nil, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, err := RenderConfig("10.0.0.5", tt.extra)
			if (err != nil) != tt.wantErr {
				t.Fatalf("RenderConfig() error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			var got map[string]any
			if err := yaml.Unmarshal(out, &got); err != nil {
				t.Fatalf("output is not valid YAML: %v", err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("RenderConfig() parsed = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestRenderConfig_EmptyAddress(t *testing.T) {
	if _, err := RenderConfig("", nil); err == nil {
		t.Error("RenderConfig(\"\") = nil error, want error")
	}
}

func TestRenderConfig_DoesNotMutateExtra(t *testing.T) {
	extra := map[string]any{"tls-san": []any{"a.example"}, "profile": "cis"}
	if _, err := RenderConfig("10.0.0.5", extra); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"tls-san": []any{"a.example"}, "profile": "cis"}
	if !reflect.DeepEqual(extra, want) {
		t.Errorf("extra mutated: %#v", extra)
	}
}

func TestRenderConfig_Deterministic(t *testing.T) {
	extra := map[string]any{"profile": "cis", "node-label": []any{"a=b"}, "cni": "calico", "tls-san": "x.example", "debug": true}
	first, err := RenderConfig("10.0.0.5", extra)
	if err != nil {
		t.Fatal(err)
	}
	for range 20 {
		next, err := RenderConfig("10.0.0.5", extra)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(first, next) {
			t.Fatalf("output differs between calls:\n%s\n---\n%s", first, next)
		}
	}
}
