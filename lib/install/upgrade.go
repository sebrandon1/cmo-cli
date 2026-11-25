package install

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"

	"github.com/sebrandon1/cmo-cli/lib/kube"
)

// CompareVersions orders two vX.Y.Z versions: -1, 0 or 1.
func CompareVersions(a, b string) int {
	pa, pb := versionParts(a), versionParts(b)
	for i := range pa {
		if pa[i] != pb[i] {
			if pa[i] < pb[i] {
				return -1
			}
			return 1
		}
	}
	return 0
}

func versionParts(v string) [3]int {
	var out [3]int
	for i, p := range strings.SplitN(strings.TrimPrefix(v, "v"), ".", 3) {
		out[i], _ = strconv.Atoi(p)
	}
	return out
}

// PendingPlan is an InstallPlan OLM made that is waiting for approval: under
// Manual approval, the next version in the channel.
type PendingPlan struct {
	Name string   `json:"name"`
	CSVs []string `json:"csvs"`
}

// PendingPlans are the operator's unapproved InstallPlans, newest version first.
func PendingPlans(ctx context.Context, dyn dynamic.Interface) ([]PendingPlan, error) {
	l, err := dyn.Resource(kube.GVRInstallPlan).Namespace(OperatorNamespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("listing InstallPlans: %w", err)
	}
	var out []PendingPlan
	for _, p := range l.Items {
		approved, _, _ := unstructured.NestedBool(p.Object, "spec", "approved")
		csvs, _, _ := unstructured.NestedStringSlice(p.Object, "spec", "clusterServiceVersionNames")
		if !approved && len(csvs) > 0 {
			out = append(out, PendingPlan{Name: p.GetName(), CSVs: csvs})
		}
	}
	slices.SortFunc(out, func(a, b PendingPlan) int {
		return CompareVersions(PlanVersion(b), PlanVersion(a))
	})
	return out, nil
}

// PlanVersion is the operator version a plan installs, such as v1.20.1.
func PlanVersion(p PendingPlan) string {
	for _, c := range p.CSVs {
		if v, ok := strings.CutPrefix(c, csvPrefix); ok {
			return v
		}
	}
	return ""
}

// UpgradeOLM moves a Manual-approval (pinned) operator to version target:
// onto target's stable-vX.Y channel, which always carries it, if the
// Subscription follows another one; then it approves target's InstallPlan
// and waits for the CSV to succeed.
func UpgradeOLM(ctx context.Context, dyn dynamic.Interface, target string, timeout time.Duration, note func(string)) (string, error) {
	sub, err := dyn.Resource(kube.GVRSubscription).Namespace(OperatorNamespace).Get(ctx, OperatorPackage, metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("reading Subscription: %w", err)
	}
	channel, _, _ := unstructured.NestedString(sub.Object, "spec", "channel")
	if want := ChannelForVersion(target); channel != want {
		patch, _ := json.Marshal(map[string]any{"spec": map[string]any{"channel": want}})
		if _, err := dyn.Resource(kube.GVRSubscription).Namespace(OperatorNamespace).Patch(ctx, OperatorPackage, types.MergePatchType, patch, metav1.PatchOptions{}); err != nil {
			return "", fmt.Errorf("moving the Subscription to channel %s: %w", want, err)
		}
	}
	// WaitForCSV approves the target's plan once OLM makes it, which after a
	// channel change takes a little while, and waits for its CSV.
	return WaitForCSV(ctx, dyn, OLMOptions{Version: target}, timeout, note)
}

// objectKey identifies an object across manifests.
func objectKey(o *unstructured.Unstructured) string {
	return o.GroupVersionKind().GroupKind().String() + "/" + o.GetNamespace() + "/" + o.GetName()
}

// Removed are the objects in old that new no longer has, CRDs excepted: an
// upgrade never deletes a CRD, and so never deletes certificates.
func Removed(old, next []*unstructured.Unstructured) []*unstructured.Unstructured {
	keep := map[string]bool{}
	for _, o := range next {
		keep[objectKey(o)] = true
	}
	var out []*unstructured.Unstructured
	for _, o := range WithoutCRDs(old) {
		if !keep[objectKey(o)] {
			out = append(out, o)
		}
	}
	return out
}
