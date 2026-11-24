package cluster

import (
	"context"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/version"
	fakediscovery "k8s.io/client-go/discovery/fake"
	kfake "k8s.io/client-go/kubernetes/fake"

	"github.com/sebrandon1/cmo-cli/lib/kube/kubetest"
)

func discoveryWith(groups ...string) *fakediscovery.FakeDiscovery {
	d := kfake.NewClientset().Discovery().(*fakediscovery.FakeDiscovery)
	for _, g := range groups {
		d.Resources = append(d.Resources, &metav1.APIResourceList{GroupVersion: g + "/v1"})
	}
	d.FakedServerVersion = &version.Info{GitVersion: "v1.34.2"}
	return d
}

func TestDetect(t *testing.T) {
	tests := []struct {
		name     string
		groups   []string
		platform Platform
		olm      bool
	}{
		{"kubernetes", []string{"apps"}, Kubernetes, false},
		{"kubernetes with OLM", []string{"operators.coreos.com"}, Kubernetes, true},
		{"openshift", []string{"config.openshift.io", "operators.coreos.com"}, OpenShift, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			info, err := Detect(discoveryWith(tt.groups...))
			if err != nil {
				t.Fatal(err)
			}
			if info.Platform != tt.platform || info.OLM != tt.olm || info.ServerVersion != "v1.34.2" {
				t.Errorf("Detect = %+v", info)
			}
		})
	}
}

func TestResolve(t *testing.T) {
	ocp := Info{Platform: OpenShift, OLM: true}
	if p, err := Resolve(Auto, ocp); err != nil || p != OpenShift {
		t.Errorf("auto on OpenShift = %v, %v", p, err)
	}
	if p, err := Resolve(Kubernetes, ocp); err != nil || p != Kubernetes {
		t.Errorf("forced kubernetes = %v, %v", p, err)
	}
	if _, err := Resolve(OpenShift, Info{Platform: Kubernetes}); err == nil || !strings.Contains(err.Error(), "OLM") {
		t.Errorf("openshift without OLM: err = %v", err)
	}
}

func TestParsePlatform(t *testing.T) {
	for _, s := range Platforms {
		if _, err := ParsePlatform(s); err != nil {
			t.Errorf("ParsePlatform(%q) = %v", s, err)
		}
	}
	if _, err := ParsePlatform("eks"); err == nil {
		t.Error("ParsePlatform(eks) accepted")
	}
}

func TestOpenShiftVersionAndDescribe(t *testing.T) {
	ctx := context.Background()
	cv := kubetest.Obj("config.openshift.io/v1", "ClusterVersion", "", "version",
		map[string]any{"status": map[string]any{"desired": map[string]any{"version": "4.22.1"}}})
	c := kubetest.Clients(nil, cv)
	if got := OpenShiftVersion(ctx, c.Dyn); got != "4.22.1" {
		t.Errorf("OpenShiftVersion = %q", got)
	}
	if got := OpenShiftVersion(ctx, kubetest.Clients(nil).Dyn); got != "" {
		t.Errorf("no ClusterVersion: %q", got)
	}
	ocp := Info{Platform: OpenShift, ServerVersion: "v1.35.9", OpenShiftVersion: "4.22.1"}
	if got := ocp.Describe(); got != "openshift 4.22.1 (kubernetes v1.35.9)" {
		t.Errorf("Describe = %q", got)
	}
	if got := (Info{Platform: Kubernetes, ServerVersion: "v1.34.2"}).Describe(); got != "kubernetes v1.34.2" {
		t.Errorf("Describe = %q", got)
	}
}
