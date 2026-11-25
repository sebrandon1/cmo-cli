// Package pebble deploys Pebble, Let's Encrypt's ACME test server, so ACME
// issuers can be tested in a cluster with no public CA, rate limits or
// account.
package pebble

import (
	"bytes"
	_ "embed"
	"fmt"
	"text/template"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/sebrandon1/cmo-cli/lib/install"
)

// Defaults.
const (
	DefaultNamespace = "pebble"
	DefaultVersion   = "2.10.1"
	DefaultDNSServer = "8.8.8.8:53"
	// Deployment is the Pebble Deployment's name.
	Deployment = "pebble"
	image      = "ghcr.io/letsencrypt/pebble"
)

//go:embed manifests/pebble.yaml
var manifest string

var tmpl = template.Must(template.New("pebble").Parse(manifest))

// Options describe the Pebble to deploy.
type Options struct {
	Namespace string
	Version   string
	// DNSServer is where Pebble resolves the names it validates.
	DNSServer string
	// AlwaysValid makes Pebble accept every challenge without checking it.
	// cert-manager's own HTTP-01 self-check still runs.
	AlwaysValid bool
}

func (o Options) withDefaults() Options {
	if o.Namespace == "" {
		o.Namespace = DefaultNamespace
	}
	if o.Version == "" {
		o.Version = DefaultVersion
	}
	if o.DNSServer == "" {
		o.DNSServer = DefaultDNSServer
	}
	return o
}

// Objects are the Namespace, ConfigMap, Deployment and Service for o.
func Objects(o Options) ([]*unstructured.Unstructured, error) {
	o = o.withDefaults()
	always := "0"
	if o.AlwaysValid {
		always = "1"
	}
	var b bytes.Buffer
	err := tmpl.Execute(&b, map[string]string{
		"Namespace": o.Namespace, "Image": image + ":" + o.Version,
		"DNSServer": o.DNSServer, "AlwaysValid": always,
	})
	if err != nil {
		return nil, err
	}
	return install.ParseManifest(b.Bytes())
}

// DirectoryURL is the ACME directory of the Pebble in namespace ns, as an
// issuer in the cluster reaches it.
func DirectoryURL(ns string) string {
	if ns == "" {
		ns = DefaultNamespace
	}
	return fmt.Sprintf("https://pebble.%s.svc.cluster.local:14000/dir", ns)
}
