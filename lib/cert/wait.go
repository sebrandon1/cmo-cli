package cert

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"

	"github.com/sebrandon1/cmo-cli/lib/kube"
	"github.com/sebrandon1/cmo-cli/lib/wait"
)

// revisionAnnotation is set on each CertificateRequest to the Certificate
// revision it issues.
const revisionAnnotation = "cert-manager.io/certificate-revision"

// ErrFailed is returned when issuance failed and waiting won't help.
var ErrFailed = errors.New("issuance failed")

// Revision is a Certificate's .status.revision, 0 before the first issuance.
func Revision(u *unstructured.Unstructured) int64 {
	r, _, _ := unstructured.NestedInt64(u.Object, "status", "revision")
	return r
}

// WaitReady waits until the Certificate is Ready at revision minRevision or
// later. It returns early when the CertificateRequest for that revision has
// failed or been denied. On failure the error carries what Diagnose found.
func WaitReady(ctx context.Context, dyn dynamic.Interface, ns, name string, minRevision int64, timeout time.Duration, note func(string)) error {
	if note == nil {
		note = func(string) {}
	}
	err := wait.For(ctx, 2*time.Second, timeout, func(ctx context.Context) (bool, error) {
		u, err := Get(ctx, dyn, ns, name)
		if err != nil {
			return false, err
		}
		if wait.IsReady(u) && Revision(u) >= minRevision {
			return true, nil
		}
		reqs, err := requests(ctx, dyn, u)
		if err != nil {
			return false, err
		}
		if len(reqs) > 0 {
			latest := reqs[len(reqs)-1]
			if requestRevision(&latest) >= max(minRevision, 1) {
				if msg, failed := requestFailed(&latest); failed {
					return false, fmt.Errorf("%w: %s", ErrFailed, msg)
				}
			}
		}
		if c, ok := wait.GetCondition(u, "Ready"); ok && c.Message != "" {
			note(c.Message)
		} else {
			note("waiting for cert-manager to issue it")
		}
		return false, nil
	})
	if err == nil {
		return nil
	}
	if errors.Is(err, wait.ErrTimeout) {
		err = fmt.Errorf("certificate %s/%s is not ready after %s", ns, name, timeout)
	}
	if lines := Diagnose(ctx, dyn, ns, name); len(lines) > 0 {
		return fmt.Errorf("%w\n  %s", err, strings.Join(lines, "\n  "))
	}
	return err
}

// requests are the CertificateRequests the Certificate owns, oldest revision
// first.
func requests(ctx context.Context, dyn dynamic.Interface, c *unstructured.Unstructured) ([]unstructured.Unstructured, error) {
	return owned(ctx, dyn, kube.GVRCertificateRequest, c, func(a, b *unstructured.Unstructured) bool {
		return requestRevision(a) < requestRevision(b)
	})
}

// owned lists the objects of gvr in owner's namespace that owner owns.
func owned(ctx context.Context, dyn dynamic.Interface, gvr schema.GroupVersionResource, owner *unstructured.Unstructured, less func(a, b *unstructured.Unstructured) bool) ([]unstructured.Unstructured, error) {
	l, err := dyn.Resource(gvr).Namespace(owner.GetNamespace()).List(ctx, metav1.ListOptions{})
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("listing %s: %w", gvr.Resource, err)
	}
	var out []unstructured.Unstructured
	for _, o := range l.Items {
		if ownedBy(&o, owner.GetUID()) {
			out = append(out, o)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return less(&out[i], &out[j]) })
	return out, nil
}

func ownedBy(o *unstructured.Unstructured, uid types.UID) bool {
	for _, r := range o.GetOwnerReferences() {
		if r.UID == uid {
			return true
		}
	}
	return false
}

func requestRevision(u *unstructured.Unstructured) int64 {
	n, _ := strconv.ParseInt(u.GetAnnotations()[revisionAnnotation], 10, 64)
	return n
}

// requestFailed reports whether a CertificateRequest failed or was denied.
func requestFailed(u *unstructured.Unstructured) (string, bool) {
	if c, ok := wait.GetCondition(u, "Denied"); ok && c.Status == string(metav1.ConditionTrue) {
		return "request " + u.GetName() + " was denied: " + c.Message, true
	}
	if c, ok := wait.GetCondition(u, "Ready"); ok && c.Status == string(metav1.ConditionFalse) && c.Reason == "Failed" {
		return "request " + u.GetName() + " failed: " + c.Message, true
	}
	return "", false
}

// Diagnose explains why a Certificate is not Ready: its conditions, its
// latest CertificateRequest, and for ACME the Order and Challenges behind it.
// It is best effort: what it cannot read it leaves out.
func Diagnose(ctx context.Context, dyn dynamic.Interface, ns, name string) []string {
	u, err := Get(ctx, dyn, ns, name)
	if err != nil {
		return []string{err.Error()}
	}
	var lines []string
	for _, c := range wait.Conditions(u) {
		lines = append(lines, fmt.Sprintf("certificate %s=%s %s: %s", c.Type, c.Status, c.Reason, c.Message))
	}
	reqs, _ := requests(ctx, dyn, u)
	if len(reqs) == 0 {
		return lines
	}
	cr := reqs[len(reqs)-1]
	for _, c := range wait.Conditions(&cr) {
		lines = append(lines, fmt.Sprintf("request %s %s=%s %s: %s", cr.GetName(), c.Type, c.Status, c.Reason, c.Message))
	}
	orders, _ := owned(ctx, dyn, kube.GVROrder, &cr, byName)
	for i := range orders {
		o := &orders[i]
		state, _, _ := unstructured.NestedString(o.Object, "status", "state")
		reason, _, _ := unstructured.NestedString(o.Object, "status", "reason")
		lines = append(lines, fmt.Sprintf("order %s state=%s %s", o.GetName(), orDash(state), reason))
		challenges, _ := owned(ctx, dyn, kube.GVRChallenge, o, byName)
		for j := range challenges {
			ch := &challenges[j]
			typ, _, _ := unstructured.NestedString(ch.Object, "spec", "type")
			dns, _, _ := unstructured.NestedString(ch.Object, "spec", "dnsName")
			st, _, _ := unstructured.NestedString(ch.Object, "status", "state")
			why, _, _ := unstructured.NestedString(ch.Object, "status", "reason")
			lines = append(lines, fmt.Sprintf("challenge %s %s for %s state=%s %s", ch.GetName(), typ, dns, orDash(st), why))
		}
	}
	return lines
}

func byName(a, b *unstructured.Unstructured) bool { return a.GetName() < b.GetName() }

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// Renew asks cert-manager to reissue the Certificate now, the way cmctl renew
// does: it sets the Issuing condition. It returns the revision before renewal.
func Renew(ctx context.Context, dyn dynamic.Interface, ns, name string) (int64, error) {
	u, err := Get(ctx, dyn, ns, name)
	if err != nil {
		return 0, err
	}
	if c, ok := wait.GetCondition(u, "Issuing"); ok && c.Status == string(metav1.ConditionTrue) {
		return 0, fmt.Errorf("certificate %s/%s is already being issued", ns, name)
	}
	rev := Revision(u)
	conds, _, _ := unstructured.NestedSlice(u.Object, "status", "conditions")
	conds = append(conds, map[string]any{
		"type": "Issuing", "status": "True", "reason": "ManuallyTriggered",
		"message":            "Certificate re-issuance manually triggered by cmo",
		"lastTransitionTime": time.Now().UTC().Format(time.RFC3339),
	})
	if err := unstructured.SetNestedSlice(u.Object, conds, "status", "conditions"); err != nil {
		return 0, err
	}
	if _, err := dyn.Resource(kube.GVRCertificate).Namespace(ns).UpdateStatus(ctx, u, metav1.UpdateOptions{}); err != nil {
		return 0, fmt.Errorf("triggering renewal of %s/%s: %w", ns, name, err)
	}
	return rev, nil
}
