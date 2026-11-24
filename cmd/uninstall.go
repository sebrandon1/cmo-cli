package cmd

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/sebrandon1/cmo-cli/lib/apply"
	"github.com/sebrandon1/cmo-cli/lib/install"
	"github.com/sebrandon1/cmo-cli/lib/kube"
)

var uninstallOpts struct {
	purge    bool
	noWait   bool
	version  string
	manifest string
}

var uninstallCmd = &cobra.Command{
	Use:   "uninstall",
	Short: "Remove cert-manager from the cluster",
	Long: `Remove cert-manager: the operator (OpenShift) or the release manifest objects
(Kubernetes), the cert-manager namespace and the webhook configurations.

The CRDs stay, and with them your Certificates, Issuers and the Secrets they
wrote, so a later cmo install picks up where you left off. --purge deletes the
CRDs too, and so every cert-manager object in the cluster.`,
	Example: `  cmo uninstall
  cmo uninstall --purge --yes    # everything, without asking`,
	Args: cobra.NoArgs,
	RunE: runUninstall,
}

func init() {
	f := uninstallCmd.Flags()
	f.BoolVar(&uninstallOpts.purge, "purge", false, "also delete the cert-manager CRDs, and with them every Certificate and Issuer")
	f.BoolVar(&uninstallOpts.noWait, "no-wait", false, "don't wait for the namespaces to finish deleting")
	f.StringVar(&uninstallOpts.version, "version", "", "release whose manifest to remove (Kubernetes; default: the installed one)")
	f.StringVar(&uninstallOpts.manifest, "manifest", "", "manifest file or URL to remove (Kubernetes)")
	rootCmd.AddCommand(uninstallCmd)
}

func runUninstall(cmd *cobra.Command, _ []string) error {
	ctx := cmd.Context()
	c, err := newClients()
	if err != nil {
		return err
	}
	_, info, err := platformFor(cmd.Context(), c, "auto")
	if err != nil {
		return err
	}
	st, err := install.GetStatus(ctx, c.Kube, c.Dyn, info)
	if err != nil {
		return err
	}
	w, errw := cmd.OutOrStdout(), cmd.ErrOrStderr()
	if !st.Installed && (!uninstallOpts.purge || !st.CRDs) {
		if structured() {
			return printStructured(w, map[string]any{"installed": false, "changes": []apply.Change{}})
		}
		fmt.Fprintln(w, "cert-manager is not installed; nothing to do")
		return nil
	}

	fmt.Fprintln(errw, uninstallSummary(&st))
	if err := confirm(cmd, "Uninstall cert-manager?"); err != nil {
		return err
	}

	prog := progress(cmd)
	step := prog.Start("Removing cert-manager")
	var changes []apply.Change
	switch st.Method {
	case install.MethodOLM:
		changes, err = install.UninstallOLM(ctx, c.Dyn, uninstallOpts.purge)
	default:
		changes, err = uninstallManifest(cmd, c, &st)
	}
	if err != nil {
		step.Fail("failed")
		return err
	}
	step.Done(plural(apply.Count(changes, apply.Removed), "object") + " deleted")

	if !uninstallOpts.noWait {
		step = prog.Start("Waiting for the namespaces to go")
		if err := install.WaitNamespacesGone(ctx, c.Dyn, []string{install.OperatorNamespace, install.OperandNamespace}, waitFor(defaultInstallTimeout), step.Update); err != nil {
			step.Fail("failed")
			return err
		}
		step.Done("gone")
	}
	if structured() {
		return printStructured(w, map[string]any{"installed": false, "changes": changes})
	}
	p := palette(w)
	hint := p.Dim("  (-o yaml lists what was deleted)")
	if verbose {
		printChanges(w, p, onlyRemoved(changes))
		hint = ""
	}
	fmt.Fprintf(w, "%s cert-manager is uninstalled%s\n", p.Icon("✓"), hint)
	return nil
}

// uninstallManifest removes the objects of the installed release manifest.
func uninstallManifest(cmd *cobra.Command, c *kube.Clients, st *install.Status) ([]apply.Change, error) {
	if !st.Installed {
		// Only leftover CRDs to purge.
		return install.UninstallManifest(cmd.Context(), newApplier(c), c.Dyn, nil, true)
	}
	source := uninstallOpts.manifest
	if source == "" {
		v := uninstallOpts.version
		if v == "" {
			v = st.Version
		}
		if v == "" {
			return nil, fmt.Errorf("can't tell which cert-manager release is installed; pass --version or --manifest")
		}
		source = install.ManifestURL(v)
	}
	b, err := install.FetchManifest(cmd.Context(), httpClient, source)
	if err != nil {
		return nil, err
	}
	objs, err := install.ParseManifest(b)
	if err != nil {
		return nil, err
	}
	return install.UninstallManifest(cmd.Context(), newApplier(c), c.Dyn, objs, uninstallOpts.purge)
}

// uninstallSummary says what uninstall will do, and what it leaves.
func uninstallSummary(st *install.Status) string {
	var b strings.Builder
	what := "cert-manager"
	if st.CertManagerVersion != "" {
		what += " " + st.CertManagerVersion
	}
	how := st.Method
	if st.Method == install.MethodOLM && st.Version != "" {
		how += ", operator " + st.Version
	}
	if st.Installed {
		fmt.Fprintf(&b, "This removes %s (%s) from the cluster.", what, how)
	} else {
		b.WriteString("cert-manager is not installed, but its CRDs are.")
	}
	r := st.Resources
	n := r.Certificates + r.Issuers + r.ClusterIssuers
	switch {
	case uninstallOpts.purge && n > 0:
		fmt.Fprintf(&b, "\n--purge deletes the CRDs and with them %s, %s and %s.", plural(r.Certificates, "certificate"), plural(r.Issuers, "issuer"), plural(r.ClusterIssuers, "cluster issuer"))
	case uninstallOpts.purge:
		b.WriteString("\n--purge deletes the cert-manager CRDs.")
	case n > 0:
		fmt.Fprintf(&b, "\nThe CRDs stay, so %s and %s remain (pass --purge to delete them too).", plural(r.Certificates, "certificate"), plural(r.Issuers+r.ClusterIssuers, "issuer"))
	}
	return b.String()
}

func onlyRemoved(changes []apply.Change) []apply.Change {
	out := make([]apply.Change, 0, len(changes))
	for _, c := range changes {
		if c.Action == apply.Removed {
			out = append(out, c)
		}
	}
	return out
}
