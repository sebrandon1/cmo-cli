package install

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"

	"github.com/sebrandon1/cmo-cli/lib/apply"
	"github.com/sebrandon1/cmo-cli/lib/kube"
	"github.com/sebrandon1/cmo-cli/lib/wait"
)

// olmLabel is the label OLM puts on what it installs for the operator.
const olmLabel = "operators.coreos.com/" + OperatorPackage + "." + OperatorNamespace

// crdGroups are the API groups whose CRDs a purge removes.
var crdGroups = []string{"cert-manager.io", "acme.cert-manager.io"}

// del deletes one object and reports it.
func del(ctx context.Context, dyn dynamic.Interface, gvr schema.GroupVersionResource, kind, ns, name string) (apply.Change, error) {
	var ri dynamic.ResourceInterface = dyn.Resource(gvr)
	if ns != "" {
		ri = dyn.Resource(gvr).Namespace(ns)
	}
	policy := metav1.DeletePropagationBackground
	err := ri.Delete(ctx, name, metav1.DeleteOptions{PropagationPolicy: &policy})
	return apply.Outcome(apply.Change{Kind: kind, Namespace: ns, Name: name}, err)
}

// rbacSelectors match the cluster RBAC that nothing else deletes: what the
// operator creates for the cert-manager deployments, and the operator's own
// (OLM leaves it behind once the CSV is gone).
var rbacSelectors = []string{"app.kubernetes.io/instance=cert-manager", "app.kubernetes.io/part-of=cert-manager-operator"}

// UninstallOLM removes the operator Subscription and CSV, then the operator
// and operand namespaces, the webhook configurations (which would otherwise
// block every cert-manager write) and the cluster RBAC nothing else deletes.
// CRDs, and so every Certificate and Issuer, stay unless purge.
//
// The operator's CertManager config is not deleted first: the operator
// recreates a default one at once. It keeps its finalizers once the operator
// is gone, which a purge clears.
func UninstallOLM(ctx context.Context, dyn dynamic.Interface, purge bool) ([]apply.Change, error) {
	var changes []apply.Change
	add := func(c apply.Change, err error) error {
		changes = append(changes, c)
		return err
	}
	if err := add(del(ctx, dyn, kube.GVRSubscription, "Subscription", OperatorNamespace, OperatorPackage)); err != nil {
		return changes, err
	}
	csvs, err := dyn.Resource(kube.GVRCSV).Namespace(OperatorNamespace).List(ctx, metav1.ListOptions{LabelSelector: olmLabel})
	if err != nil && !apierrors.IsNotFound(err) {
		return changes, fmt.Errorf("listing CSVs: %w", err)
	}
	selectors := slices.Clone(rbacSelectors)
	if csvs != nil {
		for _, c := range csvs.Items {
			if err := add(del(ctx, dyn, kube.GVRCSV, "ClusterServiceVersion", OperatorNamespace, c.GetName())); err != nil {
				return changes, err
			}
			selectors = append(selectors, "olm.owner="+c.GetName())
		}
	}
	rest, err := removeCommon(ctx, dyn, []string{OperatorNamespace, OperandNamespace}, selectors, purge)
	return append(changes, rest...), err
}

// UninstallManifest deletes what the release manifest created, keeping the
// CRDs unless purge.
func UninstallManifest(ctx context.Context, deleter apply.Deleter, dyn dynamic.Interface, objs []*unstructured.Unstructured, purge bool) ([]apply.Change, error) {
	changes, err := deleter.Delete(ctx, WithoutCRDs(objs))
	if err != nil {
		return changes, err
	}
	var namespaces []string
	if !slices.ContainsFunc(changes, func(c apply.Change) bool { return c.Kind == "Namespace" && c.Name == OperandNamespace }) {
		namespaces = append(namespaces, OperandNamespace)
	}
	rest, err := removeCommon(ctx, dyn, namespaces, rbacSelectors, purge)
	return append(changes, rest...), err
}

// target names one object to delete.
type target struct {
	gvr        schema.GroupVersionResource
	kind, name string
}

func removeCommon(ctx context.Context, dyn dynamic.Interface, namespaces, rbacSelectors []string, purge bool) ([]apply.Change, error) {
	var changes []apply.Change
	steps := []target{
		{kube.GVRValidatingWH, "ValidatingWebhookConfiguration", WebhookConfigName},
		{kube.GVRMutatingWH, "MutatingWebhookConfiguration", WebhookConfigName},
	}
	for _, n := range namespaces {
		steps = append(steps, target{kube.GVRNamespace, "Namespace", n})
	}
	rbac, err := selected(ctx, dyn, rbacSelectors)
	if err != nil {
		return changes, err
	}
	steps = append(steps, rbac...)
	for _, s := range steps {
		c, err := del(ctx, dyn, s.gvr, s.kind, "", s.name)
		if err != nil {
			return changes, err
		}
		changes = append(changes, c)
	}
	if !purge {
		return changes, nil
	}
	crds, err := dyn.Resource(kube.GVRCRD).List(ctx, metav1.ListOptions{})
	if err != nil {
		return changes, fmt.Errorf("listing CRDs: %w", err)
	}
	for _, crd := range crds.Items {
		if !OwnedCRD(&crd) {
			continue
		}
		// Nothing is left to process finalizers on what the CRD holds, so a
		// finalizer would leave the CRD terminating forever.
		if err := clearFinalizers(ctx, dyn, &crd); err != nil {
			return changes, err
		}
		c, err := del(ctx, dyn, kube.GVRCRD, "CustomResourceDefinition", "", crd.GetName())
		if err != nil {
			return changes, err
		}
		changes = append(changes, c)
	}
	return changes, nil
}

// selected are the ClusterRoles and ClusterRoleBindings matching any selector.
func selected(ctx context.Context, dyn dynamic.Interface, selectors []string) ([]target, error) {
	var out []target
	seen := map[string]bool{}
	for _, kind := range []struct {
		gvr  schema.GroupVersionResource
		kind string
	}{{kube.GVRClusterRole, "ClusterRole"}, {kube.GVRClusterRoleBinding, "ClusterRoleBinding"}} {
		for _, sel := range selectors {
			l, err := dyn.Resource(kind.gvr).List(ctx, metav1.ListOptions{LabelSelector: sel})
			if err != nil {
				return nil, fmt.Errorf("listing %ss: %w", kind.kind, err)
			}
			for _, o := range l.Items {
				if key := kind.kind + "/" + o.GetName(); !seen[key] {
					seen[key] = true
					out = append(out, target{kind.gvr, kind.kind, o.GetName()})
				}
			}
		}
	}
	return out, nil
}

// crdResource is the resource a CRD serves, at its storage version.
func crdResource(crd *unstructured.Unstructured) (schema.GroupVersionResource, bool) {
	group, _, _ := unstructured.NestedString(crd.Object, "spec", "group")
	plural, _, _ := unstructured.NestedString(crd.Object, "spec", "names", "plural")
	versions, _, _ := unstructured.NestedSlice(crd.Object, "spec", "versions")
	for _, v := range versions {
		m, _ := v.(map[string]any)
		if stored, _ := m["storage"].(bool); stored {
			name, _ := m["name"].(string)
			return schema.GroupVersionResource{Group: group, Version: name, Resource: plural}, plural != "" && name != ""
		}
	}
	return schema.GroupVersionResource{}, false
}

// clearFinalizers removes the finalizers from every object of crd's kind
// that has them.
func clearFinalizers(ctx context.Context, dyn dynamic.Interface, crd *unstructured.Unstructured) error {
	gvr, ok := crdResource(crd)
	if !ok {
		return nil
	}
	l, err := dyn.Resource(gvr).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil //nolint:nilerr // a kind that can't be listed has nothing to clear
	}
	patch := []byte(`{"metadata":{"finalizers":null}}`)
	for _, o := range l.Items {
		if len(o.GetFinalizers()) == 0 {
			continue
		}
		var ri dynamic.ResourceInterface = dyn.Resource(gvr)
		if ns := o.GetNamespace(); ns != "" {
			ri = dyn.Resource(gvr).Namespace(ns)
		}
		if _, err := ri.Patch(ctx, o.GetName(), types.MergePatchType, patch, metav1.PatchOptions{}); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("clearing finalizers on %s %s: %w", gvr.Resource, o.GetName(), err)
		}
	}
	return nil
}

// OwnedCRD reports whether a purge removes crd: it is in a cert-manager API
// group, or OLM installed it with the operator.
func OwnedCRD(crd *unstructured.Unstructured) bool {
	if _, ok := crd.GetLabels()[olmLabel]; ok {
		return true
	}
	group, _, _ := unstructured.NestedString(crd.Object, "spec", "group")
	for _, g := range crdGroups {
		if group == g || strings.HasSuffix(crd.GetName(), "."+g) {
			return true
		}
	}
	return false
}

// WaitNamespacesGone waits until the named namespaces have finished deleting.
func WaitNamespacesGone(ctx context.Context, dyn dynamic.Interface, names []string, timeout time.Duration, note func(string)) error {
	pending := ""
	err := wait.For(ctx, 2*time.Second, timeout, func(ctx context.Context) (bool, error) {
		for _, n := range names {
			_, err := dyn.Resource(kube.GVRNamespace).Get(ctx, n, metav1.GetOptions{})
			if apierrors.IsNotFound(err) {
				continue
			}
			if err != nil {
				return false, err
			}
			pending = n
			if note != nil {
				note("namespace " + n + " terminating")
			}
			return false, nil
		}
		return true, nil
	})
	if errors.Is(err, wait.ErrTimeout) {
		return fmt.Errorf("namespace %s is still terminating after %s", pending, timeout)
	}
	return err
}
