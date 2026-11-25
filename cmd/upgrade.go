package cmd

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/sebrandon1/cmo-cli/lib/apply"
	"github.com/sebrandon1/cmo-cli/lib/install"
	"github.com/sebrandon1/cmo-cli/lib/kube"
)

var upgradeOpts struct {
	version  string
	manifest string
	noWait   bool
	dryRun   bool
}

var upgradeCmd = &cobra.Command{
	Use:   "upgrade",
	Short: "Upgrade cert-manager to a newer release",
	Long: `Upgrade an installed cert-manager. Downgrades are refused.

OpenShift (OLM): for an install pinned with cmo install --version (Manual
approval), cmo approves the InstallPlan of --version, or with no --version the
newest one OLM is waiting on (cmo status shows it), moving the Subscription
onto that version's stable-vX.Y channel if needed. An unpinned install
(Automatic approval) is already kept at its channel's latest by OLM.

Kubernetes: cmo applies the --version release manifest (default ` + install.DefaultVersion + `) and
deletes what the installed release had that the new one doesn't. CRDs are
never deleted, so certificates and issuers are untouched.`,
	Example: `  cmo status                       # shows an upgrade waiting for approval
  cmo upgrade                      # OLM: approve it; Kubernetes: go to ` + install.DefaultVersion + `
  cmo upgrade --version v1.20.1
  cmo upgrade --dry-run`,
	Args: cobra.NoArgs,
	RunE: runUpgrade,
}

func init() {
	f := upgradeCmd.Flags()
	f.StringVar(&upgradeOpts.version, "version", "", "version to upgrade to (OpenShift: operator version; Kubernetes: cert-manager release)")
	f.StringVar(&upgradeOpts.manifest, "manifest", "", "manifest file or URL of the new release (Kubernetes; needs --version)")
	f.BoolVar(&upgradeOpts.noWait, "no-wait", false, "return once the upgrade is started")
	f.BoolVar(&upgradeOpts.dryRun, "dry-run", false, "say what would change, and change nothing")
	rootCmd.AddCommand(upgradeCmd)
}

// errUpToDate ends an upgrade that has nothing to do.
var errUpToDate = errors.New("up to date")

// upgradeResult is what -o json prints.
type upgradeResult struct {
	Method   string         `json:"method"`
	From     string         `json:"from"`
	To       string         `json:"to"`
	Upgraded bool           `json:"upgraded"`
	Changes  []apply.Change `json:"changes,omitempty"`
}

func runUpgrade(cmd *cobra.Command, _ []string) error {
	if err := install.ValidateVersion(upgradeOpts.version); err != nil {
		return err
	}
	c, err := newClients()
	if err != nil {
		return err
	}
	_, info, err := platformFor(cmd.Context(), c, "auto")
	if err != nil {
		return err
	}
	st, err := install.GetStatus(cmd.Context(), c.Kube, c.Dyn, info)
	if err != nil {
		return err
	}
	if !st.Installed {
		return errors.New("cert-manager is not installed; install it with: cmo install")
	}
	res := upgradeResult{Method: st.Method, From: st.Version}
	if st.Method == install.MethodOLM {
		err = upgradeOLM(cmd, c, &st, &res)
	} else {
		err = upgradeManifest(cmd, c, &st, &res)
	}
	w := cmd.OutOrStdout()
	switch {
	case errors.Is(err, errUpToDate):
		if structured() {
			return printStructured(w, res)
		}
		fmt.Fprintf(w, "cert-manager %s is up to date\n", res.From)
		return nil
	case err != nil || upgradeOpts.dryRun:
		return err
	}
	if !upgradeOpts.noWait {
		if err := waitReady(cmd, c, progress(cmd)); err != nil {
			return err
		}
	}
	res.Upgraded = true
	if structured() {
		return printStructured(w, res)
	}
	fmt.Fprintf(w, "%s upgraded %s → %s (%s)\n", palette(w).Icon("✓"), res.From, res.To, res.Method)
	return nil
}

// checkTarget refuses a downgrade and reports a no-op.
func checkTarget(from, to string) error {
	switch d := install.CompareVersions(to, from); {
	case d < 0:
		return fmt.Errorf("%s is older than the installed %s; cmo doesn't downgrade (uninstall and install it instead)", to, from)
	case d == 0:
		return errUpToDate
	}
	return nil
}

func upgradeOLM(cmd *cobra.Command, c *kube.Clients, st *install.Status, res *upgradeResult) error {
	if upgradeOpts.manifest != "" {
		return errors.New("--manifest is for Kubernetes installs; this one is OLM")
	}
	if st.Approval != install.ApprovalManual {
		if upgradeOpts.version != "" {
			return fmt.Errorf("this install follows channel %s with Automatic approval, so OLM already keeps it at the latest; to pin %s, uninstall and run: cmo install --version %s", st.Channel, upgradeOpts.version, upgradeOpts.version)
		}
		return errUpToDate
	}
	res.To = upgradeOpts.version
	if res.To == "" {
		res.To = st.PendingUpgrade
	}
	if res.To == "" {
		return errUpToDate
	}
	if err := checkTarget(st.Version, res.To); err != nil {
		return err
	}
	if upgradeOpts.dryRun {
		fmt.Fprintf(cmd.OutOrStdout(), "Would upgrade the operator %s → %s on channel %s, approving its InstallPlan\n",
			st.Version, res.To, install.ChannelForVersion(res.To))
		return nil
	}
	if err := confirm(cmd, fmt.Sprintf("Upgrade the cert-manager operator %s → %s?", st.Version, res.To)); err != nil {
		return err
	}
	step := progress(cmd).Start("Upgrading the operator to " + res.To)
	csv, err := install.UpgradeOLM(cmd.Context(), c.Dyn, res.To, waitFor(defaultInstallTimeout), step.Update)
	if err != nil {
		step.Fail("failed")
		return err
	}
	step.Done(csv + " succeeded")
	return nil
}

func upgradeManifest(cmd *cobra.Command, c *kube.Clients, st *install.Status, res *upgradeResult) error {
	if upgradeOpts.manifest != "" && upgradeOpts.version == "" {
		return errors.New("--manifest needs --version, the release it holds")
	}
	if st.Version == "" {
		return errors.New("can't tell which cert-manager release is installed, so can't tell what to remove; uninstall and install the new one instead")
	}
	res.To = upgradeOpts.version
	if res.To == "" {
		res.To = install.DefaultVersion
	}
	if err := checkTarget(st.Version, res.To); err != nil {
		return err
	}
	ctx := cmd.Context()
	step := progress(cmd).Start(fmt.Sprintf("Fetching the %s and %s manifests", st.Version, res.To))
	old, err := fetchObjects(cmd, install.ManifestURL(st.Version))
	if err != nil {
		step.Fail("failed")
		return err
	}
	source := upgradeOpts.manifest
	if source == "" {
		source = install.ManifestURL(res.To)
	}
	next, err := fetchObjects(cmd, source)
	if err != nil {
		step.Fail("failed")
		return err
	}
	removed := install.Removed(old, next)
	step.Done(fmt.Sprintf("%d objects to apply, %d to remove", len(next), len(removed)))
	if upgradeOpts.dryRun {
		w := cmd.OutOrStdout()
		fmt.Fprintf(w, "Would apply %d objects of %s", len(next), res.To)
		if len(removed) == 0 {
			fmt.Fprintln(w, " and remove nothing")
			return nil
		}
		fmt.Fprintln(w, " and remove:")
		for _, o := range removed {
			fmt.Fprintf(w, "  %s\n", apply.Change{Kind: o.GetKind(), Namespace: o.GetNamespace(), Name: o.GetName()}.Ref())
		}
		return nil
	}
	if err := confirm(cmd, fmt.Sprintf("Upgrade cert-manager %s → %s?", st.Version, res.To)); err != nil {
		return err
	}
	step = progress(cmd).Start("Applying cert-manager " + res.To)
	a := newApplier(c)
	if res.Changes, err = a.Apply(ctx, next); err != nil {
		step.Fail("failed")
		return err
	}
	gone, err := a.Delete(ctx, removed)
	if err != nil {
		step.Fail("failed")
		return err
	}
	res.Changes = append(res.Changes, gone...)
	step.Done(fmt.Sprintf("%d applied, %d removed", len(next), apply.Count(gone, apply.Removed)))
	return nil
}

// fetchObjects reads and parses a manifest.
func fetchObjects(cmd *cobra.Command, source string) ([]*unstructured.Unstructured, error) {
	b, err := install.FetchManifest(cmd.Context(), httpClient, source)
	if err != nil {
		return nil, err
	}
	return install.ParseManifest(b)
}
