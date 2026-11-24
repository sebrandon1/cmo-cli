// Package cmd implements the cmo command line.
package cmd

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/sebrandon1/go-toolbox/cliout"
	"github.com/sebrandon1/go-toolbox/cobrautil"
	"github.com/spf13/cobra"

	"github.com/sebrandon1/cmo-cli/lib/trace"
)

// Output formats for -o.
const (
	formatTable = "table"
	formatWide  = "wide"
	formatJSON  = "json"
	formatYAML  = "yaml"
)

var (
	kubeconfig   string
	kubeContext  string
	namespace    string
	outputFormat string
	noColor      bool
	verbose      bool
	assumeYes    bool
	timeout      time.Duration
)

var rootCmd = &cobra.Command{
	Use:   "cmo",
	Short: "Install cert-manager and manage its issuers and certificates",
	Long: `cmo (Cert Manager Operator CLI) installs and removes cert-manager, and creates,
lists, inspects, renews and deletes its issuers and certificates.

On OpenShift it installs the Red Hat cert-manager Operator through OLM. On any
other Kubernetes cluster it applies the upstream cert-manager release manifest.
It picks the right one by itself.

Get started:
  cmo install                                   install cert-manager
  cmo issuer create ca my-ca --generate         make a CA to sign with
  cmo cert create web --issuer my-ca --dns web.example.com
  cmo cert inspect web                          see what was issued
  cmo status                                    check on everything`,
	SilenceUsage: true,
	PersistentPreRunE: func(cmd *cobra.Command, _ []string) error {
		if noColor {
			if err := os.Setenv("NO_COLOR", "1"); err != nil {
				return err
			}
		}
		if err := cliout.ValidateFormat(outputFormat, formatTable, formatWide, formatJSON, formatYAML); err != nil {
			return err
		}
		outputFormat = strings.ToLower(outputFormat)
		if err := checkWide(cmd); err != nil {
			return err
		}
		if verbose {
			trace.Enable(cmd.ErrOrStderr())
		} else {
			trace.Disable()
		}
		return nil
	},
}

func init() {
	pf := rootCmd.PersistentFlags()
	pf.StringVar(&kubeconfig, "kubeconfig", "", "path to the kubeconfig (default: $KUBECONFIG, then ~/.kube/config)")
	pf.StringVar(&kubeContext, "context", "", "kubeconfig context to use")
	pf.StringVarP(&namespace, "namespace", "n", "", "namespace (certificates: default is the context's namespace; issuers: set it for a namespaced Issuer, leave it out for a ClusterIssuer)")
	pf.StringVarP(&outputFormat, "output", "o", formatTable, "output format: table, wide, json or yaml")
	pf.BoolVar(&noColor, "no-color", false, "don't color the output (also: NO_COLOR=1)")
	pf.BoolVarP(&verbose, "verbose", "v", false, "trace the cluster API requests on stderr")
	pf.BoolVarP(&assumeYes, "yes", "y", false, "don't ask for confirmation")
	pf.DurationVar(&timeout, "timeout", 0, "how long to wait for things to become ready (default depends on the command)")

	rootCmd.AddCommand(cobrautil.NewCompletionCommand(rootCmd, "cmo"))
}

// commandsWithWide have columns beyond the default view.
var commandsWithWide = map[string]bool{
	"cmo cert list":   true,
	"cmo issuer list": true,
}

// checkWide refuses -o wide on a command that has no wide view, rather than
// silently printing the table.
func checkWide(cmd *cobra.Command) error {
	if outputFormat != formatWide || commandsWithWide[cmd.CommandPath()] {
		return nil
	}
	return fmt.Errorf("%s has no wide output; use -o table, json or yaml", cmd.CommandPath())
}

// SetVersion is called from main with the build-time version.
func SetVersion(v string) {
	cobrautil.SetVersion(rootCmd, v)
}

// Execute runs the root command. Ctrl+C or SIGTERM cancels the command's
// context, so a wait stops promptly.
func Execute() error {
	defer trace.Flush()
	return cobrautil.Execute(rootCmd)
}
