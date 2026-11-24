package cmd

import (
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/sebrandon1/cmo-cli/lib/apply"
	"github.com/sebrandon1/cmo-cli/lib/cert"
	"github.com/sebrandon1/cmo-cli/lib/issuer"
	"github.com/sebrandon1/cmo-cli/lib/kube"
	"github.com/sebrandon1/cmo-cli/lib/ui"
)

const issuerScope = `Without -n an issuer is a ClusterIssuer, usable from every namespace. With
-n it is an Issuer in that namespace.`

var issuerCmd = &cobra.Command{
	Use:     "issuer",
	Aliases: []string{"issuers", "clusterissuer", "clusterissuers"},
	Short:   "Create, list, show and delete issuers and cluster issuers",
	Long:    "Issuers sign certificates. " + issuerScope,
	RunE:    groupRunE,
}

var issuerCreateCmd = &cobra.Command{
	Use:   "create",
	Short: "Create a self-signed, CA or ACME issuer",
	Long:  "Create an issuer and wait for it to be Ready.\n\n" + issuerScope,
	RunE:  groupRunE,
}

var issuerCreateOpts struct {
	noWait bool
	dryRun bool
	// ca
	secret     string
	generate   bool
	commonName string
	duration   time.Duration
	// acme
	acme issuer.ACMEOptions
}

var issuerCreateSelfSignedCmd = &cobra.Command{
	Use:   "selfsigned NAME",
	Short: "Create an issuer whose certificates sign themselves",
	Example: `  cmo issuer create selfsigned selfsigned
  cmo issuer create selfsigned local -n team-a`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		r := issuer.NewRef(args[0], namespace)
		u, err := issuer.SelfSigned(r)
		if err != nil {
			return err
		}
		return createIssuer(cmd, r, u)
	},
}

var issuerCreateCACmd = &cobra.Command{
	Use:   "ca NAME",
	Short: "Create a CA issuer, from an existing key pair or a newly generated root",
	Long: `Create an issuer that signs with a CA key pair.

--secret uses a kubernetes.io/tls Secret you already have (for a ClusterIssuer
it must be in the cert-manager namespace).

--generate makes a new root CA: a self-signed bootstrap issuer NAME-selfsigned,
a 10-year ECDSA P-256 root CA Certificate NAME-root-ca, and the CA issuer NAME.
cmo issuer delete NAME removes all three.`,
	Example: `  cmo issuer create ca my-ca --generate
  cmo issuer create ca my-ca --generate --common-name "Lab Root CA"
  cmo issuer create ca corp-ca --secret corp-ca-keypair`,
	Args: cobra.ExactArgs(1),
	RunE: runCreateCA,
}

var issuerCreateACMECmd = &cobra.Command{
	Use:   "acme NAME",
	Short: "Create an ACME issuer (Let's Encrypt or any ACME server)",
	Long: `Create an ACME issuer with one solver: HTTP-01 through an Ingress class, or
DNS-01 through RFC2136 dynamic DNS updates.

--server takes a URL, or letsencrypt or letsencrypt-staging.`,
	Example: `  cmo issuer create acme le-staging --server letsencrypt-staging --email me@example.com --http01-ingress-class nginx
  cmo issuer create acme pebble --server https://pebble.pebble.svc:14000/dir --skip-tls-verify \
      --email test@example.com --http01-ingress-class openshift-default
  cmo issuer create acme dns --server letsencrypt --email me@example.com \
      --dns01-rfc2136-nameserver 10.0.0.53:53 --dns01-rfc2136-tsig-key-name cmo \
      --dns01-rfc2136-tsig-secret tsig-key`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		r := issuer.NewRef(args[0], namespace)
		u, err := issuer.ACME(r, issuerCreateOpts.acme)
		if err != nil {
			return err
		}
		return createIssuer(cmd, r, u)
	},
}

var issuerListCmd = &cobra.Command{
	Use:     "list",
	Aliases: []string{"ls"},
	Short:   "List cluster issuers and issuers (all namespaces, or -n)",
	Args:    cobra.NoArgs,
	RunE:    runIssuerList,
}

var issuerGetCmd = &cobra.Command{
	Use:   "get NAME",
	Short: "Show one issuer (-o yaml for the whole object)",
	Args:  cobra.ExactArgs(1),
	RunE:  runIssuerGet,
}

var issuerDeleteOpts struct{ allManaged bool }

var issuerDeleteCmd = &cobra.Command{
	Use:     "delete [NAME...]",
	Aliases: []string{"rm"},
	Short:   "Delete issuers (a generated CA takes its root and bootstrap issuer with it)",
	Example: `  cmo issuer delete my-ca
  cmo issuer delete local -n team-a
  cmo issuer delete --all-managed     # every issuer cmo created`,
	RunE: runIssuerDelete,
}

func init() {
	for _, c := range []*cobra.Command{issuerCreateSelfSignedCmd, issuerCreateCACmd, issuerCreateACMECmd} {
		c.Flags().BoolVar(&issuerCreateOpts.noWait, "no-wait", false, "don't wait for the issuer to be Ready")
		c.Flags().BoolVar(&issuerCreateOpts.dryRun, "dry-run", false, "print the objects that would be created, and change nothing")
		issuerCreateCmd.AddCommand(c)
	}
	f := issuerCreateCACmd.Flags()
	f.StringVar(&issuerCreateOpts.secret, "secret", "", "existing TLS Secret holding the CA key pair")
	f.BoolVar(&issuerCreateOpts.generate, "generate", false, "generate a new self-signed root CA")
	f.StringVar(&issuerCreateOpts.commonName, "common-name", "", "common name of the generated root (default NAME-root-ca)")
	f.DurationVar(&issuerCreateOpts.duration, "duration", 0, "lifetime of the generated root (default 87600h, 10 years)")
	issuerCreateCACmd.MarkFlagsMutuallyExclusive("secret", "generate")
	issuerCreateCACmd.MarkFlagsOneRequired("secret", "generate")

	a := &issuerCreateOpts.acme
	f = issuerCreateACMECmd.Flags()
	f.StringVar(&a.Server, "server", "", "ACME directory URL, or letsencrypt / letsencrypt-staging")
	f.StringVar(&a.Email, "email", "", "account email for expiry notices")
	f.BoolVar(&a.SkipTLSVerify, "skip-tls-verify", false, "don't verify the ACME server's TLS certificate (test servers such as Pebble)")
	f.StringVar(&a.HTTP01IngressClass, "http01-ingress-class", "", "solve HTTP-01 through an Ingress of this class (nginx, openshift-default, ...)")
	f.StringVar(&a.RFC2136.Nameserver, "dns01-rfc2136-nameserver", "", "solve DNS-01 by RFC2136 update to this server (host:port)")
	f.StringVar(&a.RFC2136.TSIGKeyName, "dns01-rfc2136-tsig-key-name", "", "TSIG key name")
	f.StringVar(&a.RFC2136.TSIGAlgorithm, "dns01-rfc2136-tsig-algorithm", "HMACSHA256", "TSIG algorithm")
	f.StringVar(&a.RFC2136.TSIGSecret, "dns01-rfc2136-tsig-secret", "", "Secret holding the TSIG key")
	f.StringVar(&a.RFC2136.TSIGSecretKey, "dns01-rfc2136-tsig-secret-key", "tsig-secret", "key in that Secret")
	_ = issuerCreateACMECmd.MarkFlagRequired("server")

	issuerDeleteCmd.Flags().BoolVar(&issuerDeleteOpts.allManaged, "all-managed", false, "delete every issuer cmo created (in -n, or everywhere)")

	issuerCmd.AddCommand(issuerCreateCmd, issuerListCmd, issuerGetCmd, issuerDeleteCmd)
	rootCmd.AddCommand(issuerCmd)
}

// createIssuer creates u (or prints it for --dry-run) and waits for it.
func createIssuer(cmd *cobra.Command, r issuer.Ref, u *unstructured.Unstructured) error {
	if issuerCreateOpts.dryRun {
		return printObjects(cmd.OutOrStdout(), []*unstructured.Unstructured{u})
	}
	c, err := newClients()
	if err != nil {
		return err
	}
	if err := issuer.Create(cmd.Context(), c.Dyn, u); err != nil {
		return err
	}
	return finishIssuer(cmd, c, r, false)
}

// finishIssuer waits for a new issuer, unless it is known to be ready or
// --no-wait, and reports it.
func finishIssuer(cmd *cobra.Command, c *kube.Clients, r issuer.Ref, ready bool) error {
	if !ready && !issuerCreateOpts.noWait {
		step := progress(cmd).Start("Waiting for " + r.String())
		if err := issuer.WaitReady(cmd.Context(), c.Dyn, r, waitFor(defaultWaitTimeout), step.Update); err != nil {
			step.Fail("failed")
			return err
		}
		step.Done("ready")
		ready = true
	}
	w := cmd.OutOrStdout()
	if structured() {
		return printStructured(w, map[string]any{"issuer": r, "ready": ready})
	}
	p := palette(w)
	fmt.Fprintf(w, "%s %s created\n", p.Icon("✓"), p.Bold(r.String()))
	use := "cmo cert create NAME --issuer " + r.Name + " --dns NAME.example.com"
	if r.Kind == issuer.KindIssuer {
		use += " -n " + r.Namespace
	}
	fmt.Fprintf(w, "Use it: %s\n", use)
	return nil
}

func runCreateCA(cmd *cobra.Command, args []string) error {
	r := issuer.NewRef(args[0], namespace)
	o := &issuerCreateOpts
	if !o.generate {
		if cmd.Flags().Changed("common-name") || cmd.Flags().Changed("duration") {
			return errors.New("--common-name and --duration only apply with --generate")
		}
		u, err := issuer.CA(r, o.secret)
		if err != nil {
			return err
		}
		return createIssuer(cmd, r, u)
	}
	ca := issuer.CAOptions{CommonName: o.commonName, Duration: o.duration}
	if o.duration != 0 {
		ca.RenewBefore = o.duration / 3
	}
	if o.dryRun {
		return printCAPlan(cmd.OutOrStdout(), r, ca)
	}
	c, err := newClients()
	if err != nil {
		return err
	}
	step := progress(cmd).Start("Generating a root CA")
	if err := issuer.GenerateCA(cmd.Context(), c.Dyn, r, ca, waitFor(defaultWaitTimeout), step.Update); err != nil {
		step.Fail("failed")
		return err
	}
	p := issuer.PlanCA(r)
	step.Done(fmt.Sprintf("root %s/%s signed by %s", p.CertNamespace, p.CertName, p.Bootstrap))
	return finishIssuer(cmd, c, r, true) // GenerateCA waited for each step
}

// printCAPlan prints the objects --generate would create.
func printCAPlan(w io.Writer, r issuer.Ref, o issuer.CAOptions) error {
	p := issuer.PlanCA(r)
	boot, err := issuer.SelfSigned(p.Bootstrap)
	if err != nil {
		return err
	}
	co, err := p.RootCertificate(o)
	if err != nil {
		return err
	}
	root, err := cert.Build(co)
	if err != nil {
		return err
	}
	ca, err := issuer.CA(r, p.CertName)
	if err != nil {
		return err
	}
	return printObjects(w, []*unstructured.Unstructured{boot, root, ca})
}

func runIssuerList(cmd *cobra.Command, _ []string) error {
	c, err := newClients()
	if err != nil {
		return err
	}
	list, err := issuer.List(cmd.Context(), c.Dyn, namespace, "")
	if err != nil {
		return err
	}
	w := cmd.OutOrStdout()
	if structured() {
		return printStructured(w, list)
	}
	if len(list) == 0 {
		fmt.Fprintln(w, "No issuers. Create one with: cmo issuer create selfsigned NAME  (or: ca NAME --generate)")
		return nil
	}
	p := palette(w)
	t := &ui.Table{}
	head := []string{"KIND", "NAME", "NAMESPACE", "TYPE", "READY", "AGE"}
	if outputFormat == formatWide {
		head = append(head, "MANAGED", "MESSAGE")
	}
	t.Row(head...)
	for _, s := range list {
		row := []string{s.Kind, s.Name, dash(s.Namespace), s.Type, readyCell(p, s.Ready), age(s.Created)}
		if outputFormat == formatWide {
			row = append(row, fmt.Sprint(s.Managed), dash(s.Message))
		}
		t.Row(row...)
	}
	t.Write(w)
	return nil
}

func runIssuerGet(cmd *cobra.Command, args []string) error {
	c, err := newClients()
	if err != nil {
		return err
	}
	r := issuer.NewRef(args[0], namespace)
	u, err := issuer.Get(cmd.Context(), c.Dyn, r)
	if err != nil {
		return err
	}
	w := cmd.OutOrStdout()
	if structured() {
		return printStructured(w, withoutManagedFields(u))
	}
	u.SetKind(r.Kind)
	s := issuer.Summarize(u)
	p := palette(w)
	t := &ui.Table{}
	t.Row(p.Bold("Name"), s.Name)
	t.Row(p.Bold("Kind"), s.Kind)
	if s.Namespace != "" {
		t.Row(p.Bold("Namespace"), s.Namespace)
	}
	t.Row(p.Bold("Type"), s.Type)
	t.Row(p.Bold("Ready"), readyCell(p, s.Ready))
	if s.Message != "" {
		t.Row(p.Bold("Message"), s.Message)
	}
	switch s.Type {
	case issuer.TypeCA:
		secret, _, _ := unstructured.NestedString(u.Object, "spec", "ca", "secretName")
		t.Row(p.Bold("Secret"), secret)
	case issuer.TypeACME:
		server, _, _ := unstructured.NestedString(u.Object, "spec", "acme", "server")
		email, _, _ := unstructured.NestedString(u.Object, "spec", "acme", "email")
		t.Row(p.Bold("Server"), server)
		t.Row(p.Bold("Email"), dash(email))
	}
	if gen := u.GetAnnotations()[issuer.AnnCACertificate]; gen != "" {
		t.Row(p.Bold("Generated root"), gen)
	}
	t.Row(p.Bold("Managed by cmo"), fmt.Sprint(s.Managed))
	t.Row(p.Bold("Age"), age(s.Created))
	t.Write(w)
	return nil
}

func runIssuerDelete(cmd *cobra.Command, args []string) error {
	if issuerDeleteOpts.allManaged == (len(args) > 0) {
		return errors.New("name the issuers to delete, or pass --all-managed")
	}
	c, err := newClients()
	if err != nil {
		return err
	}
	ctx := cmd.Context()
	var changes []apply.Change
	if issuerDeleteOpts.allManaged {
		list, err := issuer.List(ctx, c.Dyn, namespace, kube.ManagedSelector)
		if err != nil {
			return err
		}
		if len(list) == 0 {
			fmt.Fprintln(cmd.OutOrStdout(), "No issuers made by cmo")
			return nil
		}
		if err := confirm(cmd, fmt.Sprintf("Delete %d issuers made by cmo?", len(list))); err != nil {
			return err
		}
		changes, err = issuer.DeleteManaged(ctx, c.Dyn, c.Kube, namespace)
		if err != nil {
			return err
		}
	} else {
		for _, name := range args {
			r := issuer.NewRef(name, namespace)
			if _, err := issuer.Get(ctx, c.Dyn, r); err != nil {
				return err
			}
			ch, err := issuer.Delete(ctx, c.Dyn, c.Kube, r)
			if err != nil {
				return err
			}
			changes = append(changes, ch...)
		}
	}
	w := cmd.OutOrStdout()
	if structured() {
		return printStructured(w, changes)
	}
	printChanges(w, palette(w), onlyRemoved(changes))
	return nil
}
