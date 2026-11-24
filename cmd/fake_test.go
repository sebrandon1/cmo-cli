package cmd

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/version"
	fakediscovery "k8s.io/client-go/discovery/fake"
	dynfake "k8s.io/client-go/dynamic/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/sebrandon1/cmo-cli/lib/apply"
	"github.com/sebrandon1/cmo-cli/lib/install"
	"github.com/sebrandon1/cmo-cli/lib/kube"
	"github.com/sebrandon1/cmo-cli/lib/kube/kubetest"
)

// fakeCluster is a fake cluster of the given platform with typed and
// dynamic objects in it.
func fakeCluster(t *testing.T, openshift bool, typed []runtime.Object, dyn ...runtime.Object) *kube.Clients {
	t.Helper()
	c := kubetest.Clients(typed, dyn...)
	d := c.Kube.Discovery().(*fakediscovery.FakeDiscovery)
	d.FakedServerVersion = &version.Info{GitVersion: "v1.34.2"}
	d.Resources = []*metav1.APIResourceList{{GroupVersion: "apps/v1"}}
	if openshift {
		d.Resources = append(d.Resources,
			&metav1.APIResourceList{GroupVersion: "config.openshift.io/v1"},
			&metav1.APIResourceList{GroupVersion: "operators.coreos.com/v1alpha1"})
	}
	return c
}

// readyOnCreate makes the fake act like cert-manager: what is created is
// Ready at revision 1 at once.
func readyOnCreate(c *kube.Clients) {
	c.Dyn.(*dynfake.FakeDynamicClient).PrependReactor("create", "*", func(a k8stesting.Action) (bool, runtime.Object, error) {
		u, ok := a.(k8stesting.CreateAction).GetObject().(*unstructured.Unstructured)
		if ok {
			s := kubetest.Ready("True", "Ready", "")
			s["revision"] = int64(1)
			u.Object["status"] = s
		}
		return false, nil, nil
	})
}

// operandDeployments are the three cert-manager deployments, available.
func operandDeployments(version string) []runtime.Object {
	one := int32(1)
	var out []runtime.Object
	for _, n := range install.Deployments {
		out = append(out, &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Name: n, Namespace: install.OperandNamespace, Labels: map[string]string{"app.kubernetes.io/version": version}},
			Spec:       appsv1.DeploymentSpec{Replicas: &one},
			Status: appsv1.DeploymentStatus{ReadyReplicas: 1, Conditions: []appsv1.DeploymentCondition{
				{Type: appsv1.DeploymentAvailable, Status: corev1.ConditionTrue},
			}},
		})
	}
	return out
}

// recorder is an applier that creates what it applies in the fake dynamic
// client, and records it.
type recorder struct {
	dyn     *kube.Clients
	applied []string
	deleted []string
}

var kindGVR = map[string]schema.GroupVersionResource{
	"Namespace":     kube.GVRNamespace,
	"OperatorGroup": kube.GVROperatorGroup,
	"Subscription":  kube.GVRSubscription,
}

func (r *recorder) Apply(ctx context.Context, objs []*unstructured.Unstructured) ([]apply.Change, error) {
	var out []apply.Change
	for _, o := range objs {
		r.applied = append(r.applied, o.GetKind()+"/"+o.GetName())
		if gvr, ok := kindGVR[o.GetKind()]; ok {
			var ri = r.dyn.Dyn.Resource(gvr).Namespace(o.GetNamespace())
			if _, err := ri.Create(ctx, o, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
				return nil, err
			}
		}
		out = append(out, apply.Change{Kind: o.GetKind(), Namespace: o.GetNamespace(), Name: o.GetName(), Action: apply.Created})
	}
	return out, nil
}

func (r *recorder) Delete(_ context.Context, objs []*unstructured.Unstructured) ([]apply.Change, error) {
	var out []apply.Change
	for _, o := range objs {
		r.deleted = append(r.deleted, o.GetKind()+"/"+o.GetName())
		out = append(out, apply.Change{Kind: o.GetKind(), Namespace: o.GetNamespace(), Name: o.GetName(), Action: apply.Removed})
	}
	return out, nil
}

// use makes the commands talk to c and apply through a recorder, and
// returns the recorder.
func use(t *testing.T, c *kube.Clients) *recorder {
	t.Helper()
	rec := &recorder{dyn: c}
	oldClients, oldApplier, oldTTY := newClients, newApplier, stdinIsTTY
	newClients = func() (*kube.Clients, error) { return c, nil }
	newApplier = func(*kube.Clients) applier { return rec }
	stdinIsTTY = func() bool { return false }
	t.Cleanup(func() { newClients, newApplier, stdinIsTTY = oldClients, oldApplier, oldTTY })
	return rec
}

// result is what one command run printed.
type result struct {
	stdout, stderr string
	err            error
}

// run executes cmo with args, after putting every flag back to its default.
func run(t *testing.T, args ...string) result {
	t.Helper()
	resetFlags(rootCmd)
	var out, errw bytes.Buffer
	rootCmd.SetOut(&out)
	rootCmd.SetErr(&errw)
	rootCmd.SetIn(strings.NewReader(""))
	rootCmd.SetArgs(args)
	err := rootCmd.ExecuteContext(t.Context())
	return result{out.String(), errw.String(), err}
}

func resetFlags(c *cobra.Command) {
	reset := func(f *pflag.Flag) {
		if sv, ok := f.Value.(pflag.SliceValue); ok {
			_ = sv.Replace(nil)
		} else {
			_ = f.Value.Set(f.DefValue)
		}
		f.Changed = false
	}
	c.Flags().VisitAll(reset)
	c.PersistentFlags().VisitAll(reset)
	for _, sub := range c.Commands() {
		resetFlags(sub)
	}
}

func mustContain(t *testing.T, what, got string, wants ...string) {
	t.Helper()
	for _, w := range wants {
		if !strings.Contains(got, w) {
			t.Errorf("%s lacks %q:\n%s", what, w, got)
		}
	}
}
