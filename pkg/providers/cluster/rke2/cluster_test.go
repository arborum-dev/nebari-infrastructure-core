package rke2

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// kubeconfigFor returns a kubeconfig pointing at server, an in-process test
// API server.
func kubeconfigFor(server string) []byte {
	return []byte(strings.Replace(testKubeconfig, "https://127.0.0.1:6443", server, 1))
}

// notReadyNodes is a node list with one node whose Ready condition is False.
const notReadyNodes = `{"kind":"NodeList","apiVersion":"v1","items":[
{"metadata":{"name":"node-a"},"status":{"conditions":[{"type":"Ready","status":"False"}]}}]}`

func TestWaitNodesReady_CallerEndsWait(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(notReadyNodes))
	}))
	defer srv.Close()

	interrupted := errors.New("interrupted by the operator")
	tests := []struct {
		name string
		ctx  func() (context.Context, context.CancelFunc)
		want error
	}{
		{
			name: "deadline",
			ctx: func() (context.Context, context.CancelFunc) {
				return context.WithTimeout(context.Background(), 300*time.Millisecond)
			},
			want: context.DeadlineExceeded,
		},
		{
			name: "cancelled",
			ctx: func() (context.Context, context.CancelFunc) {
				ctx, cancel := context.WithCancelCause(context.Background())
				timer := time.AfterFunc(300*time.Millisecond, func() { cancel(interrupted) })
				return ctx, func() { timer.Stop(); cancel(nil) }
			},
			want: interrupted,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := tt.ctx()
			defer cancel()

			err := waitNodesReady(ctx, kubeconfigFor(srv.URL))
			if err == nil {
				t.Fatal("waitNodesReady = nil, want an error")
			}
			if !errors.Is(err, tt.want) {
				t.Errorf("waitNodesReady error %q does not wrap %v", err, tt.want)
			}
			if strings.Contains(err.Error(), "within "+nodeReadyTimeout.String()) {
				t.Errorf("waitNodesReady error %q blames its own %s limit", err, nodeReadyTimeout)
			}
			if !strings.Contains(err.Error(), "not Ready: node-a") {
				t.Errorf("waitNodesReady error %q does not name the node that is not Ready", err)
			}
		})
	}
}

func TestAPIAnswers(t *testing.T) {
	tests := []struct {
		name    string
		handler http.HandlerFunc
		want    bool
	}{
		{
			name: "answers",
			handler: func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/version" {
					http.NotFound(w, r)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"major":"1","minor":"37","gitVersion":"v1.37.2+rke2r1"}`))
			},
			want: true,
		},
		{
			name:    "server error",
			handler: func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusInternalServerError) },
		},
		{
			name:    "never answers",
			handler: func(_ http.ResponseWriter, r *http.Request) { <-r.Context().Done() },
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(tt.handler)
			defer srv.Close()
			// Far shorter than apiAnswerTimeout, so returning in time shows
			// the request honours ctx.
			ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
			defer cancel()

			start := time.Now()
			got := apiAnswers(ctx, kubeconfigFor(srv.URL))
			if got != tt.want {
				t.Errorf("apiAnswers = %t, want %t", got, tt.want)
			}
			if elapsed := time.Since(start); elapsed > 5*time.Second {
				t.Errorf("apiAnswers took %s, want it to stop when ctx ends", elapsed)
			}
		})
	}
}
