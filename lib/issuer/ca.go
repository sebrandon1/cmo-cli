package issuer

import (
	"context"
	"fmt"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"

	"github.com/sebrandon1/cmo-cli/lib/apply"
	"github.com/sebrandon1/cmo-cli/lib/cert"
	"github.com/sebrandon1/cmo-cli/lib/kube"
)

// ClusterResourceNamespace is where cert-manager reads the secrets
// ClusterIssuers refer to. It is cert-manager's namespace on both platforms.
const ClusterResourceNamespace = "cert-manager"

// CAOptions describe a root CA to generate.
type CAOptions struct {
	CommonName string
	// Duration is the root's lifetime; RenewBefore when it is renewed.
	Duration    time.Duration
	RenewBefore time.Duration
}

// CAPlan names the three objects GenerateCA creates for r.
type CAPlan struct {
	Bootstrap Ref
	// CertNamespace and CertName hold the root CA Certificate; its secret has
	// the same name.
	CertNamespace string
	CertName      string
	CA            Ref
}

// PlanCA lays out a generated CA: a self-signed bootstrap issuer, a root CA
// Certificate it signs, and the CA issuer r that signs with the root.
func PlanCA(r Ref) CAPlan {
	ns := r.Namespace
	if r.Kind == KindClusterIssuer {
		ns = ClusterResourceNamespace
	}
	return CAPlan{
		Bootstrap:     Ref{Kind: r.Kind, Name: r.Name + "-selfsigned", Namespace: r.Namespace},
		CertNamespace: ns,
		CertName:      r.Name + "-root-ca",
		CA:            r,
	}
}

// RootCertificate is the root CA Certificate of plan p.
func (p CAPlan) RootCertificate(o CAOptions) (*cert.Options, error) {
	cn := o.CommonName
	if cn == "" {
		cn = p.CA.Name + "-root-ca"
	}
	d, rb := o.Duration, o.RenewBefore
	if d == 0 {
		d, rb = 87600*time.Hour, 8760*time.Hour
	}
	co := &cert.Options{
		Name: p.CertName, Namespace: p.CertNamespace, SecretName: p.CertName,
		Issuer:     cert.IssuerRef{Name: p.Bootstrap.Name, Kind: p.Bootstrap.Kind},
		CommonName: cn, IsCA: true, Duration: d, RenewBefore: rb,
		KeyAlgorithm: cert.AlgECDSA, KeySize: 256,
		Labels: map[string]string{cert.RoleLabel: cert.RoleCARoot},
	}
	return co, co.Validate()
}

// GenerateCA creates the objects of PlanCA(r) in order, waiting for each to
// be ready before the next. Each step is reported through step.
func GenerateCA(ctx context.Context, dyn dynamic.Interface, r Ref, o CAOptions, timeout time.Duration, step func(string)) error {
	p := PlanCA(r)
	if step == nil {
		step = func(string) {}
	}
	co, err := p.RootCertificate(o)
	if err != nil {
		return err
	}
	step("bootstrap issuer " + p.Bootstrap.String())
	boot, err := SelfSigned(p.Bootstrap)
	if err != nil {
		return err
	}
	if err := Create(ctx, dyn, boot); err != nil {
		return err
	}
	if err := WaitReady(ctx, dyn, p.Bootstrap, timeout, nil); err != nil {
		return err
	}
	step("root CA certificate " + p.CertNamespace + "/" + p.CertName)
	root, err := cert.Build(co)
	if err != nil {
		return err
	}
	if err := cert.Create(ctx, dyn, root); err != nil {
		return err
	}
	if err := cert.WaitReady(ctx, dyn, p.CertNamespace, p.CertName, 1, timeout, nil); err != nil {
		return err
	}
	step("CA issuer " + r.String())
	ca, err := build(r, caSpec(p.CertName), map[string]string{
		AnnCACertificate: p.CertNamespace + "/" + p.CertName,
		AnnCABootstrap:   p.Bootstrap.Name,
	})
	if err != nil {
		return err
	}
	if err := Create(ctx, dyn, ca); err != nil {
		return err
	}
	return WaitReady(ctx, dyn, r, timeout, nil)
}

// Delete deletes the issuer and, for a CA cmo generated, its root
// Certificate, secret and bootstrap issuer. An issuer already gone is Absent.
func Delete(ctx context.Context, dyn dynamic.Interface, k kubernetes.Interface, r Ref) ([]apply.Change, error) {
	c := apply.Change{Kind: r.Kind, Namespace: r.Namespace, Name: r.Name, Action: apply.Removed}
	u, err := r.resource(dyn).Get(ctx, r.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		c.Action = apply.Absent
		return []apply.Change{c}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", r, err)
	}
	if err := r.resource(dyn).Delete(ctx, r.Name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		return nil, fmt.Errorf("deleting %s: %w", r, err)
	}
	changes := []apply.Change{c}
	if ch, ok, err := deleteAccountKey(ctx, k, r, u); err != nil {
		return changes, err
	} else if ok {
		changes = append(changes, ch)
	}
	ann := u.GetAnnotations()
	if ref := ann[AnnCACertificate]; ref != "" {
		if ns, name, ok := strings.Cut(ref, "/"); ok {
			ch, err := cert.Delete(ctx, dyn, k, ns, name, false)
			if err != nil && !apierrors.IsNotFound(err) {
				return changes, err
			}
			changes = append(changes, ch...)
		}
	}
	if b := ann[AnnCABootstrap]; b != "" {
		ch, err := Delete(ctx, dyn, k, Ref{Kind: r.Kind, Name: b, Namespace: r.Namespace})
		if err != nil {
			return changes, err
		}
		changes = append(changes, ch...)
	}
	return changes, nil
}

// deleteAccountKey deletes the ACME account key cert-manager generated for an
// ACME issuer cmo created. A key the user supplied to another issuer is not
// cmo's to delete, so only managed issuers qualify.
func deleteAccountKey(ctx context.Context, k kubernetes.Interface, r Ref, u *unstructured.Unstructured) (apply.Change, bool, error) {
	if Type(u) != TypeACME || u.GetLabels()[kube.ManagedByLabel] != kube.ManagedByValue {
		return apply.Change{}, false, nil
	}
	name, _, _ := unstructured.NestedString(u.Object, "spec", "acme", "privateKeySecretRef", "name")
	if name == "" {
		return apply.Change{}, false, nil
	}
	ns := r.Namespace
	if r.Kind == KindClusterIssuer {
		ns = ClusterResourceNamespace
	}
	c, err := apply.Outcome(apply.Change{Kind: "Secret", Namespace: ns, Name: name},
		k.CoreV1().Secrets(ns).Delete(ctx, name, metav1.DeleteOptions{}))
	return c, true, err
}

// DeleteManaged deletes every issuer cmo created, in ns or everywhere when
// ns is empty, ClusterIssuers included.
func DeleteManaged(ctx context.Context, dyn dynamic.Interface, k kubernetes.Interface, ns string) ([]apply.Change, error) {
	all, err := List(ctx, dyn, ns, kube.ManagedSelector)
	if err != nil {
		return nil, err
	}
	var changes []apply.Change
	done := map[string]bool{}
	// Generated CAs first, so their bootstrap issuers go with them.
	for _, pass := range []bool{true, false} {
		for _, s := range all {
			if done[s.String()] || s.GeneratedCA != pass {
				continue
			}
			ch, err := Delete(ctx, dyn, k, s.Ref)
			if err != nil {
				return changes, err
			}
			for _, c := range ch {
				done[NewRef(c.Name, c.Namespace).String()] = true
			}
			changes = append(changes, ch...)
		}
	}
	return changes, nil
}
