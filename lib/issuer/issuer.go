// Package issuer builds, lists, waits on and deletes cert-manager Issuers and
// ClusterIssuers.
package issuer

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	acmev1 "github.com/cert-manager/cert-manager/pkg/apis/acme/v1"
	cmapi "github.com/cert-manager/cert-manager/pkg/apis/certmanager/v1"
	cmmeta "github.com/cert-manager/cert-manager/pkg/apis/meta/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/dynamic"

	"github.com/sebrandon1/cmo-cli/lib/apply"
	"github.com/sebrandon1/cmo-cli/lib/cert"
	"github.com/sebrandon1/cmo-cli/lib/kube"
	"github.com/sebrandon1/cmo-cli/lib/wait"
)

// Kinds of issuer.
const (
	KindIssuer        = "Issuer"
	KindClusterIssuer = "ClusterIssuer"
)

// Issuer types, as cmo names them.
const (
	TypeSelfSigned = "selfsigned"
	TypeCA         = "ca"
	TypeACME       = "acme"
	TypeVault      = "vault"
	TypeVenafi     = "venafi"
	TypeExternal   = "external"
)

// Annotations on a CA issuer cmo generated, naming what it generated with it,
// so deleting the issuer cleans them up.
const (
	AnnCACertificate = "cmo-cli/ca-certificate"
	AnnCABootstrap   = "cmo-cli/ca-bootstrap-issuer"
)

// Ref names one issuer. Namespace is empty for a ClusterIssuer.
type Ref struct {
	Kind      string `json:"kind"`
	Name      string `json:"name"`
	Namespace string `json:"namespace,omitempty"`
}

// NewRef is a ClusterIssuer when ns is empty, an Issuer in ns otherwise.
func NewRef(name, ns string) Ref {
	if ns == "" {
		return Ref{Kind: KindClusterIssuer, Name: name}
	}
	return Ref{Kind: KindIssuer, Name: name, Namespace: ns}
}

func (r Ref) String() string {
	if r.Kind == KindClusterIssuer {
		return "ClusterIssuer/" + r.Name
	}
	return r.Namespace + "/Issuer/" + r.Name
}

func (r Ref) resource(dyn dynamic.Interface) dynamic.ResourceInterface {
	if r.Kind == KindClusterIssuer {
		return dyn.Resource(kube.GVRClusterIssuer)
	}
	return dyn.Resource(kube.GVRIssuer).Namespace(r.Namespace)
}

// build wraps spec in an Issuer or ClusterIssuer for r.
func build(r Ref, spec cmapi.IssuerSpec, annotations map[string]string) (*unstructured.Unstructured, error) {
	meta := metav1.ObjectMeta{
		Name: r.Name, Namespace: r.Namespace, Annotations: annotations,
		Labels: map[string]string{kube.ManagedByLabel: kube.ManagedByValue},
	}
	tm := metav1.TypeMeta{APIVersion: "cert-manager.io/v1", Kind: r.Kind}
	var obj runtime.Object
	if r.Kind == KindClusterIssuer {
		obj = &cmapi.ClusterIssuer{TypeMeta: tm, ObjectMeta: meta, Spec: spec}
	} else {
		obj = &cmapi.Issuer{TypeMeta: tm, ObjectMeta: meta, Spec: spec}
	}
	return cert.ToUnstructured(obj)
}

// SelfSigned is a self-signed issuer.
func SelfSigned(r Ref) (*unstructured.Unstructured, error) {
	return build(r, cmapi.IssuerSpec{IssuerConfig: cmapi.IssuerConfig{SelfSigned: &cmapi.SelfSignedIssuer{}}}, nil)
}

// CA is an issuer that signs with the key pair in secret.
func CA(r Ref, secret string) (*unstructured.Unstructured, error) {
	if secret == "" {
		return nil, errors.New("a CA issuer needs --secret, or --generate to make a new root CA")
	}
	return build(r, caSpec(secret), nil)
}

func caSpec(secret string) cmapi.IssuerSpec {
	return cmapi.IssuerSpec{IssuerConfig: cmapi.IssuerConfig{CA: &cmapi.CAIssuer{SecretName: secret}}}
}

// ACMEOptions describe an ACME issuer.
type ACMEOptions struct {
	Server        string
	Email         string
	SkipTLSVerify bool
	// HTTP01IngressClass solves HTTP-01 through an Ingress of this class.
	HTTP01IngressClass string
	// RFC2136 solves DNS-01 by dynamic DNS update, when Nameserver is set.
	RFC2136 RFC2136
}

// RFC2136 configures a DNS-01 solver that updates a DNS server directly.
type RFC2136 struct {
	Nameserver    string
	TSIGKeyName   string
	TSIGAlgorithm string
	TSIGSecret    string
	TSIGSecretKey string
}

// Well-known ACME directories.
var acmeServers = map[string]string{
	"letsencrypt":         "https://acme-v02.api.letsencrypt.org/directory",
	"letsencrypt-staging": "https://acme-staging-v02.api.letsencrypt.org/directory",
}

// ACMEServer expands a shorthand (letsencrypt, letsencrypt-staging) to its URL.
func ACMEServer(s string) string {
	if u, ok := acmeServers[s]; ok {
		return u
	}
	return s
}

// ACME is an ACME issuer with one solver.
func ACME(r Ref, o ACMEOptions) (*unstructured.Unstructured, error) {
	server := ACMEServer(o.Server)
	switch {
	case !strings.HasPrefix(server, "https://"):
		return nil, fmt.Errorf("--server %q must be an https:// URL, letsencrypt or letsencrypt-staging", o.Server)
	case o.HTTP01IngressClass == "" && o.RFC2136.Nameserver == "":
		return nil, errors.New("choose a solver: --http01-ingress-class or --dns01-rfc2136-nameserver")
	case o.HTTP01IngressClass != "" && o.RFC2136.Nameserver != "":
		return nil, errors.New("choose one solver: --http01-ingress-class or --dns01-rfc2136-nameserver, not both")
	}
	var solver acmev1.ACMEChallengeSolver
	if o.HTTP01IngressClass != "" {
		class := o.HTTP01IngressClass
		solver.HTTP01 = &acmev1.ACMEChallengeSolverHTTP01{Ingress: &acmev1.ACMEChallengeSolverHTTP01Ingress{IngressClassName: &class}}
	} else {
		d := o.RFC2136
		p := &acmev1.ACMEIssuerDNS01ProviderRFC2136{Nameserver: d.Nameserver, TSIGKeyName: d.TSIGKeyName, TSIGAlgorithm: d.TSIGAlgorithm}
		if d.TSIGSecret != "" {
			key := d.TSIGSecretKey
			if key == "" {
				key = "tsig-secret"
			}
			p.TSIGSecret = cmmeta.SecretKeySelector{LocalObjectReference: cmmeta.LocalObjectReference{Name: d.TSIGSecret}, Key: key}
		}
		solver.DNS01 = &acmev1.ACMEChallengeSolverDNS01{RFC2136: p}
	}
	spec := cmapi.IssuerSpec{IssuerConfig: cmapi.IssuerConfig{ACME: &acmev1.ACMEIssuer{
		Server:        server,
		Email:         o.Email,
		SkipTLSVerify: o.SkipTLSVerify,
		PrivateKey:    cmmeta.SecretKeySelector{LocalObjectReference: cmmeta.LocalObjectReference{Name: r.Name + "-account-key"}},
		Solvers:       []acmev1.ACMEChallengeSolver{solver},
	}}}
	return build(r, spec, nil)
}

// Create creates the issuer.
func Create(ctx context.Context, dyn dynamic.Interface, u *unstructured.Unstructured) error {
	r := Ref{Kind: u.GetKind(), Name: u.GetName(), Namespace: u.GetNamespace()}
	_, err := r.resource(dyn).Create(ctx, u, metav1.CreateOptions{FieldManager: apply.FieldManager})
	if apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("%s already exists (delete it first: cmo issuer delete %s)", r, deleteArgs(r))
	}
	if err != nil {
		return fmt.Errorf("creating %s: %w", r, err)
	}
	return nil
}

func deleteArgs(r Ref) string {
	if r.Kind == KindIssuer {
		return r.Name + " -n " + r.Namespace
	}
	return r.Name
}

// Get reads one issuer.
func Get(ctx context.Context, dyn dynamic.Interface, r Ref) (*unstructured.Unstructured, error) {
	u, err := r.resource(dyn).Get(ctx, r.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, fmt.Errorf("%s not found (%s; list them with: cmo issuer list)", r, notFoundHint(ctx, dyn, r))
	}
	return u, err
}

// notFoundHint points at an issuer of the same name and the other kind, the
// usual reason one isn't found.
func notFoundHint(ctx context.Context, dyn dynamic.Interface, r Ref) string {
	if r.Kind == KindIssuer {
		if _, err := dyn.Resource(kube.GVRClusterIssuer).Get(ctx, r.Name, metav1.GetOptions{}); err == nil {
			return "there is a ClusterIssuer " + r.Name + ": leave out -n"
		}
		return "for a ClusterIssuer leave out -n"
	}
	l, err := dyn.Resource(kube.GVRIssuer).List(ctx, metav1.ListOptions{})
	if err == nil {
		var where []string
		for _, i := range l.Items {
			if i.GetName() == r.Name {
				where = append(where, i.GetNamespace())
			}
		}
		if len(where) > 0 {
			sort.Strings(where)
			return fmt.Sprintf("there is an Issuer %s in %s: pass -n %s", r.Name, strings.Join(where, ", "), where[0])
		}
	}
	return "ClusterIssuers are cluster-wide; for a namespaced Issuer pass -n"
}

// Type is the kind of issuer u configures.
func Type(u *unstructured.Unstructured) string {
	spec, _, _ := unstructured.NestedMap(u.Object, "spec")
	for key, t := range map[string]string{"selfSigned": TypeSelfSigned, "ca": TypeCA, "acme": TypeACME, "vault": TypeVault, "venafi": TypeVenafi} {
		if _, ok := spec[key]; ok {
			return t
		}
	}
	return TypeExternal
}

// Summary is one row of cmo issuer list.
type Summary struct {
	Ref
	Type    string    `json:"type"`
	Ready   string    `json:"ready"`
	Message string    `json:"message,omitempty"`
	Created time.Time `json:"created"`
	Managed bool      `json:"managed"`
	// GeneratedCA is set on a CA issuer cmo generated with --generate.
	GeneratedCA bool `json:"generatedCA,omitempty"`
}

// Summarize reads the fields cmo shows from an issuer.
func Summarize(u *unstructured.Unstructured) Summary {
	s := Summary{
		Ref:  Ref{Kind: u.GetKind(), Name: u.GetName(), Namespace: u.GetNamespace()},
		Type: Type(u), Ready: wait.ReadyText(u), Created: u.GetCreationTimestamp().Time,
		Managed:     u.GetLabels()[kube.ManagedByLabel] == kube.ManagedByValue,
		GeneratedCA: u.GetAnnotations()[AnnCACertificate] != "",
	}
	if s.Kind == "" {
		s.Kind = KindIssuer
		if s.Namespace == "" {
			s.Kind = KindClusterIssuer
		}
	}
	if c, ok := wait.GetCondition(u, "Ready"); ok && c.Status != string(metav1.ConditionTrue) {
		s.Message = c.Message
	}
	return s
}

// List lists ClusterIssuers and the Issuers in ns (every namespace when ns is
// empty): ClusterIssuers first, then by namespace and name.
func List(ctx context.Context, dyn dynamic.Interface, ns, selector string) ([]Summary, error) {
	opts := metav1.ListOptions{LabelSelector: selector}
	ci, err := dyn.Resource(kube.GVRClusterIssuer).List(ctx, opts)
	if err != nil {
		return nil, fmt.Errorf("listing ClusterIssuers: %w", err)
	}
	is, err := dyn.Resource(kube.GVRIssuer).Namespace(ns).List(ctx, opts)
	if err != nil {
		return nil, fmt.Errorf("listing Issuers: %w", err)
	}
	out := make([]Summary, 0, len(ci.Items)+len(is.Items))
	for i := range ci.Items {
		ci.Items[i].SetKind(KindClusterIssuer)
		out = append(out, Summarize(&ci.Items[i]))
	}
	for i := range is.Items {
		is.Items[i].SetKind(KindIssuer)
		out = append(out, Summarize(&is.Items[i]))
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Kind != b.Kind {
			return a.Kind == KindClusterIssuer
		}
		if a.Namespace != b.Namespace {
			return a.Namespace < b.Namespace
		}
		return a.Name < b.Name
	})
	return out, nil
}

// WaitReady waits for the issuer to be Ready.
func WaitReady(ctx context.Context, dyn dynamic.Interface, r Ref, timeout time.Duration, note func(string)) error {
	var last string
	err := wait.For(ctx, 2*time.Second, timeout, func(ctx context.Context) (bool, error) {
		u, err := Get(ctx, dyn, r)
		if err != nil {
			return false, err
		}
		if wait.IsReady(u) {
			return true, nil
		}
		if c, ok := wait.GetCondition(u, "Ready"); ok {
			last = c.Reason + ": " + c.Message
			if note != nil {
				note(c.Message)
			}
		}
		return false, nil
	})
	if errors.Is(err, wait.ErrTimeout) {
		if last == "" {
			last = "cert-manager has not reported on it; is cert-manager running? (cmo status)"
		}
		return fmt.Errorf("%s is not ready after %s: %s", r, timeout, last)
	}
	return err
}

// Resolve finds the issuer a Certificate in ns should use: the Issuer of that
// name in ns, else the ClusterIssuer. clusterOnly skips the Issuer.
func Resolve(ctx context.Context, dyn dynamic.Interface, name, ns string, clusterOnly bool) (cert.IssuerRef, error) {
	if !clusterOnly {
		_, err := dyn.Resource(kube.GVRIssuer).Namespace(ns).Get(ctx, name, metav1.GetOptions{})
		if err == nil {
			return cert.IssuerRef{Name: name, Kind: KindIssuer}, nil
		}
		if !apierrors.IsNotFound(err) {
			return cert.IssuerRef{}, fmt.Errorf("looking up Issuer %s: %w", name, err)
		}
	}
	_, err := dyn.Resource(kube.GVRClusterIssuer).Get(ctx, name, metav1.GetOptions{})
	if err == nil {
		return cert.IssuerRef{Name: name, Kind: KindClusterIssuer}, nil
	}
	if !apierrors.IsNotFound(err) {
		return cert.IssuerRef{}, fmt.Errorf("looking up ClusterIssuer %s: %w", name, err)
	}
	msg := fmt.Sprintf("no Issuer %q in namespace %s and no ClusterIssuer %q", name, ns, name)
	if clusterOnly {
		msg = fmt.Sprintf("no ClusterIssuer %q", name)
	}
	if all, err := List(ctx, dyn, ns, ""); err == nil && len(all) > 0 {
		names := make([]string, 0, len(all))
		for _, s := range all {
			names = append(names, s.Name)
		}
		msg += "; available here: " + strings.Join(names, ", ")
	} else {
		msg += "; create one with: cmo issuer create selfsigned <name>"
	}
	return cert.IssuerRef{}, errors.New(msg)
}
