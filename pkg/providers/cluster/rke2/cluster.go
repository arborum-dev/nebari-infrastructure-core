package rke2

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

const (
	// nodeReadyTimeout bounds waitNodesReady. A fresh RKE2 server pulls its
	// system images before its node turns Ready.
	nodeReadyTimeout = 10 * time.Minute
	// nodeReadyPollInterval is how often waitNodesReady lists the nodes.
	nodeReadyPollInterval = 5 * time.Second
	// apiAnswerTimeout bounds apiAnswers.
	apiAnswerTimeout = 10 * time.Second
)

// waitNodesReady waits until at least one node is registered and every node
// is Ready, for at most nodeReadyTimeout or until ctx ends.
func waitNodesReady(ctx context.Context, kubeconfig []byte) error {
	tracer := otel.Tracer("nebari-infrastructure-core")
	ctx, span := tracer.Start(ctx, "rke2.waitNodesReady")
	defer span.End()

	restConfig, err := clientcmd.RESTConfigFromKubeConfig(kubeconfig)
	if err != nil {
		err = fmt.Errorf("parse kubeconfig: %w", err)
		span.RecordError(err)
		return err
	}
	client, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		err = fmt.Errorf("create Kubernetes client: %w", err)
		span.RecordError(err)
		return err
	}

	var (
		registered int
		notReady   []string
		// lastErr is the most recent node list error, cleared by a list
		// that succeeds. The poll returns only the context's error, so
		// without it an unreachable API would read as a node not Ready.
		lastErr error
	)
	err = wait.PollUntilContextTimeout(ctx, nodeReadyPollInterval, nodeReadyTimeout, true, func(ctx context.Context) (bool, error) {
		nodes, err := client.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
		if err != nil {
			// Expected while the API server starts; keep polling.
			lastErr = err
			return false, nil
		}
		lastErr = nil
		registered = len(nodes.Items)
		notReady = notReady[:0]
		for _, n := range nodes.Items {
			if !nodeIsReady(n) {
				notReady = append(notReady, n.Name)
			}
		}
		return registered > 0 && len(notReady) == 0, nil
	})
	span.SetAttributes(
		attribute.Int("nodes", registered),
		attribute.Int("nodes_not_ready", len(notReady)),
	)
	if err != nil {
		err = fmt.Errorf("nodes not Ready within %s: %w", nodeReadyTimeout, err)
		switch {
		case lastErr != nil:
			err = fmt.Errorf("%w (last node list error: %v)", err, lastErr)
		case len(notReady) > 0:
			slices.Sort(notReady)
			err = fmt.Errorf("%w (not Ready: %s)", err, strings.Join(notReady, ", "))
		default:
			err = fmt.Errorf("%w (no node registered)", err)
		}
		span.RecordError(err)
		return err
	}
	return nil
}

// nodeIsReady reports whether the node's Ready condition is True.
func nodeIsReady(n corev1.Node) bool {
	for _, c := range n.Status.Conditions {
		if c.Type == corev1.NodeReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

// apiAnswers reports whether the cluster's API server returns its version
// within apiAnswerTimeout.
func apiAnswers(ctx context.Context, kubeconfig []byte) bool {
	tracer := otel.Tracer("nebari-infrastructure-core")
	ctx, span := tracer.Start(ctx, "rke2.apiAnswers")
	defer span.End()

	// ServerVersion takes no context, so the limit is set on the client.
	if ctx.Err() != nil {
		span.RecordError(ctx.Err())
		return false
	}
	restConfig, err := clientcmd.RESTConfigFromKubeConfig(kubeconfig)
	if err != nil {
		span.RecordError(err)
		return false
	}
	restConfig.Timeout = apiAnswerTimeout
	client, err := discovery.NewDiscoveryClientForConfig(restConfig)
	if err != nil {
		span.RecordError(err)
		return false
	}
	if _, err := client.ServerVersion(); err != nil {
		span.RecordError(err)
		span.SetAttributes(attribute.Bool("answered", false))
		return false
	}
	span.SetAttributes(attribute.Bool("answered", true))
	return true
}
