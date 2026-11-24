// Package cluster works out what kind of cluster cmo is talking to.
package cluster

import (
	"context"
	"fmt"
	"slices"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"

	"github.com/sebrandon1/cmo-cli/lib/kube"
)

// Platform is how cert-manager is installed on a cluster.
type Platform string

const (
	// Auto means detect the platform.
	Auto Platform = "auto"
	// OpenShift installs the Red Hat cert-manager Operator through OLM.
	OpenShift Platform = "openshift"
	// Kubernetes applies the upstream cert-manager release manifest.
	Kubernetes Platform = "kubernetes"
)

// Platforms are the values --platform accepts.
var Platforms = []string{string(Auto), string(OpenShift), string(Kubernetes)}

// ParsePlatform checks a --platform value.
func ParsePlatform(s string) (Platform, error) {
	if !slices.Contains(Platforms, s) {
		return "", fmt.Errorf("unknown platform %q: choose auto, openshift or kubernetes", s)
	}
	return Platform(s), nil
}

// Info is what discovery says about the cluster.
type Info struct {
	Platform Platform `json:"platform"`
	// OLM is whether the OLM API (operators.coreos.com) is served.
	OLM bool `json:"olm"`
	// ServerVersion is the Kubernetes version, such as v1.34.2.
	ServerVersion string `json:"serverVersion"`
	// OpenShiftVersion is set on OpenShift, such as 4.22.1.
	OpenShiftVersion string `json:"openshiftVersion,omitempty"`
}

// Detect asks discovery which API groups the cluster serves. It is OpenShift
// when config.openshift.io is served.
func Detect(dc discovery.DiscoveryInterface) (Info, error) {
	var info Info
	v, err := dc.ServerVersion()
	if err != nil {
		return info, fmt.Errorf("cannot reach the cluster: %w", err)
	}
	info.ServerVersion = v.GitVersion
	groups, err := dc.ServerGroups()
	if err != nil {
		return info, fmt.Errorf("listing API groups: %w", err)
	}
	info.Platform = Kubernetes
	for _, g := range groups.Groups {
		switch g.Name {
		case "config.openshift.io":
			info.Platform = OpenShift
		case "operators.coreos.com":
			info.OLM = true
		}
	}
	return info, nil
}

// Resolve turns --platform into the platform to use: auto takes the detected
// one. OpenShift needs OLM.
func Resolve(requested Platform, info Info) (Platform, error) {
	p := requested
	if p == Auto || p == "" {
		p = info.Platform
	}
	if p == OpenShift && !info.OLM {
		return "", fmt.Errorf("the cluster does not serve the OLM API (operators.coreos.com), which the OpenShift install needs; try --platform kubernetes")
	}
	return p, nil
}

// OpenShiftVersion reads the version ClusterVersion reports, such as 4.22.1.
// It is empty when it can't be read.
func OpenShiftVersion(ctx context.Context, dyn dynamic.Interface) string {
	cv, err := dyn.Resource(kube.GVRClusterVersion).Get(ctx, "version", metav1.GetOptions{})
	if err != nil {
		return ""
	}
	v, _, _ := unstructured.NestedString(cv.Object, "status", "desired", "version")
	return v
}

// Describe is "openshift 4.22.1 (kubernetes v1.35.9)" or "kubernetes v1.34.2".
func (i Info) Describe() string {
	if i.OpenShiftVersion != "" {
		return fmt.Sprintf("%s %s (kubernetes %s)", i.Platform, i.OpenShiftVersion, i.ServerVersion)
	}
	return fmt.Sprintf("%s %s", i.Platform, i.ServerVersion)
}
