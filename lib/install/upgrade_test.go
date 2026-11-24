package install

import (
	"context"
	"slices"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/sebrandon1/cmo-cli/lib/cluster"
	"github.com/sebrandon1/cmo-cli/lib/kube"
	"github.com/sebrandon1/cmo-cli/lib/kube/kubetest"
)

func TestCompareVersions(t *testing.T) {
	for _, tt := range []struct {
		a, b string
		want int
	}{
		{"v1.20.1", "v1.20.0", 1}, {"v1.20.0", "v1.20.1", -1}, {"v1.9.0", "v1.10.0", -1},
		{"v1.21.3", "v1.21.3", 0}, {"v2.0.0", "v1.99.99", 1},
	} {
		if got := CompareVersions(tt.a, tt.b); got != tt.want {
			t.Errorf("CompareVersions(%s, %s) = %d, want %d", tt.a, tt.b, got, tt.want)
		}
	}
}

func TestPendingPlansNewestFirst(t *testing.T) {
	c := kubetest.Clients(nil,
		installPlan("old", true, CSVName("v1.20.0")),
		installPlan("next", false, CSVName("v1.20.1")),
		installPlan("later", false, CSVName("v1.20.10")),
	)
	plans, err := PendingPlans(context.Background(), c.Dyn)
	if err != nil {
		t.Fatal(err)
	}
	if len(plans) != 2 || plans[0].Name != "later" || PlanVersion(plans[0]) != "v1.20.10" {
		t.Errorf("plans = %+v", plans)
	}
	if PlanVersion(PendingPlan{CSVs: []string{"other.v1"}}) != "" {
		t.Error("PlanVersion of a foreign CSV")
	}
}

func TestStatusShowsPendingUpgrade(t *testing.T) {
	sub := subWith()
	_ = unstructured.SetNestedField(sub.Object, "Manual", "spec", "installPlanApproval")
	_ = unstructured.SetNestedField(sub.Object, CSVName("v1.20.0"), "status", "installedCSV")
	c := kubetest.Clients(nil, sub, csv(CSVName("v1.20.0"), PhaseSucceeded),
		installPlan("done", true, CSVName("v1.20.0")), installPlan("next", false, CSVName("v1.20.1")))
	s, err := GetStatus(context.Background(), c.Kube, c.Dyn, cluster.Info{Platform: cluster.OpenShift, OLM: true})
	if err != nil {
		t.Fatal(err)
	}
	if s.Approval != "Manual" || s.PendingUpgrade != "v1.20.1" {
		t.Errorf("status = %+v", s)
	}
}

func TestUpgradeOLM(t *testing.T) {
	ctx := context.Background()
	sub := subWith()
	_ = unstructured.SetNestedField(sub.Object, "stable-v1.20", "spec", "channel")
	c := kubetest.Clients(nil, sub, installPlan("next", false, CSVName("v1.21.0")), csv(CSVName("v1.21.0"), PhaseSucceeded))
	got, err := UpgradeOLM(ctx, c.Dyn, "v1.21.0", time.Second, nil)
	if err != nil || got != CSVName("v1.21.0") {
		t.Fatalf("UpgradeOLM = %q, %v", got, err)
	}
	s, _ := c.Dyn.Resource(kube.GVRSubscription).Namespace(OperatorNamespace).Get(ctx, OperatorPackage, metav1.GetOptions{})
	if ch, _, _ := unstructured.NestedString(s.Object, "spec", "channel"); ch != "stable-v1.21" {
		t.Errorf("channel = %q, want stable-v1.21", ch)
	}
	p, _ := c.Dyn.Resource(kube.GVRInstallPlan).Namespace(OperatorNamespace).Get(ctx, "next", metav1.GetOptions{})
	if ok, _, _ := unstructured.NestedBool(p.Object, "spec", "approved"); !ok {
		t.Error("the target's InstallPlan was not approved")
	}
	if _, err := UpgradeOLM(ctx, kubetest.Clients(nil).Dyn, "v1.21.0", time.Second, nil); err == nil {
		t.Error("upgraded with no Subscription")
	}
}

func TestRemoved(t *testing.T) {
	old, err := ParseManifest([]byte(manifest + `---
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: cert-manager-tokenrequest
  namespace: cert-manager
---
apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata:
  name: retired.cert-manager.io
`))
	if err != nil {
		t.Fatal(err)
	}
	next, err := ParseManifest([]byte(manifest))
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, o := range Removed(old, next) {
		got = append(got, o.GetKind()+"/"+o.GetName())
	}
	// The Role is gone in the new release; the retired CRD stays, since
	// deleting a CRD deletes its objects.
	if !slices.Equal(got, []string{"Role/cert-manager-tokenrequest"}) {
		t.Errorf("Removed = %v", got)
	}
}
