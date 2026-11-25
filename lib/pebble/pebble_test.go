package pebble

import (
	"slices"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/sebrandon1/cmo-cli/lib/kube"
)

func env(t *testing.T, objs []*unstructured.Unstructured, name string) string {
	t.Helper()
	for _, o := range objs {
		if o.GetKind() != "Deployment" {
			continue
		}
		cs, _, _ := unstructured.NestedSlice(o.Object, "spec", "template", "spec", "containers")
		envs, _, _ := unstructured.NestedSlice(cs[0].(map[string]any), "env")
		for _, e := range envs {
			if m := e.(map[string]any); m["name"] == name {
				return m["value"].(string)
			}
		}
	}
	t.Fatalf("no %s", name)
	return ""
}

func TestObjects(t *testing.T) {
	objs, err := Objects(Options{})
	if err != nil {
		t.Fatal(err)
	}
	var kinds []string
	for _, o := range objs {
		kinds = append(kinds, o.GetKind())
		if o.GetLabels()[kube.ManagedByLabel] != kube.ManagedByValue {
			t.Errorf("%s is not labeled as managed by cmo", o.GetKind())
		}
		if o.GetKind() != "Namespace" && o.GetNamespace() != DefaultNamespace {
			t.Errorf("%s in namespace %q", o.GetKind(), o.GetNamespace())
		}
	}
	if !slices.Equal(kinds, []string{"Namespace", "ConfigMap", "Deployment", "Service"}) {
		t.Errorf("kinds = %v", kinds)
	}
	if got := env(t, objs, "PEBBLE_VA_ALWAYS_VALID"); got != "0" {
		t.Errorf("always valid by default = %q", got)
	}
	dep := objs[2]
	cs, _, _ := unstructured.NestedSlice(dep.Object, "spec", "template", "spec", "containers")
	c := cs[0].(map[string]any)
	if c["image"] != "ghcr.io/letsencrypt/pebble:"+DefaultVersion {
		t.Errorf("image = %v", c["image"])
	}
	if args := c["args"].([]any); args[len(args)-1] != DefaultDNSServer {
		t.Errorf("args = %v", args)
	}
	if _, found, _ := unstructured.NestedBool(dep.Object, "spec", "template", "spec", "securityContext", "runAsNonRoot"); found {
		t.Error("runAsNonRoot is set; the image runs as root and Kind won't start it")
	}

	objs, err = Objects(Options{Namespace: "acme", Version: "2.9.0", DNSServer: "10.0.0.10:53", AlwaysValid: true})
	if err != nil {
		t.Fatal(err)
	}
	if objs[0].GetName() != "acme" || objs[2].GetNamespace() != "acme" || env(t, objs, "PEBBLE_VA_ALWAYS_VALID") != "1" {
		t.Errorf("custom options not applied: %v", objs[0].GetName())
	}
}

func TestDirectoryURL(t *testing.T) {
	if got := DirectoryURL(""); got != "https://pebble.pebble.svc.cluster.local:14000/dir" {
		t.Errorf("DirectoryURL = %q", got)
	}
	if got := DirectoryURL("acme"); got != "https://pebble.acme.svc.cluster.local:14000/dir" {
		t.Errorf("DirectoryURL(acme) = %q", got)
	}
}
