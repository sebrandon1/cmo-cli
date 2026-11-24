package trace

import (
	"bytes"
	"context"
	"net/http"
	"net/url"
	"testing"
)

func TestCommand(t *testing.T) {
	for _, c := range []struct {
		method, path, want string
		skip               bool
	}{
		{"GET", "/api/v1/namespaces/cmo/pods", "get pods -n cmo", false},
		{"GET", "/api/v1/namespaces/cmo/pods?labelSelector=app%3Dbuilder", "get pods -n cmo -l app=builder", false},
		{"GET", "/api/v1/namespaces/cmo/pods/builder-0", "get pods builder-0 -n cmo", false},
		{"GET", "/api/v1/nodes/worker-0", "get nodes worker-0", false},
		{"GET", "/api/v1/namespaces/cmo", "get namespaces cmo", false},
		{"GET", "/apis/kubevirt.io/v1/namespaces/cmo/virtualmachines/seed", "get virtualmachines.kubevirt.io seed -n cmo", false},
		{"GET", "/apis/cdi.kubevirt.io/v1beta1/namespaces/cmo/datavolumes/target", "get datavolumes.cdi.kubevirt.io target -n cmo", false},
		{"POST", "/apis/kubevirt.io/v1/namespaces/cmo/virtualmachines", "create virtualmachines.kubevirt.io -n cmo", false},
		{"PUT", "/api/v1/namespaces/cmo/configmaps/x", "replace configmaps x -n cmo", false},
		{"PATCH", "/apis/kubevirt.io/v1/namespaces/cmo/virtualmachines/seed/status", "patch virtualmachines.kubevirt.io seed -n cmo status", false},
		{"DELETE", "/api/v1/namespaces/cmo/pods/builder-0", "delete pods builder-0 -n cmo", false},
		{"DELETE", "/api/v1/namespaces/cmo/pods", "deletecollection pods -n cmo", false},
		{"GET", "/api/v1/namespaces/cmo/pods/builder-0/log", "logs builder-0 -n cmo", false},
		{"GET", "/api/v1/namespaces/cmo/pods/builder-0/exec", "", true},
		{"GET", "/apis/subresources.kubevirt.io/v1/namespaces/cmo/virtualmachineinstances/seed/console", "", true},
		{"GET", "/apis/kubevirt.io/v1/namespaces/cmo/virtualmachineinstances/seed/screenshot", "get virtualmachineinstances.kubevirt.io seed -n cmo screenshot", false},
		{"GET", "/api/v1/namespaces/cmo/pods?fieldSelector=spec.nodeName%3Dworker-0&watch=true", "get pods -n cmo --field-selector spec.nodeName=worker-0 -w", false},
		{"GET", "/version", "GET /version", false},
		{"GET", "/apis", "GET /apis", false},
	} {
		u, err := url.Parse("https://example.test" + c.path)
		if err != nil {
			t.Fatalf("%s: %v", c.path, err)
		}
		got, ok := Command(c.method, u)
		if c.skip {
			if ok {
				t.Errorf("%s %s: want it skipped, got %q", c.method, c.path, got)
			}
			continue
		}
		if !ok || got != c.want {
			t.Errorf("%s %s = %q, %v; want %q", c.method, c.path, got, ok, c.want)
		}
	}
}

// The trace must not read what a request carries, only how it is addressed.
func TestRoundTripper(t *testing.T) {
	u, _ := url.Parse("https://example.test/api/v1/namespaces/cmo/pods?labelSelector=app%3Dbuilder")
	var seen string
	next := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		seen = r.URL.Path
		return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody}, nil
	})
	rt := RoundTripper(next)
	resp, err := rt.RoundTrip(&http.Request{Method: "GET", URL: u})
	if err != nil {
		t.Fatalf("off: %v", err)
	}
	if err := resp.Body.Close(); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	Enable(&buf)
	t.Cleanup(Disable)
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "https://example.test/api/v1/namespaces/cmo/pods", nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp, err = rt.RoundTrip(req); err != nil {
		t.Fatalf("on: %v", err)
	}
	if err := resp.Body.Close(); err != nil {
		t.Fatal(err)
	}
	Flush()
	if got, want := buf.String(), "→ get pods -n cmo\n"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	if seen != "/api/v1/namespaces/cmo/pods" {
		t.Errorf("next saw %q", seen)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
