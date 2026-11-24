// Package install installs, checks and removes cert-manager: through OLM on
// OpenShift (the Red Hat cert-manager Operator), or from the upstream release
// manifest on Kubernetes.
package install

import (
	"context"
	"errors"
	"fmt"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"

	"github.com/sebrandon1/cmo-cli/lib/kube"
	"github.com/sebrandon1/cmo-cli/lib/wait"
)

// OperandNamespace holds the cert-manager controller, webhook and cainjector
// on both platforms. It is also the cluster resource namespace, where
// ClusterIssuers find their secrets.
const OperandNamespace = "cert-manager"

// Deployments are the cert-manager components.
var Deployments = []string{"cert-manager", "cert-manager-webhook", "cert-manager-cainjector"}

// WebhookConfigName names the validating and mutating webhook configurations.
const WebhookConfigName = "cert-manager-webhook"

// WaitForOperands waits until the cert-manager deployments are available.
func WaitForOperands(ctx context.Context, k kubernetes.Interface, timeout time.Duration, note func(string)) error {
	return wait.Deployments(ctx, k, OperandNamespace, Deployments, timeout, note)
}

// apiCheckName is the Issuer the API check dry-runs.
const apiCheckName = "cmo-api-check"

// CheckAPI waits until the cert-manager API accepts objects, which needs the
// webhook to serve: a deployment can be available before its webhook answers.
// It dry-run creates an Issuer, so nothing is left behind.
func CheckAPI(ctx context.Context, dyn dynamic.Interface, timeout time.Duration, note func(string)) error {
	issuer := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "cert-manager.io/v1", "kind": "Issuer",
		"metadata": map[string]any{"name": apiCheckName, "namespace": OperandNamespace},
		"spec":     map[string]any{"selfSigned": map[string]any{}},
	}}
	var last error
	err := wait.For(ctx, 2*time.Second, timeout, func(ctx context.Context) (bool, error) {
		_, err := dyn.Resource(kube.GVRIssuer).Namespace(OperandNamespace).Create(ctx, issuer, metav1.CreateOptions{DryRun: []string{metav1.DryRunAll}})
		if err == nil || apierrors.IsAlreadyExists(err) {
			return true, nil
		}
		last = err
		if note != nil {
			note("waiting for the cert-manager webhook")
		}
		return false, nil
	})
	if errors.Is(err, wait.ErrTimeout) {
		return fmt.Errorf("the cert-manager API is not accepting objects after %s: %w", timeout, last)
	}
	return err
}
