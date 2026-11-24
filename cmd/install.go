package cmd

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/sebrandon1/cmo-cli/lib/apply"
	"github.com/sebrandon1/cmo-cli/lib/cluster"
	"github.com/sebrandon1/cmo-cli/lib/install"
	"github.com/sebrandon1/cmo-cli/lib/kube"
	"github.com/sebrandon1/cmo-cli/lib/ui"
)

var installOpts struct {
	platform        string
	version         string
	channel         string
	source          string
	sourceNamespace string
	manifest        string
	noWait          bool
	dryRun          bool
}

var installCmd = &cobra.Command{
	Use:   "install",
	Short: "Install cert-manager (OLM on OpenShift, the release manifest elsewhere)",
	Long: `Install cert-manager and wait until it is ready to issue certificates.

On OpenShift, cmo subscribes to the Red Hat cert-manager Operator
(openshift-cert-manager-operator from redhat-operators) in the
cert-manager-operator namespace. With no --version it follows the package's
default channel; with --version it pins that release (Manual approval, only
its InstallPlan is approved).

On other Kubernetes clusters, cmo server-side applies the upstream
cert-manager.yaml for --version (default ` + install.DefaultVersion + `).

Running it again is safe: what is already in place is left alone.`,
	Example: `  cmo install
  cmo install --version v1.18.0          # pin an operator release on OpenShift
  cmo install --platform kubernetes --version v1.20.2
  cmo install --manifest ./cert-manager.yaml   # air-gapped Kubernetes
  cmo install --dry-run                  # print what would be applied`,
	Args: cobra.NoArgs,
	RunE: runInstall,
}

func init() {
	f := installCmd.Flags()
	f.StringVar(&installOpts.platform, "platform", string(cluster.Auto), "auto, openshift or kubernetes")
	f.StringVar(&installOpts.version, "version", "", "version to install, such as v1.18.0 (OpenShift: operator version; Kubernetes: cert-manager release)")
	f.StringVar(&installOpts.channel, "channel", "", "OLM channel (OpenShift; default: stable-vX.Y for --version, else the package default)")
	f.StringVar(&installOpts.source, "source", install.CatalogSource, "OLM CatalogSource (OpenShift)")
	f.StringVar(&installOpts.sourceNamespace, "source-namespace", install.CatalogNamespace, "namespace of the CatalogSource (OpenShift)")
	f.StringVar(&installOpts.manifest, "manifest", "", "manifest file or URL to apply instead of the release download (Kubernetes)")
	f.BoolVar(&installOpts.noWait, "no-wait", false, "return once applied, without waiting for cert-manager to be ready")
	f.BoolVar(&installOpts.dryRun, "dry-run", false, "print the objects that would be applied, and change nothing")
	rootCmd.AddCommand(installCmd)
}

// installResult is what -o json prints.
type installResult struct {
	Platform cluster.Platform `json:"platform"`
	Method   string           `json:"method"`
	Version  string           `json:"version,omitempty"`
	Channel  string           `json:"channel,omitempty"`
	CSV      string           `json:"csv,omitempty"`
	Ready    bool             `json:"ready"`
	Changes  []apply.Change   `json:"changes"`
}

// detect asks the cluster what it is, OpenShift version included.
func detect(ctx context.Context, c *kube.Clients) (cluster.Info, error) {
	info, err := cluster.Detect(c.Kube.Discovery())
	if err == nil && info.Platform == cluster.OpenShift {
		info.OpenShiftVersion = cluster.OpenShiftVersion(ctx, c.Dyn)
	}
	return info, err
}

// platformFor detects the cluster and resolves --platform.
func platformFor(ctx context.Context, c *kube.Clients, requested string) (cluster.Platform, cluster.Info, error) {
	p, err := cluster.ParsePlatform(requested)
	if err != nil {
		return "", cluster.Info{}, err
	}
	info, err := detect(ctx, c)
	if err != nil {
		return "", info, err
	}
	p, err = cluster.Resolve(p, info)
	return p, info, err
}

func validateInstallFlags(cmd *cobra.Command, p cluster.Platform) error {
	if err := install.ValidateVersion(installOpts.version); err != nil {
		return err
	}
	f := cmd.Flags()
	if p == cluster.OpenShift && f.Changed("manifest") {
		return fmt.Errorf("--manifest is for Kubernetes; on OpenShift cmo installs through OLM (or pass --platform kubernetes)")
	}
	if p == cluster.Kubernetes {
		for _, name := range []string{"channel", "source", "source-namespace"} {
			if f.Changed(name) {
				return fmt.Errorf("--%s is for the OLM install on OpenShift", name)
			}
		}
		if f.Changed("manifest") && f.Changed("version") {
			return fmt.Errorf("give --manifest or --version, not both")
		}
	}
	return nil
}

func runInstall(cmd *cobra.Command, _ []string) error {
	ctx := cmd.Context()
	c, err := newClients()
	if err != nil {
		return err
	}
	p, info, err := platformFor(cmd.Context(), c, installOpts.platform)
	if err != nil {
		return err
	}
	if err := validateInstallFlags(cmd, p); err != nil {
		return err
	}
	cur, err := install.GetStatus(ctx, c.Kube, c.Dyn, info)
	if err != nil {
		return err
	}
	want := install.MethodManifest
	if p == cluster.OpenShift {
		want = install.MethodOLM
	}
	if cur.Installed && cur.Method != want {
		return fmt.Errorf("cert-manager is already installed here via %s; uninstall it first (cmo uninstall) to switch to %s", cur.Method, want)
	}

	prog := progress(cmd)
	res := installResult{Platform: p, Method: want}
	if p == cluster.OpenShift {
		err = installOLM(cmd, c, &res)
	} else {
		err = installManifest(cmd, c, &res)
	}
	if err != nil || installOpts.dryRun {
		return err
	}
	if !installOpts.noWait {
		if err := waitReady(cmd, c, prog); err != nil {
			return err
		}
		res.Ready = true
	}
	return printInstall(cmd, &res)
}

func installOLM(cmd *cobra.Command, c *kube.Clients, res *installResult) error {
	ctx := cmd.Context()
	prog := progress(cmd)
	opts := install.OLMOptions{
		Version: installOpts.version, Channel: installOpts.channel,
		Source: installOpts.source, SourceNamespace: installOpts.sourceNamespace,
	}
	step := prog.Start("Resolving the operator channel")
	ch, err := install.ResolveChannel(ctx, c.Dyn, opts)
	if err != nil {
		step.Fail("failed")
		return err
	}
	step.Done("channel " + ch)
	res.Channel, res.Version = ch, opts.Version
	objs := install.OLMObjects(opts, ch)
	if installOpts.dryRun {
		return printObjects(cmd.OutOrStdout(), objs)
	}
	step = prog.Start("Subscribing to " + install.OperatorPackage)
	res.Changes, err = newApplier(c).Apply(ctx, objs)
	if err != nil {
		step.Fail("failed")
		return err
	}
	step.Done(apply.Tally(res.Changes, "nothing to change", apply.Label{Action: apply.Created, Text: "created"},
		apply.Label{Action: apply.Updated, Text: "updated"}, apply.Label{Action: apply.Unchanged, Text: "unchanged"}))
	if installOpts.noWait {
		return nil
	}
	step = prog.Start("Waiting for the operator to install")
	res.CSV, err = install.WaitForCSV(ctx, c.Dyn, opts, waitFor(defaultInstallTimeout), step.Update)
	if err != nil {
		step.Fail("failed")
		return err
	}
	step.Done(res.CSV + " succeeded")
	return nil
}

func installManifest(cmd *cobra.Command, c *kube.Clients, res *installResult) error {
	ctx := cmd.Context()
	prog := progress(cmd)
	source := installOpts.manifest
	res.Version = installOpts.version
	if source == "" {
		if res.Version == "" {
			res.Version = install.DefaultVersion
		}
		source = install.ManifestURL(res.Version)
	}
	step := prog.Start("Fetching " + source)
	b, err := install.FetchManifest(ctx, httpClient, source)
	if err != nil {
		step.Fail("failed")
		return err
	}
	objs, err := install.ParseManifest(b)
	if err != nil {
		step.Fail("failed")
		return err
	}
	step.Done(fmt.Sprintf("%d objects", len(objs)))
	if installOpts.dryRun {
		return printObjects(cmd.OutOrStdout(), objs)
	}
	step = prog.Start("Applying cert-manager")
	res.Changes, err = newApplier(c).Apply(ctx, objs)
	if err != nil {
		step.Fail("failed")
		return err
	}
	step.Done(apply.Tally(res.Changes, "nothing to change", apply.Label{Action: apply.Created, Text: "created"},
		apply.Label{Action: apply.Updated, Text: "updated"}, apply.Label{Action: apply.Unchanged, Text: "unchanged"}))
	return nil
}

// waitReady waits for the deployments and then for the API to accept objects.
func waitReady(cmd *cobra.Command, c *kube.Clients, prog *ui.Progress) error {
	ctx := cmd.Context()
	t := waitFor(defaultInstallTimeout)
	step := prog.Start("Waiting for the cert-manager deployments")
	if err := install.WaitForOperands(ctx, c.Kube, t, step.Update); err != nil {
		step.Fail("failed")
		return err
	}
	step.Done("cert-manager, webhook and cainjector available")
	step = prog.Start("Checking the cert-manager API")
	if err := install.CheckAPI(ctx, c.Dyn, t, step.Update); err != nil {
		step.Fail("failed")
		return err
	}
	step.Done("webhook is serving")
	return nil
}

func printInstall(cmd *cobra.Command, res *installResult) error {
	w := cmd.OutOrStdout()
	if structured() {
		return printStructured(w, res)
	}
	p := palette(w)
	what := "cert-manager"
	switch {
	case res.CSV != "":
		what = res.CSV
	case res.Version != "":
		what += " " + res.Version
	}
	state := "is ready"
	if !res.Ready {
		state = "is applied; not waiting for it (check with: cmo status)"
	}
	fmt.Fprintf(w, "%s %s %s (%s, %s)\n", p.Icon("✓"), p.Bold(what), state, res.Platform, res.Method)
	if res.Ready {
		fmt.Fprintf(w, "Next: cmo issuer create ca my-ca --generate\n")
	}
	return nil
}
