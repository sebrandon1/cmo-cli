package cmd

import (
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/sebrandon1/cmo-cli/lib/apply"
	"github.com/sebrandon1/cmo-cli/lib/cert"
	"github.com/sebrandon1/cmo-cli/lib/issuer"
	"github.com/sebrandon1/cmo-cli/lib/kube"
	"github.com/sebrandon1/cmo-cli/lib/ui"
	"github.com/sebrandon1/cmo-cli/lib/wait"
)

var certCmd = &cobra.Command{
	Use:     "cert",
	Aliases: []string{"certs", "certificate", "certificates"},
	Short:   "Create, list, inspect, renew and delete certificates",
	Long: `Certificates are issued by an issuer into a Secret (NAME-tls by default).
They live in a namespace: -n, or the kubeconfig context's namespace.`,
	RunE: groupRunE,
}

var certCreateOpts struct {
	opts          cert.Options
	issuer        string
	clusterIssuer bool
	noWait        bool
	dryRun        bool
}

var certCreateCmd = &cobra.Command{
	Use:   "create NAME",
	Short: "Request a certificate and wait for it to be issued",
	Long: `Request a certificate from an issuer and wait until it is issued.

--issuer names an Issuer in the certificate's namespace or, if there is none
by that name, a ClusterIssuer. --cluster-issuer skips the Issuer lookup.`,
	Example: `  cmo cert create web --issuer my-ca --dns web.example.com
  cmo cert create api --issuer my-ca --dns api.example.com,api.internal --ip 10.0.0.10 -n team-a
  cmo cert create mtls --issuer my-ca --common-name client-1 --key-algorithm ecdsa --key-size 384
  cmo cert create short --issuer my-ca --dns s.example.com --duration 2h --renew-before 1h
  cmo cert create web --issuer my-ca --dns web.example.com --dry-run`,
	Args: cobra.ExactArgs(1),
	RunE: runCertCreate,
}

var certListOpts struct {
	allNamespaces bool
	managed       bool
}

var certListCmd = &cobra.Command{
	Use:     "list",
	Aliases: []string{"ls"},
	Short:   "List certificates",
	Example: `  cmo cert list
  cmo cert list -A
  cmo cert list -A --managed -o wide`,
	Args: cobra.NoArgs,
	RunE: runCertList,
}

var certGetCmd = &cobra.Command{
	Use:   "get NAME",
	Short: "Show a certificate's spec and status, and why it isn't ready (-o yaml for the whole object)",
	Args:  cobra.ExactArgs(1),
	RunE:  runCertGet,
}

var certDeleteOpts struct {
	keepSecret bool
	allManaged bool
}

var certDeleteCmd = &cobra.Command{
	Use:     "delete [NAME...]",
	Aliases: []string{"rm"},
	Short:   "Delete certificates and the Secrets they wrote",
	Example: `  cmo cert delete web
  cmo cert delete web --keep-secret
  cmo cert delete --all-managed -A    # every certificate cmo created`,
	RunE: runCertDelete,
}

var certRenewOpts struct{ noWait bool }

var certRenewCmd = &cobra.Command{
	Use:   "renew NAME",
	Short: "Reissue a certificate now",
	Args:  cobra.ExactArgs(1),
	RunE:  runCertRenew,
}

var certInspectCmd = &cobra.Command{
	Use:   "inspect NAME",
	Short: "Decode the issued certificate: subject, SANs, validity, key and serial",
	Args:  cobra.ExactArgs(1),
	RunE:  runCertInspect,
}

func init() {
	o := &certCreateOpts.opts
	f := certCreateCmd.Flags()
	f.StringVar(&certCreateOpts.issuer, "issuer", "", "issuer to sign with (an Issuer in the namespace, else a ClusterIssuer)")
	f.BoolVar(&certCreateOpts.clusterIssuer, "cluster-issuer", false, "--issuer is a ClusterIssuer even if an Issuer has the same name")
	f.StringSliceVar(&o.DNSNames, "dns", nil, "DNS names (comma separated, or repeat the flag)")
	f.StringSliceVar(&o.IPs, "ip", nil, "IP address SANs")
	f.StringSliceVar(&o.URIs, "uri", nil, "URI SANs, such as spiffe://cluster.local/ns/a/sa/b")
	f.StringVar(&o.CommonName, "common-name", "", "subject common name")
	f.DurationVar(&o.Duration, "duration", 0, "lifetime, such as 2160h (default: cert-manager's 90 days)")
	f.DurationVar(&o.RenewBefore, "renew-before", 0, "renew this long before expiry (default: cert-manager's, a third of the lifetime)")
	f.StringVar(&o.KeyAlgorithm, "key-algorithm", "", "rsa, ecdsa or ed25519 (default: cert-manager's, RSA 2048)")
	f.IntVar(&o.KeySize, "key-size", 0, "key size: rsa 2048/3072/4096, ecdsa 256/384/521")
	f.StringVar(&o.SecretName, "secret", "", "Secret to write the key pair to (default NAME-tls)")
	f.BoolVar(&o.IsCA, "ca", false, "issue a CA certificate")
	f.BoolVar(&certCreateOpts.noWait, "no-wait", false, "don't wait for the certificate to be issued")
	f.BoolVar(&certCreateOpts.dryRun, "dry-run", false, "print the Certificate that would be created, and change nothing")
	_ = certCreateCmd.MarkFlagRequired("issuer")

	certListCmd.Flags().BoolVarP(&certListOpts.allNamespaces, "all-namespaces", "A", false, "list certificates in every namespace")
	certListCmd.Flags().BoolVar(&certListOpts.managed, "managed", false, "only certificates cmo created")

	certDeleteCmd.Flags().BoolVar(&certDeleteOpts.keepSecret, "keep-secret", false, "leave the Secret with the key pair in place")
	certDeleteCmd.Flags().BoolVar(&certDeleteOpts.allManaged, "all-managed", false, "delete every certificate cmo created (in the namespace, or with -A everywhere)")
	certDeleteCmd.Flags().BoolVarP(&certListOpts.allNamespaces, "all-namespaces", "A", false, "with --all-managed, every namespace")

	certRenewCmd.Flags().BoolVar(&certRenewOpts.noWait, "no-wait", false, "don't wait for the new certificate")

	certCmd.AddCommand(certCreateCmd, certListCmd, certGetCmd, certDeleteCmd, certRenewCmd, certInspectCmd)
	rootCmd.AddCommand(certCmd)
}

func runCertCreate(cmd *cobra.Command, args []string) error {
	ctx := cmd.Context()
	o := certCreateOpts.opts
	o.Name = args[0]
	o.Issuer.Name = certCreateOpts.issuer
	if err := o.Validate(); err != nil {
		return err
	}
	c, err := newClients()
	if err != nil {
		return err
	}
	o.Namespace = certNamespace(c)
	ref, err := issuer.Resolve(ctx, c.Dyn, certCreateOpts.issuer, o.Namespace, certCreateOpts.clusterIssuer)
	if err != nil {
		return err
	}
	o.Issuer = ref
	u, err := cert.Build(&o)
	if err != nil {
		return err
	}
	if certCreateOpts.dryRun {
		return printObjects(cmd.OutOrStdout(), []*unstructured.Unstructured{u})
	}
	if err := cert.Create(ctx, c.Dyn, u); err != nil {
		return err
	}
	if !certCreateOpts.noWait {
		step := progress(cmd).Start(fmt.Sprintf("Issuing %s/%s from %s %s", o.Namespace, o.Name, ref.Kind, ref.Name))
		if err := cert.WaitReady(ctx, c.Dyn, o.Namespace, o.Name, 1, waitFor(defaultWaitTimeout), step.Update); err != nil {
			step.Fail("not issued")
			return err
		}
		step.Done("issued")
	}
	got, err := cert.Get(ctx, c.Dyn, o.Namespace, o.Name)
	if err != nil {
		return err
	}
	s := cert.Summarize(got)
	w := cmd.OutOrStdout()
	if structured() {
		return printStructured(w, s)
	}
	p := palette(w)
	if certCreateOpts.noWait {
		fmt.Fprintf(w, "%s certificate %s/%s requested (check with: cmo cert get %s -n %s)\n", p.Icon("✓"), s.Namespace, s.Name, s.Name, s.Namespace)
		return nil
	}
	fmt.Fprintf(w, "%s certificate %s/%s issued into secret %s, valid until %s\n", p.Icon("✓"), s.Namespace, s.Name, p.Bold(s.Secret), formatTime(s.NotAfter))
	fmt.Fprintf(w, "Inspect it: cmo cert inspect %s -n %s\n", s.Name, s.Namespace)
	return nil
}

func formatTime(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.Local().Format("2006-01-02 15:04 MST")
}

// timeAndUntil is "2027-02-22 13:22 CST (in 89d)", or "-" before issuance.
func timeAndUntil(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return fmt.Sprintf("%s (in %s)", formatTime(t), until(t))
}

func runCertList(cmd *cobra.Command, _ []string) error {
	c, err := newClients()
	if err != nil {
		return err
	}
	ns := certNamespace(c)
	if certListOpts.allNamespaces {
		ns = ""
	}
	selector := ""
	if certListOpts.managed {
		selector = kube.ManagedSelector
	}
	list, err := cert.List(cmd.Context(), c.Dyn, ns, selector)
	if err != nil {
		return err
	}
	w := cmd.OutOrStdout()
	if structured() {
		return printStructured(w, list)
	}
	if len(list) == 0 {
		where := "in namespace " + ns
		if ns == "" {
			where = "in any namespace"
		}
		fmt.Fprintf(w, "No certificates %s. Create one with: cmo cert create NAME --issuer ISSUER --dns NAME.example.com\n", where)
		return nil
	}
	p := palette(w)
	wide := outputFormat == formatWide
	t := &ui.Table{}
	head := []string{"NAMESPACE", "NAME", "READY", "SECRET", "ISSUER", "EXPIRES", "AGE"}
	if wide {
		head = append(head, "RENEWS", "DNS NAMES", "REASON")
	}
	t.Row(head...)
	for _, s := range list {
		row := []string{s.Namespace, s.Name, readyCell(p, s.Ready), s.Secret, issuerCell(s), until(s.NotAfter), age(s.Created)}
		if wide {
			row = append(row, until(s.RenewalTime), dash(strings.Join(s.DNSNames, ",")), dash(s.Reason))
		}
		t.Row(row...)
	}
	t.Write(w)
	return nil
}

func issuerCell(s cert.Summary) string {
	if s.IssuerKind == issuer.KindClusterIssuer {
		return s.Issuer + " (cluster)"
	}
	return s.Issuer
}

func runCertGet(cmd *cobra.Command, args []string) error {
	ctx := cmd.Context()
	c, err := newClients()
	if err != nil {
		return err
	}
	ns := certNamespace(c)
	u, err := cert.Get(ctx, c.Dyn, ns, args[0])
	if err != nil {
		return err
	}
	w := cmd.OutOrStdout()
	if structured() {
		return printStructured(w, withoutManagedFields(u))
	}
	s := cert.Summarize(u)
	p := palette(w)
	t := &ui.Table{}
	t.Row(p.Bold("Name"), s.Namespace+"/"+s.Name)
	t.Row(p.Bold("Ready"), readyCell(p, s.Ready))
	t.Row(p.Bold("Issuer"), s.IssuerKind+"/"+s.Issuer)
	t.Row(p.Bold("Secret"), s.Secret)
	cn, _, _ := unstructured.NestedString(u.Object, "spec", "commonName")
	if cn != "" {
		t.Row(p.Bold("Common name"), cn)
	}
	if len(s.DNSNames) > 0 {
		t.Row(p.Bold("DNS names"), strings.Join(s.DNSNames, ", "))
	}
	if ips, _, _ := unstructured.NestedStringSlice(u.Object, "spec", "ipAddresses"); len(ips) > 0 {
		t.Row(p.Bold("IP addresses"), strings.Join(ips, ", "))
	}
	t.Row(p.Bold("Expires"), timeAndUntil(s.NotAfter))
	t.Row(p.Bold("Renews"), timeAndUntil(s.RenewalTime))
	t.Row(p.Bold("Revision"), fmt.Sprint(cert.Revision(u)))
	t.Row(p.Bold("Managed by cmo"), fmt.Sprint(s.Managed))
	t.Row(p.Bold("Age"), age(s.Created))
	t.Write(w)
	if !wait.IsReady(u) {
		fmt.Fprintln(w, p.Bold("\nWhy it is not ready:"))
		for _, l := range cert.Diagnose(ctx, c.Dyn, ns, args[0]) {
			fmt.Fprintln(w, "  "+l)
		}
	}
	return nil
}

func runCertDelete(cmd *cobra.Command, args []string) error {
	if certDeleteOpts.allManaged == (len(args) > 0) {
		return errors.New("name the certificates to delete, or pass --all-managed")
	}
	if certListOpts.allNamespaces && !certDeleteOpts.allManaged {
		return errors.New("-A only applies with --all-managed")
	}
	ctx := cmd.Context()
	c, err := newClients()
	if err != nil {
		return err
	}
	ns := certNamespace(c)
	var targets []certTarget
	if certDeleteOpts.allManaged {
		if certListOpts.allNamespaces {
			ns = ""
		}
		if targets, err = managedCerts(cmd, c, ns); err != nil || len(targets) == 0 {
			return err
		}
	} else {
		for _, a := range args {
			targets = append(targets, certTarget{ns, a})
		}
	}
	var changes []apply.Change
	for _, t := range targets {
		ch, err := cert.Delete(ctx, c.Dyn, c.Kube, t.ns, t.name, certDeleteOpts.keepSecret)
		if err != nil {
			return err
		}
		changes = append(changes, ch...)
	}
	w := cmd.OutOrStdout()
	if structured() {
		return printStructured(w, changes)
	}
	printChanges(w, palette(w), onlyRemoved(changes))
	return nil
}

// certTarget is one certificate to delete.
type certTarget struct{ ns, name string }

// managedCerts are the certificates --all-managed deletes, once confirmed.
// Generated CA roots are left to their issuer.
func managedCerts(cmd *cobra.Command, c *kube.Clients, ns string) ([]certTarget, error) {
	ctx := cmd.Context()
	all, err := cert.List(ctx, c.Dyn, ns, kube.ManagedSelector)
	if err != nil {
		return nil, err
	}
	var list []cert.Summary
	roots := 0
	for _, s := range all {
		if s.CARoot {
			roots++
		} else {
			list = append(list, s)
		}
	}
	if roots > 0 {
		fmt.Fprintf(cmd.ErrOrStderr(), "Keeping %s of generated CAs; they go with their issuer (cmo issuer delete).\n", plural(roots, "root certificate"))
	}
	if len(list) == 0 {
		fmt.Fprintln(cmd.OutOrStdout(), "No certificates made by cmo")
		return nil, nil
	}
	if err := confirm(cmd, fmt.Sprintf("Delete %d certificates made by cmo, and their secrets?", len(list))); err != nil {
		return nil, err
	}
	out := make([]certTarget, 0, len(list))
	for _, s := range list {
		out = append(out, certTarget{s.Namespace, s.Name})
	}
	return out, nil
}

func runCertRenew(cmd *cobra.Command, args []string) error {
	ctx := cmd.Context()
	c, err := newClients()
	if err != nil {
		return err
	}
	ns := certNamespace(c)
	rev, err := cert.Renew(ctx, c.Dyn, ns, args[0])
	if err != nil {
		return err
	}
	w := cmd.OutOrStdout()
	p := palette(w)
	if !certRenewOpts.noWait {
		step := progress(cmd).Start(fmt.Sprintf("Renewing %s/%s", ns, args[0]))
		if err := cert.WaitReady(ctx, c.Dyn, ns, args[0], rev+1, waitFor(defaultWaitTimeout), step.Update); err != nil {
			step.Fail("not renewed")
			return err
		}
		step.Done(fmt.Sprintf("revision %d", rev+1))
	}
	u, err := cert.Get(ctx, c.Dyn, ns, args[0])
	if err != nil {
		return err
	}
	if structured() {
		return printStructured(w, map[string]any{"name": args[0], "namespace": ns, "previousRevision": rev, "revision": cert.Revision(u)})
	}
	if certRenewOpts.noWait {
		fmt.Fprintf(w, "%s renewal of %s/%s triggered\n", p.Icon("✓"), ns, args[0])
		return nil
	}
	fmt.Fprintf(w, "%s %s/%s renewed (revision %d), valid until %s\n", p.Icon("✓"), ns, args[0], cert.Revision(u), formatTime(cert.Summarize(u).NotAfter))
	return nil
}

func runCertInspect(cmd *cobra.Command, args []string) error {
	c, err := newClients()
	if err != nil {
		return err
	}
	ns := certNamespace(c)
	s, err := cert.ReadSecret(cmd.Context(), c.Dyn, c.Kube, ns, args[0])
	if err != nil {
		return err
	}
	info, err := cert.Inspect(s.Data["tls.crt"], s.Data["ca.crt"])
	if err != nil {
		return fmt.Errorf("secret %s: %w", s.Name, err)
	}
	w := cmd.OutOrStdout()
	if structured() {
		return printStructured(w, info)
	}
	printInspect(w, palette(w), s.Namespace+"/"+s.Name, &info)
	return nil
}

func printInspect(w io.Writer, p ui.Palette, secret string, i *cert.Info) {
	t := &ui.Table{}
	t.Row(p.Bold("Secret"), secret)
	t.Row(p.Bold("Subject"), dash(i.Subject))
	issuedBy := i.Issuer
	if i.SelfSigned {
		issuedBy += " (self-signed)"
	}
	t.Row(p.Bold("Issuer"), dash(issuedBy))
	t.Row(p.Bold("Serial"), i.Serial)
	t.Row(p.Bold("Not before"), formatTime(i.NotBefore))
	expiry := formatTime(i.NotAfter) + " (in " + until(i.NotAfter) + ")"
	if time.Until(i.NotAfter) < 0 {
		expiry = p.Red(formatTime(i.NotAfter) + " (expired)")
	}
	t.Row(p.Bold("Not after"), expiry)
	if len(i.DNSNames) > 0 {
		t.Row(p.Bold("DNS names"), strings.Join(i.DNSNames, ", "))
	}
	if len(i.IPAddresses) > 0 {
		t.Row(p.Bold("IP addresses"), strings.Join(i.IPAddresses, ", "))
	}
	if len(i.URIs) > 0 {
		t.Row(p.Bold("URIs"), strings.Join(i.URIs, ", "))
	}
	key := i.KeyAlgorithm
	if i.KeySize > 0 {
		key += fmt.Sprintf(" %d", i.KeySize)
	}
	t.Row(p.Bold("Key"), key)
	t.Row(p.Bold("CA"), fmt.Sprint(i.IsCA))
	t.Row(p.Bold("Chain"), fmt.Sprintf("%d certificate(s) in tls.crt; ca.crt present: %v", i.ChainLength, i.HasCA))
	t.Row(p.Bold("SHA-256"), i.SHA256)
	t.Write(w)
}
