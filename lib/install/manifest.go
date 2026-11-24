package install

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
)

// DefaultVersion is the upstream cert-manager release installed on
// Kubernetes when --version is not given.
const DefaultVersion = "v1.21.3"

// manifestURL is where each upstream release publishes its static manifest.
const manifestURL = "https://github.com/cert-manager/cert-manager/releases/download/%s/cert-manager.yaml"

// maxManifest bounds the download; the real manifest is about 1 MiB.
const maxManifest = 32 << 20

// ManifestURL is the release manifest for version.
func ManifestURL(version string) string { return fmt.Sprintf(manifestURL, version) }

// FetchManifest reads the manifest at source, a URL or a local file path.
func FetchManifest(ctx context.Context, client *http.Client, source string) ([]byte, error) {
	if !strings.HasPrefix(source, "https://") && !strings.HasPrefix(source, "http://") {
		b, err := os.ReadFile(source) // #nosec G304 -- the user names the manifest file
		if err != nil {
			return nil, fmt.Errorf("reading manifest: %w", err)
		}
		return b, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, source, http.NoBody)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("downloading %s: %w", source, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("downloading %s: %s", source, resp.Status)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxManifest))
	if err != nil {
		return nil, fmt.Errorf("downloading %s: %w", source, err)
	}
	return b, nil
}

// ParseManifest splits multi-document YAML into objects, skipping empty
// documents.
func ParseManifest(b []byte) ([]*unstructured.Unstructured, error) {
	dec := utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader(b), 4096)
	var objs []*unstructured.Unstructured
	for {
		var m map[string]any
		err := dec.Decode(&m)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("parsing manifest: %w", err)
		}
		if len(m) == 0 {
			continue
		}
		u := &unstructured.Unstructured{Object: m}
		if u.GetKind() == "" || u.GetName() == "" {
			return nil, fmt.Errorf("parsing manifest: an object has no kind or name")
		}
		objs = append(objs, u)
	}
	if len(objs) == 0 {
		return nil, errors.New("the manifest holds no objects")
	}
	return objs, nil
}

// WithoutCRDs drops the CustomResourceDefinitions from objs.
func WithoutCRDs(objs []*unstructured.Unstructured) []*unstructured.Unstructured {
	out := make([]*unstructured.Unstructured, 0, len(objs))
	for _, o := range objs {
		if o.GetKind() != "CustomResourceDefinition" {
			out = append(out, o)
		}
	}
	return out
}
