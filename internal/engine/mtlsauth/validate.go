package mtlsauth

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"strings"
	"time"
)

var (
	ErrMaterialMissing = errors.New("mTLS certificate and key are required")
	ErrMaterialInvalid = errors.New("mTLS certificate/key invalid or mismatched")
	ErrCertExpired     = errors.New("mTLS certificate is expired")
)

// CertificatePair validates mTLS material without returning parser details that
// might include secret-adjacent input context in logs, telemetry, or API errors.
func CertificatePair(certPEM, keyPEM string, now time.Time) (tls.Certificate, error) {
	if strings.TrimSpace(certPEM) == "" || strings.TrimSpace(keyPEM) == "" {
		return tls.Certificate{}, ErrMaterialMissing
	}
	cert, err := tls.X509KeyPair([]byte(certPEM), []byte(keyPEM))
	if err != nil {
		return tls.Certificate{}, ErrMaterialInvalid
	}
	if CertificateExpired(certPEM, now) {
		return tls.Certificate{}, ErrCertExpired
	}
	return cert, nil
}

// CertificateExpired reads the leaf PEM directly because tls.X509KeyPair does
// not consistently populate Leaf, and expiry must fail before network dispatch.
func CertificateExpired(certPEM string, now time.Time) bool {
	block, _ := pem.Decode([]byte(certPEM))
	if block == nil {
		return true
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return true
	}
	return now.UTC().After(cert.NotAfter)
}
