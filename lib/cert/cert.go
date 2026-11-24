// Package cert builds, lists, waits on, renews, deletes and inspects
// cert-manager Certificates.
package cert

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"slices"
	"sort"
	"strings"
	"time"

	cmapi "github.com/cert-manager/cert-manager/pkg/apis/certmanager/v1"
	cmmeta "github.com/cert-manager/cert-manager/pkg/apis/meta/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"

	"github.com/sebrandon1/cmo-cli/lib/apply"
	"github.com/sebrandon1/cmo-cli/lib/kube"
	"github.com/sebrandon1/cmo-cli/lib/wait"
)

// RoleLabel marks the root Certificate of a generated CA, which goes with its
// issuer (cmo issuer delete) rather than with the other certificates.
const (
	RoleLabel  = "cmo-cli/role"
	RoleCARoot = "ca-root"
)

// IssuerRef names the issuer that signs a Certificate.
type IssuerRef struct {
	Name string
	// Kind is Issuer or ClusterIssuer.
	Kind string
}

// Key algorithms --key-algorithm accepts.
const (
	AlgRSA     = "rsa"
	AlgECDSA   = "ecdsa"
	AlgEd25519 = "ed25519"
)

// Options describe a Certificate to create.
type Options struct {
	Name       string
	Namespace  string
	Issuer     IssuerRef
	CommonName string
	DNSNames   []string
	IPs        []string
	URIs       []string
	// Duration and RenewBefore are cert-manager's defaults (90d, 2/3 through)
	// when zero.
	Duration    time.Duration
	RenewBefore time.Duration
	// KeyAlgorithm is rsa, ecdsa or ed25519; KeySize 0 takes its default.
	KeyAlgorithm string
	KeySize      int
	// SecretName defaults to <name>-tls.
	SecretName string
	IsCA       bool
	Labels     map[string]string
}

var keySizes = map[string][]int{AlgRSA: {2048, 3072, 4096}, AlgECDSA: {256, 384, 521}, AlgEd25519: nil}

var algorithms = map[string]cmapi.PrivateKeyAlgorithm{
	AlgRSA: cmapi.RSAKeyAlgorithm, AlgECDSA: cmapi.ECDSAKeyAlgorithm, AlgEd25519: cmapi.Ed25519KeyAlgorithm,
}

// Validate reports the first problem with o.
func (o *Options) Validate() error {
	switch {
	case o.Name == "":
		return errors.New("a certificate needs a name")
	case o.Issuer.Name == "":
		return errors.New("--issuer is required (list them with: cmo issuer list)")
	case o.CommonName == "" && len(o.DNSNames) == 0 && len(o.IPs) == 0 && len(o.URIs) == 0:
		return errors.New("give at least one of --dns, --ip, --uri or --common-name")
	}
	if err := o.validateSANs(); err != nil {
		return err
	}
	if err := o.validateKey(); err != nil {
		return err
	}
	if o.Duration != 0 && o.Duration < time.Hour {
		return errors.New("--duration must be at least 1h")
	}
	if o.Duration != 0 && o.RenewBefore >= o.Duration {
		return errors.New("--renew-before must be shorter than --duration")
	}
	return nil
}

func (o *Options) validateSANs() error {
	for _, ip := range o.IPs {
		if net.ParseIP(ip) == nil {
			return fmt.Errorf("--ip %q is not an IP address", ip)
		}
	}
	for _, u := range o.URIs {
		if p, err := url.Parse(u); err != nil || p.Scheme == "" {
			return fmt.Errorf("--uri %q is not an absolute URI", u)
		}
	}
	return nil
}

func (o *Options) validateKey() error {
	if o.KeyAlgorithm == "" {
		if o.KeySize != 0 {
			return errors.New("--key-size needs --key-algorithm")
		}
		return nil
	}
	sizes, ok := keySizes[strings.ToLower(o.KeyAlgorithm)]
	if !ok {
		return fmt.Errorf("--key-algorithm %q: choose rsa, ecdsa or ed25519", o.KeyAlgorithm)
	}
	if o.KeySize == 0 || slices.Contains(sizes, o.KeySize) {
		return nil
	}
	if sizes == nil {
		return errors.New("--key-size does not apply to ed25519")
	}
	return fmt.Errorf("--key-size %d is not valid for %s: choose one of %v", o.KeySize, o.KeyAlgorithm, sizes)
}

// Build makes the Certificate o describes.
func Build(o *Options) (*unstructured.Unstructured, error) {
	if err := o.Validate(); err != nil {
		return nil, err
	}
	secret := o.SecretName
	if secret == "" {
		secret = o.Name + "-tls"
	}
	labels := map[string]string{kube.ManagedByLabel: kube.ManagedByValue}
	for k, v := range o.Labels {
		labels[k] = v
	}
	c := &cmapi.Certificate{
		TypeMeta:   metav1.TypeMeta{APIVersion: "cert-manager.io/v1", Kind: "Certificate"},
		ObjectMeta: metav1.ObjectMeta{Name: o.Name, Namespace: o.Namespace, Labels: labels},
		Spec: cmapi.CertificateSpec{
			SecretName:  secret,
			CommonName:  o.CommonName,
			DNSNames:    o.DNSNames,
			IPAddresses: o.IPs,
			URIs:        o.URIs,
			IsCA:        o.IsCA,
			IssuerRef:   cmmeta.IssuerReference{Name: o.Issuer.Name, Kind: o.Issuer.Kind, Group: "cert-manager.io"},
		},
	}
	if o.Duration != 0 {
		c.Spec.Duration = &metav1.Duration{Duration: o.Duration}
	}
	if o.RenewBefore != 0 {
		c.Spec.RenewBefore = &metav1.Duration{Duration: o.RenewBefore}
	}
	if o.KeyAlgorithm != "" {
		c.Spec.PrivateKey = &cmapi.CertificatePrivateKey{
			Algorithm:      algorithms[strings.ToLower(o.KeyAlgorithm)],
			Size:           o.KeySize,
			RotationPolicy: cmapi.RotationPolicyAlways,
		}
	}
	return ToUnstructured(c)
}

// ToUnstructured converts a typed object, dropping the empty status and
// creation timestamp the conversion adds.
func ToUnstructured(obj runtime.Object) (*unstructured.Unstructured, error) {
	m, err := runtime.DefaultUnstructuredConverter.ToUnstructured(obj)
	if err != nil {
		return nil, err
	}
	delete(m, "status")
	if md, ok := m["metadata"].(map[string]any); ok {
		delete(md, "creationTimestamp")
	}
	return &unstructured.Unstructured{Object: m}, nil
}

// Create creates the Certificate.
func Create(ctx context.Context, dyn dynamic.Interface, u *unstructured.Unstructured) error {
	_, err := dyn.Resource(kube.GVRCertificate).Namespace(u.GetNamespace()).Create(ctx, u, metav1.CreateOptions{FieldManager: apply.FieldManager})
	if apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("certificate %s already exists in %s (delete it first: cmo cert delete %s -n %s)", u.GetName(), u.GetNamespace(), u.GetName(), u.GetNamespace())
	}
	if err != nil {
		return fmt.Errorf("creating certificate %s: %w", u.GetName(), err)
	}
	return nil
}

// Get reads one Certificate.
func Get(ctx context.Context, dyn dynamic.Interface, ns, name string) (*unstructured.Unstructured, error) {
	u, err := dyn.Resource(kube.GVRCertificate).Namespace(ns).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, &notFound{msg: fmt.Sprintf("certificate %s not found in namespace %s (list them with: cmo cert list -A)", name, ns), err: err}
	}
	return u, err
}

// notFound is a friendlier message for a NotFound API error, which it still
// unwraps to, so apierrors.IsNotFound holds.
type notFound struct {
	msg string
	err error
}

func (e *notFound) Error() string { return e.msg }
func (e *notFound) Unwrap() error { return e.err }

// Summary is one row of cmo cert list.
type Summary struct {
	Name        string    `json:"name"`
	Namespace   string    `json:"namespace"`
	Ready       string    `json:"ready"`
	Reason      string    `json:"reason,omitempty"`
	Secret      string    `json:"secret"`
	Issuer      string    `json:"issuer"`
	IssuerKind  string    `json:"issuerKind"`
	DNSNames    []string  `json:"dnsNames,omitempty"`
	NotAfter    time.Time `json:"notAfter,omitzero"`
	RenewalTime time.Time `json:"renewalTime,omitzero"`
	Created     time.Time `json:"created"`
	Managed     bool      `json:"managed"`
	// CARoot is set on the root of a CA cmo generated.
	CARoot bool `json:"caRoot,omitempty"`
}

// Summarize reads the fields cmo shows from a Certificate.
func Summarize(u *unstructured.Unstructured) Summary {
	s := Summary{
		Name: u.GetName(), Namespace: u.GetNamespace(), Ready: wait.ReadyText(u),
		Created: u.GetCreationTimestamp().Time,
		Managed: u.GetLabels()[kube.ManagedByLabel] == kube.ManagedByValue,
		CARoot:  u.GetLabels()[RoleLabel] == RoleCARoot,
	}
	if c, ok := wait.GetCondition(u, "Ready"); ok && c.Status != string(metav1.ConditionTrue) {
		s.Reason = c.Reason
	}
	s.Secret, _, _ = unstructured.NestedString(u.Object, "spec", "secretName")
	s.Issuer, _, _ = unstructured.NestedString(u.Object, "spec", "issuerRef", "name")
	s.IssuerKind, _, _ = unstructured.NestedString(u.Object, "spec", "issuerRef", "kind")
	if s.IssuerKind == "" {
		s.IssuerKind = "Issuer"
	}
	s.DNSNames, _, _ = unstructured.NestedStringSlice(u.Object, "spec", "dnsNames")
	s.NotAfter = nestedTime(u, "status", "notAfter")
	s.RenewalTime = nestedTime(u, "status", "renewalTime")
	return s
}

func nestedTime(u *unstructured.Unstructured, path ...string) time.Time {
	v, _, _ := unstructured.NestedString(u.Object, path...)
	t, err := time.Parse(time.RFC3339, v)
	if err != nil {
		return time.Time{}
	}
	return t
}

// List lists Certificates in ns, or in every namespace when ns is empty,
// sorted by namespace and name.
func List(ctx context.Context, dyn dynamic.Interface, ns, selector string) ([]Summary, error) {
	l, err := dyn.Resource(kube.GVRCertificate).Namespace(ns).List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		return nil, fmt.Errorf("listing certificates: %w", err)
	}
	out := make([]Summary, 0, len(l.Items))
	for i := range l.Items {
		out = append(out, Summarize(&l.Items[i]))
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Namespace != out[j].Namespace {
			return out[i].Namespace < out[j].Namespace
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

// Delete deletes a Certificate and, unless keepSecret, the Secret it wrote.
func Delete(ctx context.Context, dyn dynamic.Interface, k kubernetes.Interface, ns, name string, keepSecret bool) ([]apply.Change, error) {
	u, err := Get(ctx, dyn, ns, name)
	if err != nil {
		return nil, err
	}
	secret, _, _ := unstructured.NestedString(u.Object, "spec", "secretName")
	changes := []apply.Change{{Kind: "Certificate", Namespace: ns, Name: name, Action: apply.Removed}}
	if err := dyn.Resource(kube.GVRCertificate).Namespace(ns).Delete(ctx, name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		return nil, fmt.Errorf("deleting certificate %s: %w", name, err)
	}
	if keepSecret || secret == "" {
		return changes, nil
	}
	c, err := apply.Outcome(apply.Change{Kind: "Secret", Namespace: ns, Name: secret},
		k.CoreV1().Secrets(ns).Delete(ctx, secret, metav1.DeleteOptions{}))
	if err != nil {
		return changes, err
	}
	return append(changes, c), nil
}

// ReadSecret reads the Secret a Certificate wrote.
func ReadSecret(ctx context.Context, dyn dynamic.Interface, k kubernetes.Interface, ns, name string) (*corev1.Secret, error) {
	u, err := Get(ctx, dyn, ns, name)
	if err != nil {
		return nil, err
	}
	secret, _, _ := unstructured.NestedString(u.Object, "spec", "secretName")
	s, err := k.CoreV1().Secrets(ns).Get(ctx, secret, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, fmt.Errorf("certificate %s has not been issued yet: secret %s does not exist (see: cmo cert get %s -n %s)", name, secret, name, ns)
	}
	if err != nil {
		return nil, fmt.Errorf("reading secret %s: %w", secret, err)
	}
	return s, nil
}
