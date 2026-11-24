package cmd

import (
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/sebrandon1/cmo-cli/lib/cluster"
	"github.com/sebrandon1/cmo-cli/lib/install"
	"github.com/sebrandon1/cmo-cli/lib/ui"
)

var statusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show whether cert-manager is installed and healthy",
	Example: `  cmo status
  cmo status -o json | jq .ready`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
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
		w := cmd.OutOrStdout()
		if structured() {
			return printStructured(w, st)
		}
		printStatus(w, palette(w), c.Context, &st)
		return nil
	},
}

func init() { rootCmd.AddCommand(statusCmd) }

func printStatus(w io.Writer, p ui.Palette, context string, st *install.Status) {
	t := &ui.Table{}
	info := cluster.Info{Platform: st.Platform, ServerVersion: st.ServerVersion, OpenShiftVersion: st.OpenShiftVersion}
	t.Row(p.Bold("Cluster"), fmt.Sprintf("%s, context %s", info.Describe(), dash(context)))
	version := dash(st.CertManagerVersion)
	if st.Method == install.MethodManifest {
		version = dash(st.Version)
	}
	switch {
	case !st.Installed:
		t.Row(p.Bold("cert-manager"), p.Yellow("not installed")+p.Dim("  (install it with: cmo install)"))
	case st.Ready:
		t.Row(p.Bold("cert-manager"), p.Green("ready")+fmt.Sprintf("  %s via %s", version, st.Method))
	default:
		t.Row(p.Bold("cert-manager"), p.Red("not ready")+fmt.Sprintf("  %s via %s  (see: cmo doctor)", version, st.Method))
	}
	if !st.Installed && st.CRDs {
		t.Row(p.Bold("Leftovers"), p.Yellow("cert-manager CRDs are still defined")+p.Dim("  (remove them with: cmo uninstall --purge)"))
	}
	if st.Method == install.MethodOLM {
		t.Row(p.Bold("Operator"), fmt.Sprintf("%s  %s  channel %s", dash(st.CSV), dash(st.CSVPhase), dash(st.Channel)))
	}
	if st.Installed {
		for i, d := range st.Deployments {
			label := ""
			if i == 0 {
				label = p.Bold("Deployments")
			}
			icon := "✓"
			switch {
			case !d.Found:
				icon = "✗"
			case !d.Available:
				icon = "!"
			}
			t.Row(label, fmt.Sprintf("%s %s %d/%d", p.Icon(icon), d.Name, d.Ready, d.Desired))
		}
	}
	if st.APIs {
		r := st.Resources
		t.Row(p.Bold("Resources"), fmt.Sprintf("%s, %s, %s (%d ready); %d made by cmo",
			plural(r.ClusterIssuers, "cluster issuer"), plural(r.Issuers, "issuer"), plural(r.Certificates, "certificate"),
			r.CertificatesReady, r.Managed))
	}
	t.Write(w)
}
