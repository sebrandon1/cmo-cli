package cmd

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/sebrandon1/go-toolbox/cliout"
	"github.com/spf13/cobra"
	"golang.org/x/term"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"

	"github.com/sebrandon1/cmo-cli/lib/apply"
	"github.com/sebrandon1/cmo-cli/lib/kube"
	"github.com/sebrandon1/cmo-cli/lib/ui"
)

// Seams for tests: the clients, the applier, the manifest HTTP client and
// whether stdin is a terminal.
var (
	newClients = func() (*kube.Clients, error) { return kube.New(kubeconfig, kubeContext) }
	newApplier = func(c *kube.Clients) applier { return apply.NewSSA(c.Kube.Discovery(), c.Dyn) }
	httpClient = &http.Client{Timeout: 2 * time.Minute}
	stdinIsTTY = func() bool { return term.IsTerminal(int(os.Stdin.Fd())) } // #nosec G115 -- fd fits in int
)

// applier applies and deletes manifests.
type applier interface {
	apply.Applier
	apply.Deleter
}

// Default waits, when --timeout is not given.
const (
	defaultInstallTimeout = 10 * time.Minute
	defaultWaitTimeout    = 3 * time.Minute
)

// waitFor is --timeout, or def when it is not set.
func waitFor(def time.Duration) time.Duration {
	if timeout > 0 {
		return timeout
	}
	return def
}

// certNamespace is -n, or the kubeconfig context's namespace.
func certNamespace(c *kube.Clients) string {
	if namespace != "" {
		return namespace
	}
	return c.Namespace
}

// structured reports whether -o asks for json or yaml.
func structured() bool { return outputFormat == formatJSON || outputFormat == formatYAML }

// progress shows steps on stderr, except for structured output.
func progress(cmd *cobra.Command) *ui.Progress { return ui.New(cmd.ErrOrStderr(), !structured()) }

// palette colors table output written to a terminal.
func palette(w io.Writer) ui.Palette {
	return ui.Palette{On: !structured() && ui.ColorEnabled(w)}
}

// printStructured writes v as json or yaml.
func printStructured(w io.Writer, v any) error {
	if outputFormat == formatYAML {
		b, err := yaml.Marshal(v)
		if err != nil {
			return err
		}
		_, err = w.Write(b)
		return err
	}
	return cliout.PrintJSON(w, v)
}

// withoutManagedFields is the object as kubectl get shows it: no
// managedFields, which only matter to the API server.
func withoutManagedFields(u *unstructured.Unstructured) map[string]any {
	c := u.DeepCopy()
	unstructured.RemoveNestedField(c.Object, "metadata", "managedFields")
	return c.Object
}

// printObjects writes objects as a YAML stream (or a JSON list with -o json),
// as a dry run shows them.
func printObjects(w io.Writer, objs []*unstructured.Unstructured) error {
	if outputFormat == formatJSON {
		items := make([]map[string]any, len(objs))
		for i, o := range objs {
			items[i] = o.Object
		}
		return cliout.PrintJSON(w, items)
	}
	for i, o := range objs {
		if i > 0 {
			if _, err := io.WriteString(w, "---\n"); err != nil {
				return err
			}
		}
		b, err := yaml.Marshal(o.Object)
		if err != nil {
			return err
		}
		if _, err := w.Write(b); err != nil {
			return err
		}
	}
	return nil
}

// confirm asks before something destructive. --yes skips it; without a
// terminal to ask on, it refuses rather than guess.
func confirm(cmd *cobra.Command, question string) error {
	if assumeYes {
		return nil
	}
	if !stdinIsTTY() {
		return fmt.Errorf("%s: pass --yes to confirm (stdin is not a terminal)", strings.TrimSuffix(question, "?"))
	}
	fmt.Fprintf(cmd.ErrOrStderr(), "%s [y/N] ", question)
	line, _ := bufio.NewReader(cmd.InOrStdin()).ReadString('\n')
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return nil
	}
	return errors.New("canceled")
}

// age renders how long ago t was, kubectl style: 45s, 12m, 3h, 5d.
func age(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return shortDuration(time.Since(t))
}

// until renders how long until t: 89d, or "expired".
func until(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	d := time.Until(t)
	if d < 0 {
		return "expired"
	}
	return shortDuration(d)
}

func shortDuration(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}

// dash is s, or "-" when empty, for table cells.
func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// readyCell colors a Ready cell.
func readyCell(p ui.Palette, s string) string {
	switch metav1.ConditionStatus(s) {
	case metav1.ConditionTrue:
		return p.Green(s)
	case metav1.ConditionFalse:
		return p.Red(s)
	}
	return p.Yellow(s)
}

// printChanges lists what a command changed, one per line, to w.
func printChanges(w io.Writer, p ui.Palette, changes []apply.Change) {
	for _, c := range changes {
		icon := "✓"
		if c.Action == apply.Absent || c.Action == apply.Unchanged {
			icon = "·"
		}
		fmt.Fprintf(w, "  %s %s %s\n", p.Icon(icon), c.Ref(), p.Dim(string(c.Action)))
	}
}

// groupRunE makes a command that only groups others refuse an unknown
// subcommand, rather than print help and succeed.
func groupRunE(cmd *cobra.Command, args []string) error {
	if len(args) > 0 {
		return fmt.Errorf("unknown command %q for %q; see %s --help", args[0], cmd.CommandPath(), cmd.CommandPath())
	}
	return cmd.Help()
}

// plural is "1 issuer" or "2 issuers".
func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}
