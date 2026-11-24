package cert

import (
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Info is what cmo cert inspect shows about an issued certificate.
type Info struct {
	Subject      string    `json:"subject"`
	Issuer       string    `json:"issuer"`
	Serial       string    `json:"serial"`
	NotBefore    time.Time `json:"notBefore"`
	NotAfter     time.Time `json:"notAfter"`
	DNSNames     []string  `json:"dnsNames,omitempty"`
	IPAddresses  []string  `json:"ipAddresses,omitempty"`
	URIs         []string  `json:"uris,omitempty"`
	KeyAlgorithm string    `json:"keyAlgorithm"`
	KeySize      int       `json:"keySize,omitempty"`
	IsCA         bool      `json:"isCA"`
	SelfSigned   bool      `json:"selfSigned"`
	SHA256       string    `json:"sha256"`
	// ChainLength counts the certificates in tls.crt, leaf included.
	ChainLength int `json:"chainLength"`
	// HasCA is whether the secret carries ca.crt.
	HasCA bool `json:"hasCA"`
}

// Inspect parses a PEM chain (tls.crt) and describes its first certificate.
func Inspect(chain, ca []byte) (Info, error) {
	certs, err := parseChain(chain)
	if err != nil {
		return Info{}, err
	}
	leaf := certs[0]
	sum := sha256.Sum256(leaf.Raw)
	info := Info{
		Subject:     leaf.Subject.String(),
		Issuer:      leaf.Issuer.String(),
		Serial:      strings.ToUpper(leaf.SerialNumber.Text(16)),
		NotBefore:   leaf.NotBefore.UTC(),
		NotAfter:    leaf.NotAfter.UTC(),
		DNSNames:    leaf.DNSNames,
		IsCA:        leaf.IsCA,
		SelfSigned:  leaf.CheckSignature(leaf.SignatureAlgorithm, leaf.RawTBSCertificate, leaf.Signature) == nil,
		SHA256:      hex.EncodeToString(sum[:]),
		ChainLength: len(certs),
		HasCA:       len(strings.TrimSpace(string(ca))) > 0,
	}
	for _, ip := range leaf.IPAddresses {
		info.IPAddresses = append(info.IPAddresses, ip.String())
	}
	for _, u := range leaf.URIs {
		info.URIs = append(info.URIs, u.String())
	}
	info.KeyAlgorithm, info.KeySize = keyInfo(leaf.PublicKey)
	return info, nil
}

func parseChain(b []byte) ([]*x509.Certificate, error) {
	var out []*x509.Certificate
	for {
		var block *pem.Block
		block, b = pem.Decode(b)
		if block == nil {
			break
		}
		if block.Type != "CERTIFICATE" {
			continue
		}
		c, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("parsing certificate: %w", err)
		}
		out = append(out, c)
	}
	if len(out) == 0 {
		return nil, errors.New("tls.crt holds no PEM certificate")
	}
	return out, nil
}

func keyInfo(pub any) (string, int) {
	switch k := pub.(type) {
	case *rsa.PublicKey:
		return "RSA", k.N.BitLen()
	case *ecdsa.PublicKey:
		return "ECDSA", k.Curve.Params().BitSize
	case ed25519.PublicKey:
		return "Ed25519", 0
	}
	return "unknown", 0
}
