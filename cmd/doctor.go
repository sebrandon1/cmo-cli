package cmd

import (
	"context"
	"fmt"
	"time"

	"github.com/spf13/cobra"
	authv1 "k8s.io/api/authorization/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/sebrandon1/cmo-cli/lib/cert"
	"github.com/sebrandon1/cmo-cli/lib/cluster"
	"github.com/sebrandon1/cmo-cli/lib/install"
	"github.com/sebrandon1/cmo-cli/lib/issuer"
	"github.com/sebrandon1/cmo-cli/lib/kube"
)

// checkStatus is how one doctor check came out.
type checkStatus string

// Check results.
const (
	checkOK   checkStatus = "ok"
	checkWarn checkStatus = "warn"
	checkFail checkStatus = "fail"
)

// checkIcons are the table icons for each result.
var checkIcons = map[checkStatus]string{checkOK: "✓", checkWarn: "!", checkFail: "✗"}

// check is one line of cmo doctor.
type check struct {
	Name   string      `json:"name"`
	Status checkStatus `json:"status"`
	Detail string      `json:"detail"`
}

var doctorCmd = &cobra.Command{
	Use:   "doctor",
	Short: "Check the cluster, the cert-manager install, and every issuer and certificate",
	Long: `Run checks that explain why something isn't working: cluster access and
permissions, the OLM catalog (OpenShift), the cert-manager deployments, CRDs
and webhook, and every issuer and certificate that is not Ready, with the
reason cert-manager gives.

It exits non-zero when a check fails.`,
	Args: cobra.NoArgs,
	RunE: runDoctor,
}

func init() { rootCmd.AddCommand(doctorCmd) }

func runDoctor(cmd *cobra.Command, _ []string) error {
	c, err := newClients()
	if err != nil {
		return err
	}
	checks := doctorChecks(cmd.Context(), c)
	w := cmd.OutOrStdout()
	failed := 0
	for _, ch := range checks {
		if ch.Status == checkFail {
			failed++
		}
	}
	if structured() {
		if err := printStructured(w, checks); err != nil {
			return err
		}
	} else {
		p := palette(w)
		for _, ch := range checks {
			icon := checkIcons[ch.Status]
			fmt.Fprintf(w, "%s %s: %s\n", p.Icon(icon), ch.Name, ch.Detail)
		}
	}
	if failed > 0 {
		return fmt.Errorf("%d checks failed", failed)
	}
	return nil
}

// doctorChecks runs the checks in order, skipping those that need something
// an earlier check found missing.
func doctorChecks(ctx context.Context, c *kube.Clients) []check {
	var out []check
	add := func(name string, status checkStatus, detail string) { out = append(out, check{name, status, detail}) }

	info, err := detect(ctx, c)
	if err != nil {
		add("cluster", checkFail, err.Error())
		return out
	}
	add("cluster", checkOK, fmt.Sprintf("%s, context %s", info.Describe(), dash(c.Context)))
	out = append(out, permissionChecks(ctx, c)...)
	if info.Platform == cluster.OpenShift {
		out = append(out, catalogChecks(ctx, c, info)...)
	}

	st, err := install.GetStatus(ctx, c.Kube, c.Dyn, info)
	if err != nil {
		add("install", checkFail, err.Error())
		return out
	}
	if !st.Installed {
		add("install", checkWarn, "cert-manager is not installed (run: cmo install)")
		return out
	}
	what := fmt.Sprintf("cert-manager %s via %s", dash(st.CertManagerVersion), st.Method)
	if st.Method == install.MethodOLM {
		what += " (operator " + dash(st.Version) + ")"
	}
	add("install", checkOK, what)
	if st.Method == install.MethodOLM {
		if st.CSVPhase == install.PhaseSucceeded {
			add("operator", checkOK, st.CSV+" Succeeded")
			if st.PendingUpgrade != "" {
				add("upgrade", checkWarn, st.PendingUpgrade+" is waiting for approval (run: cmo upgrade)")
			}
		} else {
			add("operator", checkFail, fmt.Sprintf("%s is %s", dash(st.CSV), dash(st.CSVPhase)))
		}
	}
	for _, d := range st.Deployments {
		switch {
		case !d.Found:
			add("deployment "+d.Name, checkFail, "missing in namespace "+d.Namespace)
		case !d.Available:
			add("deployment "+d.Name, checkFail, fmt.Sprintf("%d/%d ready", d.Ready, d.Desired))
		default:
			add("deployment "+d.Name, checkOK, fmt.Sprintf("%d/%d ready", d.Ready, d.Desired))
		}
	}
	if !st.APIs {
		add("crds", checkFail, "certificates.cert-manager.io is not defined")
		return out
	}
	add("crds", checkOK, "present")
	if err := install.CheckAPI(ctx, c.Dyn, 5*time.Second, nil); err != nil {
		add("webhook", checkFail, err.Error())
	} else {
		add("webhook", checkOK, "accepting objects")
	}
	return append(out, resourceChecks(ctx, c)...)
}

func permissionChecks(ctx context.Context, c *kube.Clients) []check {
	review := &authv1.SelfSubjectAccessReview{Spec: authv1.SelfSubjectAccessReviewSpec{
		ResourceAttributes: &authv1.ResourceAttributes{Verb: "*", Group: "*", Resource: "*"},
	}}
	r, err := c.Kube.AuthorizationV1().SelfSubjectAccessReviews().Create(ctx, review, metav1.CreateOptions{})
	switch {
	case err != nil:
		return []check{{"permissions", checkWarn, "could not check: " + err.Error()}}
	case !r.Status.Allowed:
		return []check{{"permissions", checkWarn, "not cluster-admin: cmo install and uninstall need it"}}
	}
	return []check{{"permissions", checkOK, "cluster-admin"}}
}

func catalogChecks(ctx context.Context, c *kube.Clients, info cluster.Info) []check {
	if !info.OLM {
		return []check{{"olm", checkFail, "the OLM API (operators.coreos.com) is not served"}}
	}
	var out []check
	cs, err := c.Dyn.Resource(kube.GVRCatalogSource).Namespace(install.CatalogNamespace).Get(ctx, install.CatalogSource, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		out = append(out, check{"catalog", checkFail, fmt.Sprintf("CatalogSource %s/%s not found", install.CatalogNamespace, install.CatalogSource)})
	case err != nil:
		out = append(out, check{"catalog", checkWarn, "could not read the CatalogSource: " + err.Error()})
	default:
		state, _, _ := unstructured.NestedString(cs.Object, "status", "connectionState", "lastObservedState")
		if state == "READY" {
			out = append(out, check{"catalog", checkOK, install.CatalogSource + " READY"})
		} else {
			out = append(out, check{"catalog", checkFail, fmt.Sprintf("%s is %s", install.CatalogSource, dash(state))})
		}
	}
	if ch, err := install.DefaultChannel(ctx, c.Dyn, install.CatalogSource, install.CatalogNamespace); err != nil {
		out = append(out, check{"package", checkFail, err.Error()})
	} else {
		out = append(out, check{"package", checkOK, fmt.Sprintf("%s, default channel %s", install.OperatorPackage, ch)})
	}
	return out
}

func resourceChecks(ctx context.Context, c *kube.Clients) []check {
	var out []check
	issuers, err := issuer.List(ctx, c.Dyn, "", "")
	if err != nil {
		return []check{{"issuers", checkWarn, err.Error()}}
	}
	notReady := 0
	for _, s := range issuers {
		if s.Ready != string(metav1.ConditionTrue) {
			notReady++
			out = append(out, check{"issuer " + s.String(), checkWarn, "not ready: " + dash(s.Message)})
		}
	}
	if notReady == 0 {
		out = append(out, check{"issuers", checkOK, fmt.Sprintf("%d ready", len(issuers))})
	}
	certs, err := cert.List(ctx, c.Dyn, "", "")
	if err != nil {
		return append(out, check{"certificates", checkWarn, err.Error()})
	}
	notReady = 0
	for _, s := range certs {
		if s.Ready == string(metav1.ConditionTrue) {
			continue
		}
		notReady++
		detail := "not ready"
		if lines := cert.Diagnose(ctx, c.Dyn, s.Namespace, s.Name); len(lines) > 0 {
			detail += ": " + lines[len(lines)-1]
		}
		out = append(out, check{"certificate " + s.Namespace + "/" + s.Name, checkWarn, detail})
	}
	if notReady == 0 {
		out = append(out, check{"certificates", checkOK, fmt.Sprintf("%d ready", len(certs))})
	}
	return out
}
