package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/sebrandon1/cmo-cli/lib/apply"
	"github.com/sebrandon1/cmo-cli/lib/cluster"
	"github.com/sebrandon1/cmo-cli/lib/install"
	"github.com/sebrandon1/cmo-cli/lib/issuer"
	"github.com/sebrandon1/cmo-cli/lib/pebble"
	"github.com/sebrandon1/cmo-cli/lib/wait"
)

var pebbleCmd = &cobra.Command{
	Use:   "pebble",
	Short: "Run Pebble, an ACME test server, to try ACME issuers without a public CA",
	Long: `Pebble is Let's Encrypt's ACME test server. cmo runs it in the cluster (in the
pebble namespace, or -n) so an ACME issuer can be tried with no public CA,
account or rate limits:

  cmo pebble install --always-valid
  cmo pebble issuer pebble
  cmo cert create web --issuer pebble --dns web.apps.example.com

cert-manager still checks HTTP-01 challenges itself before Pebble sees them, so
the DNS name must resolve to the cluster's ingress: on OpenShift, a name under
the *.apps domain; on Kubernetes, one that points at the ingress controller
(such as <ingress IP>.nip.io).`,
	RunE: groupRunE,
}

var pebbleOpts struct {
	version     string
	dnsServer   string
	alwaysValid bool
	noWait      bool
	dryRun      bool
	// issuer
	ingressClass string
	email        string
}

var pebbleInstallCmd = &cobra.Command{
	Use:   "install",
	Short: "Deploy Pebble and wait until it serves",
	Args:  cobra.NoArgs,
	RunE:  runPebbleInstall,
}

var pebbleIssuerCmd = &cobra.Command{
	Use:   "issuer NAME",
	Short: "Create a ClusterIssuer that gets certificates from Pebble over HTTP-01",
	Args:  cobra.ExactArgs(1),
	RunE:  runPebbleIssuer,
}

var pebbleUninstallCmd = &cobra.Command{
	Use:   "uninstall",
	Short: "Remove Pebble (issuers that use it stay; delete them with cmo issuer delete)",
	Args:  cobra.NoArgs,
	RunE:  runPebbleUninstall,
}

func init() {
	f := pebbleInstallCmd.Flags()
	f.StringVar(&pebbleOpts.version, "version", pebble.DefaultVersion, "Pebble image version")
	f.StringVar(&pebbleOpts.dnsServer, "dns-server", pebble.DefaultDNSServer, "DNS server Pebble resolves challenge names with")
	f.BoolVar(&pebbleOpts.alwaysValid, "always-valid", false, "accept every challenge without validating it (cert-manager's self-check still runs)")
	f.BoolVar(&pebbleOpts.noWait, "no-wait", false, "don't wait for Pebble to serve")
	f.BoolVar(&pebbleOpts.dryRun, "dry-run", false, "print the objects that would be applied, and change nothing")

	f = pebbleIssuerCmd.Flags()
	f.StringVar(&pebbleOpts.ingressClass, "ingress-class", "", "ingress class for HTTP-01 (default: openshift-default on OpenShift, nginx elsewhere)")
	f.StringVar(&pebbleOpts.email, "email", "cmo@example.com", "ACME account email (Pebble doesn't check it)")
	f.BoolVar(&pebbleOpts.noWait, "no-wait", false, "don't wait for the issuer to register")

	pebbleUninstallCmd.Flags().BoolVar(&pebbleOpts.noWait, "no-wait", false, "don't wait for the namespace to go")

	pebbleCmd.AddCommand(pebbleInstallCmd, pebbleIssuerCmd, pebbleUninstallCmd)
	rootCmd.AddCommand(pebbleCmd)
}

// pebbleNamespace is -n, or pebble.
func pebbleNamespace() string {
	if namespace != "" {
		return namespace
	}
	return pebble.DefaultNamespace
}

func pebbleObjects() ([]*unstructured.Unstructured, error) {
	return pebble.Objects(pebble.Options{
		Namespace: pebbleNamespace(), Version: pebbleOpts.version,
		DNSServer: pebbleOpts.dnsServer, AlwaysValid: pebbleOpts.alwaysValid,
	})
}

func runPebbleInstall(cmd *cobra.Command, _ []string) error {
	objs, err := pebbleObjects()
	if err != nil {
		return err
	}
	if pebbleOpts.dryRun {
		return printObjects(cmd.OutOrStdout(), objs)
	}
	c, err := newClients()
	if err != nil {
		return err
	}
	ns := pebbleNamespace()
	step := progress(cmd).Start("Deploying Pebble in " + ns)
	changes, err := newApplier(c).Apply(cmd.Context(), objs)
	if err != nil {
		step.Fail("failed")
		return err
	}
	if !pebbleOpts.noWait {
		if err := wait.Deployments(cmd.Context(), c.Kube, ns, []string{pebble.Deployment}, waitFor(defaultWaitTimeout), step.Update); err != nil {
			step.Fail("failed")
			return err
		}
	}
	step.Done(apply.Tally(changes, "nothing to change", apply.Label{Action: apply.Created, Text: "created"},
		apply.Label{Action: apply.Updated, Text: "updated"}, apply.Label{Action: apply.Unchanged, Text: "unchanged"}))
	w := cmd.OutOrStdout()
	if structured() {
		return printStructured(w, map[string]any{"directory": pebble.DirectoryURL(ns), "changes": changes})
	}
	fmt.Fprintf(w, "%s Pebble is serving %s\nNext: cmo pebble issuer pebble\n", palette(w).Icon("✓"), pebble.DirectoryURL(ns))
	return nil
}

func runPebbleIssuer(cmd *cobra.Command, args []string) error {
	class := pebbleOpts.ingressClass
	if class == "" {
		c, err := newClients()
		if err != nil {
			return err
		}
		info, err := detect(cmd.Context(), c)
		if err != nil {
			return err
		}
		class = "nginx"
		if info.Platform == cluster.OpenShift {
			class = "openshift-default"
		}
	}
	r := issuer.NewRef(args[0], "")
	u, err := issuer.ACME(r, issuer.ACMEOptions{
		Server: pebble.DirectoryURL(pebbleNamespace()), Email: pebbleOpts.email,
		SkipTLSVerify: true, HTTP01IngressClass: class,
	})
	if err != nil {
		return err
	}
	// createIssuer reads the shared issuer-create flags.
	issuerCreateOpts.noWait = pebbleOpts.noWait
	issuerCreateOpts.dryRun = false
	return createIssuer(cmd, r, u)
}

func runPebbleUninstall(cmd *cobra.Command, _ []string) error {
	objs, err := pebbleObjects()
	if err != nil {
		return err
	}
	c, err := newClients()
	if err != nil {
		return err
	}
	ns := pebbleNamespace()
	step := progress(cmd).Start("Removing Pebble from " + ns)
	changes, err := newApplier(c).Delete(cmd.Context(), objs)
	if err != nil {
		step.Fail("failed")
		return err
	}
	if !pebbleOpts.noWait {
		if err := install.WaitNamespacesGone(cmd.Context(), c.Dyn, []string{ns}, waitFor(defaultWaitTimeout), step.Update); err != nil {
			step.Fail("failed")
			return err
		}
	}
	step.Done(plural(apply.Count(changes, apply.Removed), "object") + " deleted")
	w := cmd.OutOrStdout()
	if structured() {
		return printStructured(w, changes)
	}
	fmt.Fprintf(w, "%s Pebble is removed\n", palette(w).Icon("✓"))
	return nil
}
