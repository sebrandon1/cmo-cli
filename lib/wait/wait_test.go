package wait

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	kfake "k8s.io/client-go/kubernetes/fake"
)

func TestFor(t *testing.T) {
	ctx := context.Background()
	n := 0
	if err := For(ctx, time.Millisecond, time.Second, func(context.Context) (bool, error) { n++; return n == 3, nil }); err != nil || n != 3 {
		t.Errorf("For = %v after %d calls, want nil after 3", err, n)
	}
	if err := For(ctx, time.Millisecond, 20*time.Millisecond, func(context.Context) (bool, error) { return false, nil }); !errors.Is(err, ErrTimeout) {
		t.Errorf("never done: err = %v, want ErrTimeout", err)
	}
	boom := errors.New("boom")
	if err := For(ctx, time.Millisecond, time.Second, func(context.Context) (bool, error) { return false, boom }); !errors.Is(err, boom) {
		t.Errorf("err = %v, want boom", err)
	}
	// A call cut off by the deadline is a timeout, not its own error.
	err := For(ctx, time.Millisecond, 10*time.Millisecond, func(ctx context.Context) (bool, error) {
		<-ctx.Done()
		return false, ctx.Err()
	})
	if !errors.Is(err, ErrTimeout) {
		t.Errorf("deadline inside f: err = %v, want ErrTimeout", err)
	}
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if err := For(cctx, time.Millisecond, time.Second, func(context.Context) (bool, error) { return false, nil }); !errors.Is(err, context.Canceled) {
		t.Errorf("canceled: err = %v", err)
	}
}

func TestConditions(t *testing.T) {
	u := &unstructured.Unstructured{Object: map[string]any{"status": map[string]any{"conditions": []any{
		map[string]any{"type": "Ready", "status": "False", "reason": "Pending", "message": "waiting"},
		"junk",
	}}}}
	c, ok := GetCondition(u, "Ready")
	if !ok || c.Reason != "Pending" || c.Message != "waiting" {
		t.Errorf("GetCondition = %+v, %v", c, ok)
	}
	if IsReady(u) || ReadyText(u) != "False" {
		t.Error("a False Ready condition reads as ready")
	}
	if _, ok := GetCondition(u, "Issuing"); ok {
		t.Error("found a condition that isn't there")
	}
	if ReadyText(&unstructured.Unstructured{Object: map[string]any{}}) != "Unknown" {
		t.Error("no conditions should be Unknown")
	}
}

func deployment(name string, ready int32, available bool) *appsv1.Deployment {
	status := corev1.ConditionFalse
	if available {
		status = corev1.ConditionTrue
	}
	one := int32(1)
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns"},
		Spec:       appsv1.DeploymentSpec{Replicas: &one},
		Status: appsv1.DeploymentStatus{ReadyReplicas: ready, Conditions: []appsv1.DeploymentCondition{
			{Type: appsv1.DeploymentAvailable, Status: status},
		}},
	}
}

func TestDeployments(t *testing.T) {
	ctx := context.Background()
	k := kfake.NewClientset(deployment("a", 1, true), deployment("b", 0, false))
	s, err := Deployment(ctx, k, "ns", "missing")
	if err != nil || s.Found {
		t.Errorf("missing deployment = %+v, %v", s, err)
	}
	if err := Deployments(ctx, k, "ns", []string{"a"}, time.Second, nil); err != nil {
		t.Errorf("available deployment: %v", err)
	}
	var notes []string
	err = Deployments(ctx, k, "ns", []string{"a", "b"}, 50*time.Millisecond, func(s string) { notes = append(notes, s) })
	if err == nil || !strings.Contains(err.Error(), "ns/b is not available") {
		t.Errorf("err = %v", err)
	}
	if len(notes) == 0 || notes[0] != "b 0/1 ready" {
		t.Errorf("notes = %v", notes)
	}
}
