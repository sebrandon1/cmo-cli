// Package kubetest builds fake clusters for tests.
package kubetest

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	dynfake "k8s.io/client-go/dynamic/fake"
	kfake "k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"

	"github.com/sebrandon1/cmo-cli/lib/kube"
)

// Clients is a fake cluster: typed objects go to the typed client, the rest
// (cert-manager, OLM, CRDs) to the dynamic client.
func Clients(typed []runtime.Object, dynamic ...runtime.Object) *kube.Clients {
	return &kube.Clients{
		Config:    &rest.Config{Host: "https://example.test:6443"},
		Kube:      kfake.NewClientset(typed...),
		Dyn:       dynfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), kube.ListKinds(), dynamic...),
		Context:   "fake",
		Namespace: "default",
	}
}

// Obj builds an unstructured object. ns may be empty.
func Obj(apiVersion, kind, ns, name string, fields map[string]any) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]any{"apiVersion": apiVersion, "kind": kind}}
	for k, v := range fields {
		u.Object[k] = v
	}
	u.SetName(name)
	if ns != "" {
		u.SetNamespace(ns)
	}
	u.SetUID(types.UID(kind + "-" + ns + "-" + name))
	u.SetCreationTimestamp(metav1.Now())
	return u
}

// Ready is a status with the given Ready condition.
func Ready(status, reason, message string) map[string]any {
	return map[string]any{"conditions": []any{map[string]any{
		"type": "Ready", "status": status, "reason": reason, "message": message,
	}}}
}

// Owned makes owner the owner of u.
func Owned(u, owner *unstructured.Unstructured) *unstructured.Unstructured {
	u.SetOwnerReferences([]metav1.OwnerReference{{
		APIVersion: owner.GetAPIVersion(), Kind: owner.GetKind(), Name: owner.GetName(), UID: owner.GetUID(),
	}})
	return u
}
