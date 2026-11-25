package install

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"

	"github.com/sebrandon1/cmo-cli/lib/kube"
	"github.com/sebrandon1/cmo-cli/lib/wait"
)

// OLM install of the Red Hat cert-manager Operator.
const (
	OperatorNamespace = "cert-manager-operator"
	OperatorPackage   = "openshift-cert-manager-operator"
	CatalogSource     = "redhat-operators"
	CatalogNamespace  = "openshift-marketplace"
	// csvPrefix is the CSV name before the version: cert-manager-operator.v1.18.0.
	csvPrefix = "cert-manager-operator."
	// fallbackChannel is used when the PackageManifest can't be read.
	fallbackChannel = "stable-v1"
)

// CSV phases cmo acts on; PhaseMissing is cmo's, for a CSV that isn't there.
const (
	PhaseSucceeded = "Succeeded"
	PhaseFailed    = "Failed"
	PhaseMissing   = "Missing"
)

// installPlanApproval values: Manual for a pinned install, which only moves
// when cmo approves the next plan.
const (
	ApprovalAutomatic = "Automatic"
	ApprovalManual    = "Manual"
)

// OLMOptions say which operator to subscribe to.
type OLMOptions struct {
	// Version pins the operator, such as v1.18.0: the Subscription starts at
	// that CSV with Manual approval, and only its InstallPlan is approved.
	// Empty means the latest in Channel, with Automatic approval.
	Version string
	// Channel overrides the channel. Empty means stable-vX.Y for a pinned
	// version, or the package's default channel.
	Channel string
	// Source and SourceNamespace name the CatalogSource.
	Source          string
	SourceNamespace string
}

var versionRE = regexp.MustCompile(`^v\d+\.\d+\.\d+$`)

// ValidateVersion checks a version is vX.Y.Z.
func ValidateVersion(v string) error {
	if v != "" && !versionRE.MatchString(v) {
		return fmt.Errorf("version %q must look like v1.2.3", v)
	}
	return nil
}

// CSVName is the operator CSV for a version.
func CSVName(version string) string { return csvPrefix + version }

// ChannelForVersion is the stable-vX.Y channel holding version vX.Y.Z. The
// stable-v1 channel only carries the latest z-stream, so a pinned version
// needs its minor channel.
func ChannelForVersion(version string) string {
	return "stable-" + version[:strings.LastIndex(version, ".")]
}

// DefaultChannel reads the package's default channel from its PackageManifest.
func DefaultChannel(ctx context.Context, dyn dynamic.Interface, source, sourceNS string) (string, error) {
	pm, err := dyn.Resource(kube.GVRPackageManifest).Namespace(sourceNS).Get(ctx, OperatorPackage, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return "", fmt.Errorf("package %s is not in any catalog: is the %s CatalogSource in %s healthy? (cmo doctor checks)", OperatorPackage, source, sourceNS)
	}
	if err != nil {
		return "", fmt.Errorf("reading PackageManifest %s: %w", OperatorPackage, err)
	}
	ch, _, _ := unstructured.NestedString(pm.Object, "status", "defaultChannel")
	if ch == "" {
		return fallbackChannel, nil
	}
	return ch, nil
}

// ResolveChannel picks the channel for opts.
func ResolveChannel(ctx context.Context, dyn dynamic.Interface, opts OLMOptions) (string, error) {
	switch {
	case opts.Channel != "":
		return opts.Channel, nil
	case opts.Version != "":
		return ChannelForVersion(opts.Version), nil
	}
	return DefaultChannel(ctx, dyn, opts.Source, opts.SourceNamespace)
}

func managedLabels() map[string]any {
	return map[string]any{kube.ManagedByLabel: kube.ManagedByValue}
}

// OLMObjects are the Namespace, OperatorGroup and Subscription that install
// the operator on channel.
func OLMObjects(opts OLMOptions, channel string) []*unstructured.Unstructured {
	ns := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1", "kind": "Namespace",
		"metadata": map[string]any{"name": OperatorNamespace, "labels": managedLabels()},
	}}
	og := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "operators.coreos.com/v1", "kind": "OperatorGroup",
		"metadata": map[string]any{"name": OperatorNamespace, "namespace": OperatorNamespace, "labels": managedLabels()},
		"spec":     map[string]any{"targetNamespaces": []any{OperatorNamespace}},
	}}
	spec := map[string]any{
		"channel":             channel,
		"name":                OperatorPackage,
		"source":              opts.Source,
		"sourceNamespace":     opts.SourceNamespace,
		"installPlanApproval": ApprovalAutomatic,
	}
	if opts.Version != "" {
		spec["installPlanApproval"] = ApprovalManual
		spec["startingCSV"] = CSVName(opts.Version)
	}
	sub := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "operators.coreos.com/v1alpha1", "kind": "Subscription",
		"metadata": map[string]any{"name": OperatorPackage, "namespace": OperatorNamespace, "labels": managedLabels()},
		"spec":     spec,
	}}
	return []*unstructured.Unstructured{ns, og, sub}
}

// PlansToApprove are the unapproved InstallPlans that install csv. Newer plans
// in the channel stay unapproved, so a pinned install stays pinned.
func PlansToApprove(plans []unstructured.Unstructured, csv string) []string {
	var names []string
	for _, p := range plans {
		approved, _, _ := unstructured.NestedBool(p.Object, "spec", "approved")
		csvs, _, _ := unstructured.NestedStringSlice(p.Object, "spec", "clusterServiceVersionNames")
		if !approved && slices.Contains(csvs, csv) {
			names = append(names, p.GetName())
		}
	}
	return names
}

// transientRE matches resolution failures caused by a catalog that is
// starting or briefly unreachable, which OLM retries by itself.
var transientRE = regexp.MustCompile(`code = Unavailable|connection refused|connection reset by peer|i/o timeout|context deadline exceeded|no such host`)

// SubscriptionProblem says what is wrong with a Subscription, if anything.
// terminal problems won't fix themselves; the others are reported and waited
// out.
func SubscriptionProblem(sub *unstructured.Unstructured) (msg string, terminal bool) {
	for _, c := range wait.Conditions(sub) {
		if c.Status != string(metav1.ConditionTrue) {
			continue
		}
		switch c.Type {
		case "ResolutionFailed":
			return "resolution failed: " + c.Message, !transientRE.MatchString(c.Message)
		case "InstallPlanFailed":
			return "install plan failed: " + c.Message, true
		case "CatalogSourcesUnhealthy":
			msg = "catalog sources unhealthy"
		}
	}
	return msg, false
}

// approve approves the named InstallPlans.
func approve(ctx context.Context, dyn dynamic.Interface, names []string) error {
	for _, n := range names {
		patch := []byte(`{"spec":{"approved":true}}`)
		if _, err := dyn.Resource(kube.GVRInstallPlan).Namespace(OperatorNamespace).Patch(ctx, n, types.MergePatchType, patch, metav1.PatchOptions{}); err != nil {
			return fmt.Errorf("approving InstallPlan %s: %w", n, err)
		}
	}
	return nil
}

// ErrTerminal marks an OLM failure that waiting won't fix.
var ErrTerminal = errors.New("OLM install failed")

// csvWaiter is one wait for the operator CSV.
type csvWaiter struct {
	dyn    dynamic.Interface
	pinned string
	note   func(string)
	// csv is the CSV being waited for; last the latest non-terminal problem.
	csv, last string
}

// WaitForCSV waits until the operator's CSV has Succeeded, approving the
// pinned InstallPlan along the way, and returns the CSV name.
func WaitForCSV(ctx context.Context, dyn dynamic.Interface, opts OLMOptions, timeout time.Duration, note func(string)) (string, error) {
	w := &csvWaiter{dyn: dyn, note: note}
	if w.note == nil {
		w.note = func(string) {}
	}
	if opts.Version != "" {
		w.pinned = CSVName(opts.Version)
		w.csv = w.pinned
	}
	err := wait.For(ctx, 5*time.Second, timeout, w.poll)
	if errors.Is(err, wait.ErrTimeout) {
		detail := ""
		if w.last != "" {
			detail = " (last problem: " + w.last + ")"
		}
		return w.csv, fmt.Errorf("the operator did not install within %s%s; run cmo doctor", timeout, detail)
	}
	return w.csv, err
}

func (w *csvWaiter) poll(ctx context.Context) (bool, error) {
	if w.pinned != "" {
		plans, err := w.dyn.Resource(kube.GVRInstallPlan).Namespace(OperatorNamespace).List(ctx, metav1.ListOptions{})
		if err != nil {
			return false, fmt.Errorf("listing InstallPlans: %w", err)
		}
		if err := approve(ctx, w.dyn, PlansToApprove(plans.Items, w.pinned)); err != nil {
			return false, err
		}
	}
	sub, err := w.dyn.Resource(kube.GVRSubscription).Namespace(OperatorNamespace).Get(ctx, OperatorPackage, metav1.GetOptions{})
	if err != nil {
		return false, fmt.Errorf("reading Subscription: %w", err)
	}
	if msg, terminal := SubscriptionProblem(sub); msg != "" {
		if terminal {
			return false, fmt.Errorf("%w: %s", ErrTerminal, msg)
		}
		w.last = msg
		w.note(msg)
	}
	if w.pinned == "" {
		w.csv, _, _ = unstructured.NestedString(sub.Object, "status", "installedCSV")
	}
	if w.csv == "" {
		w.note("waiting for OLM to resolve the subscription")
		return false, nil
	}
	return w.csvSucceeded(ctx)
}

func (w *csvWaiter) csvSucceeded(ctx context.Context) (bool, error) {
	c, err := w.dyn.Resource(kube.GVRCSV).Namespace(OperatorNamespace).Get(ctx, w.csv, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		w.note("waiting for " + w.csv)
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("reading CSV %s: %w", w.csv, err)
	}
	phase, _, _ := unstructured.NestedString(c.Object, "status", "phase")
	switch phase {
	case PhaseSucceeded:
		return true, nil
	case PhaseFailed:
		msg, _, _ := unstructured.NestedString(c.Object, "status", "message")
		return false, fmt.Errorf("%w: CSV %s failed: %s", ErrTerminal, w.csv, msg)
	}
	w.note(fmt.Sprintf("%s %s", w.csv, strings.ToLower(orDefault(phase, "pending"))))
	return false, nil
}

func orDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}
