package rke2_test

import (
	"context"
	"errors"
	"io/fs"
	"testing"

	"k8s.io/client-go/tools/clientcmd"

	"github.com/nebari-dev/nebari-infrastructure-core/pkg/rke2"
	"github.com/nebari-dev/nebari-infrastructure-core/pkg/rke2/rke2test"
)

const sourceKubeconfig = `apiVersion: v1
kind: Config
clusters:
- name: default
  cluster:
    server: https://127.0.0.1:6443
    certificate-authority-data: Y2EtYnl0ZXM=
contexts:
- name: default
  context:
    cluster: default
    user: default
current-context: default
users:
- name: default
  user:
    client-certificate-data: Y2VydC1ieXRlcw==
    client-key-data: a2V5LWJ5dGVz
`

func TestFetchKubeconfig(t *testing.T) {
	withFile := map[string][]byte{rke2.KubeconfigPath: []byte(sourceKubeconfig)}
	tests := []struct {
		name       string
		addr       string
		files      map[string][]byte
		wantServer string
		wantErr    bool
		wantNoFile bool
	}{
		{name: "ipv4", addr: "203.0.113.10", files: withFile, wantServer: "https://203.0.113.10:6443"},
		{name: "hostname", addr: "node.example", files: withFile, wantServer: "https://node.example:6443"},
		{name: "ipv6", addr: "2001:db8::1", files: withFile, wantServer: "https://[2001:db8::1]:6443"},
		{name: "empty address", addr: "", files: withFile, wantErr: true},
		{name: "no kubeconfig", addr: "203.0.113.10", wantErr: true, wantNoFile: true},
		{name: "invalid kubeconfig", addr: "203.0.113.10", files: map[string][]byte{rke2.KubeconfigPath: []byte("{{{")}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := &rke2test.FakeRunner{Files: tt.files}
			out, err := rke2.FetchKubeconfig(context.Background(), r, tt.addr)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				if tt.wantNoFile && !errors.Is(err, fs.ErrNotExist) {
					t.Errorf("error %v does not satisfy fs.ErrNotExist", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			got, err := clientcmd.Load(out)
			if err != nil {
				t.Fatalf("result does not parse: %v", err)
			}
			want, err := clientcmd.Load([]byte(sourceKubeconfig))
			if err != nil {
				t.Fatal(err)
			}
			if len(got.Clusters) != 1 {
				t.Fatalf("got %d clusters, want 1", len(got.Clusters))
			}
			for name, c := range got.Clusters {
				if c.Server != tt.wantServer {
					t.Errorf("cluster %s server = %q, want %q", name, c.Server, tt.wantServer)
				}
				if string(c.CertificateAuthorityData) != string(want.Clusters[name].CertificateAuthorityData) {
					t.Errorf("cluster %s CA data changed", name)
				}
			}
			if len(got.AuthInfos) != len(want.AuthInfos) {
				t.Errorf("got %d users, want %d", len(got.AuthInfos), len(want.AuthInfos))
			}
			for name, a := range got.AuthInfos {
				w := want.AuthInfos[name]
				if string(a.ClientCertificateData) != string(w.ClientCertificateData) || string(a.ClientKeyData) != string(w.ClientKeyData) {
					t.Errorf("user %s credentials changed", name)
				}
			}
		})
	}
}
