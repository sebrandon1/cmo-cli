// Package wait polls the cluster until something is ready, and reads the
// status conditions cert-manager and OLM objects carry.
package wait

import (
	"context"
	"errors"
	"fmt"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/kubernetes"
)

// ErrTimeout is returned when the condition was not met in time. Callers wrap
// it with what they were waiting for.
var ErrTimeout = errors.New("timed out")

// Func reports whether the wait is over. A non-nil error stops the wait at once.
type Func func(ctx context.Context) (done bool, err error)

// For calls f every interval until it reports done, returns an error, the
// timeout passes, or ctx is canceled. f runs once immediately.
func For(ctx context.Context, interval, timeout time.Duration, f Func) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		done, err := f(ctx)
		if err != nil {
			// A call cut off by the deadline is the wait timing out.
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return ErrTimeout
			}
			return err
		}
		if done {
			return nil
		}
		select {
		case <-ctx.Done():
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return ErrTimeout
			}
			return ctx.Err()
		case <-t.C:
		}
	}
}

// Condition is one entry of an object's .status.conditions.
type Condition struct {
	Type    string `json:"type"`
	Status  string `json:"status"`
	Reason  string `json:"reason,omitempty"`
	Message string `json:"message,omitempty"`
}

// Conditions returns u's .status.conditions.
func Conditions(u *unstructured.Unstructured) []Condition {
	raw, _, _ := unstructured.NestedSlice(u.Object, "status", "conditions")
	out := make([]Condition, 0, len(raw))
	for _, r := range raw {
		m, ok := r.(map[string]any)
		if !ok {
			continue
		}
		c := Condition{}
		c.Type, _ = m["type"].(string)
		c.Status, _ = m["status"].(string)
		c.Reason, _ = m["reason"].(string)
		c.Message, _ = m["message"].(string)
		out = append(out, c)
	}
	return out
}

// GetCondition returns the condition of type t, and whether it is there.
func GetCondition(u *unstructured.Unstructured, t string) (Condition, bool) {
	for _, c := range Conditions(u) {
		if c.Type == t {
			return c, true
		}
	}
	return Condition{}, false
}

// IsReady reports whether u has Ready=True.
func IsReady(u *unstructured.Unstructured) bool {
	c, ok := GetCondition(u, "Ready")
	return ok && c.Status == string(metav1.ConditionTrue)
}

// ReadyText is "True", "False" or "Unknown" for a table cell.
func ReadyText(u *unstructured.Unstructured) string {
	c, ok := GetCondition(u, "Ready")
	if !ok || c.Status == "" {
		return "Unknown"
	}
	return c.Status
}

// DeploymentState is a deployment's readiness, for display.
type DeploymentState struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
	Found     bool   `json:"found"`
	Ready     int32  `json:"ready"`
	Desired   int32  `json:"desired"`
	Available bool   `json:"available"`
}

// Deployment reads one deployment's state. A missing deployment is not an error.
func Deployment(ctx context.Context, kube kubernetes.Interface, ns, name string) (DeploymentState, error) {
	s := DeploymentState{Name: name, Namespace: ns}
	d, err := kube.AppsV1().Deployments(ns).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return s, nil
	}
	if err != nil {
		return s, fmt.Errorf("getting deployment %s/%s: %w", ns, name, err)
	}
	s.Found = true
	s.Ready = d.Status.ReadyReplicas
	s.Desired = 1
	if d.Spec.Replicas != nil {
		s.Desired = *d.Spec.Replicas
	}
	s.Available = deploymentAvailable(d) && s.Ready >= s.Desired
	return s, nil
}

func deploymentAvailable(d *appsv1.Deployment) bool {
	for _, c := range d.Status.Conditions {
		if c.Type == appsv1.DeploymentAvailable {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

// Deployments waits until every named deployment in ns is available. It
// reports progress through note, which may be nil.
func Deployments(ctx context.Context, kube kubernetes.Interface, ns string, names []string, timeout time.Duration, note func(string)) error {
	pending := ""
	err := For(ctx, 2*time.Second, timeout, func(ctx context.Context) (bool, error) {
		for _, n := range names {
			s, err := Deployment(ctx, kube, ns, n)
			if err != nil {
				return false, err
			}
			if !s.Available {
				pending = n
				if note != nil {
					note(fmt.Sprintf("%s %d/%d ready", n, s.Ready, s.Desired))
				}
				return false, nil
			}
		}
		return true, nil
	})
	if errors.Is(err, ErrTimeout) {
		return fmt.Errorf("deployment %s/%s is not available after %s", ns, pending, timeout)
	}
	return err
}
