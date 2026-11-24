package install

import (
	"context"
	"fmt"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"

	"github.com/sebrandon1/cmo-cli/lib/cluster"
	"github.com/sebrandon1/cmo-cli/lib/kube"
	"github.com/sebrandon1/cmo-cli/lib/wait"
)

// Install methods.
const (
	MethodOLM      = "olm"
	MethodManifest = "manifest"
)

// versionLabel is the label the upstream manifest puts on its deployments.
const versionLabel = "app.kubernetes.io/version"

// Status is what cmo status reports.
type Status struct {
	Platform         cluster.Platform `json:"platform"`
	ServerVersion    string           `json:"serverVersion"`
	OpenShiftVersion string           `json:"openshiftVersion,omitempty"`
	Installed        bool             `json:"installed"`
	// Ready is whether every deployment is available.
	Ready bool `json:"ready"`
	// Method is olm or manifest, when installed.
	Method string `json:"method,omitempty"`
	// Version is the operator version for olm, the release for manifest.
	Version string `json:"version,omitempty"`
	// CertManagerVersion is the cert-manager the deployments run. Under OLM
	// it differs from the operator version.
	CertManagerVersion string `json:"certManagerVersion,omitempty"`
	// Channel, CSV and CSVPhase are set for an OLM install.
	Channel  string `json:"channel,omitempty"`
	CSV      string `json:"csv,omitempty"`
	CSVPhase string `json:"csvPhase,omitempty"`
	// Approval is the Subscription's installPlanApproval: Manual when pinned.
	Approval string `json:"approval,omitempty"`
	// PendingUpgrade is the version of an InstallPlan waiting for approval.
	PendingUpgrade string `json:"pendingUpgrade,omitempty"`
	// CRDs is whether any CRD a purge removes is still defined; APIs whether
	// the Certificate API is.
	CRDs        bool                   `json:"crds"`
	APIs        bool                   `json:"apis"`
	Deployments []wait.DeploymentState `json:"deployments"`
	Resources   Counts                 `json:"resources"`
}

// Counts are how many cert-manager objects exist.
type Counts struct {
	ClusterIssuers    int `json:"clusterIssuers"`
	Issuers           int `json:"issuers"`
	Certificates      int `json:"certificates"`
	CertificatesReady int `json:"certificatesReady"`
	// Managed is how many of the above cmo created.
	Managed int `json:"managed"`
}

// GetStatus reads the install state.
func GetStatus(ctx context.Context, k kubernetes.Interface, dyn dynamic.Interface, info cluster.Info) (Status, error) {
	s := Status{Platform: info.Platform, ServerVersion: info.ServerVersion, OpenShiftVersion: info.OpenShiftVersion}
	if info.OLM {
		if err := olmStatus(ctx, dyn, &s); err != nil {
			return s, err
		}
	}
	s.Ready = true
	for _, n := range Deployments {
		d, err := wait.Deployment(ctx, k, OperandNamespace, n)
		if err != nil {
			return s, err
		}
		s.Deployments = append(s.Deployments, d)
		s.Ready = s.Ready && d.Available
		if d.Found && !s.Installed {
			s.Installed, s.Method = true, MethodManifest
		}
	}
	s.CertManagerVersion = deploymentVersion(ctx, k)
	if s.Method == MethodManifest {
		s.Version = s.CertManagerVersion
	}
	s.Ready = s.Ready && s.Installed

	var err error
	s.CRDs, s.APIs, err = crdState(ctx, dyn)
	if err != nil {
		return s, err
	}
	if s.APIs {
		c, err := CountResources(ctx, dyn)
		if err != nil {
			return s, err
		}
		s.Resources = c
	}
	return s, nil
}

// crdState reports whether any CRD a purge removes is defined, and whether the
// Certificate API is served (defined and not being deleted).
func crdState(ctx context.Context, dyn dynamic.Interface) (owned, apis bool, err error) {
	crds, err := dyn.Resource(kube.GVRCRD).List(ctx, metav1.ListOptions{})
	if apierrors.IsNotFound(err) {
		return false, false, nil
	}
	if err != nil {
		return false, false, fmt.Errorf("listing CRDs: %w", err)
	}
	for i := range crds.Items {
		c := &crds.Items[i]
		owned = owned || OwnedCRD(c)
		apis = apis || (c.GetName() == "certificates.cert-manager.io" && c.GetDeletionTimestamp() == nil)
	}
	return owned, apis, nil
}

func olmStatus(ctx context.Context, dyn dynamic.Interface, s *Status) error {
	sub, err := dyn.Resource(kube.GVRSubscription).Namespace(OperatorNamespace).Get(ctx, OperatorPackage, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("reading Subscription: %w", err)
	}
	s.Installed, s.Method = true, MethodOLM
	s.Channel, _, _ = unstructured.NestedString(sub.Object, "spec", "channel")
	s.Approval, _, _ = unstructured.NestedString(sub.Object, "spec", "installPlanApproval")
	s.CSV, _, _ = unstructured.NestedString(sub.Object, "status", "installedCSV")
	if s.CSV == "" {
		s.CSV, _, _ = unstructured.NestedString(sub.Object, "spec", "startingCSV")
	}
	if s.CSV == "" {
		return nil
	}
	s.Version = strings.TrimPrefix(s.CSV, csvPrefix)
	if plans, err := PendingPlans(ctx, dyn); err == nil && len(plans) > 0 {
		if v := PlanVersion(plans[0]); v != "" && CompareVersions(v, s.Version) > 0 {
			s.PendingUpgrade = v
		}
	}
	csv, err := dyn.Resource(kube.GVRCSV).Namespace(OperatorNamespace).Get(ctx, s.CSV, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		s.CSVPhase = PhaseMissing
		return nil
	}
	if err != nil {
		return fmt.Errorf("reading CSV %s: %w", s.CSV, err)
	}
	s.CSVPhase, _, _ = unstructured.NestedString(csv.Object, "status", "phase")
	return nil
}

func deploymentVersion(ctx context.Context, k kubernetes.Interface) string {
	d, err := k.AppsV1().Deployments(OperandNamespace).Get(ctx, "cert-manager", metav1.GetOptions{})
	if err != nil {
		return ""
	}
	return d.Labels[versionLabel]
}

// CountResources counts Issuers, ClusterIssuers and Certificates in all
// namespaces.
func CountResources(ctx context.Context, dyn dynamic.Interface) (Counts, error) {
	var c Counts
	ci, err := dyn.Resource(kube.GVRClusterIssuer).List(ctx, metav1.ListOptions{})
	if err != nil {
		return c, fmt.Errorf("listing ClusterIssuers: %w", err)
	}
	is, err := dyn.Resource(kube.GVRIssuer).List(ctx, metav1.ListOptions{})
	if err != nil {
		return c, fmt.Errorf("listing Issuers: %w", err)
	}
	certs, err := dyn.Resource(kube.GVRCertificate).List(ctx, metav1.ListOptions{})
	if err != nil {
		return c, fmt.Errorf("listing Certificates: %w", err)
	}
	c.ClusterIssuers, c.Issuers, c.Certificates = len(ci.Items), len(is.Items), len(certs.Items)
	for _, l := range []*unstructured.UnstructuredList{ci, is, certs} {
		for i := range l.Items {
			if l.Items[i].GetLabels()[kube.ManagedByLabel] == kube.ManagedByValue {
				c.Managed++
			}
		}
	}
	for i := range certs.Items {
		if wait.IsReady(&certs.Items[i]) {
			c.CertificatesReady++
		}
	}
	return c, nil
}
