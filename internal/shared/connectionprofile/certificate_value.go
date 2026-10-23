package connectionprofile

import (
	"crypto/x509"
	"encoding/pem"
	"errors"
	"strings"
	"unicode"
)

// validateCertificateBucketValue keeps public certificate storage isolated from environment names and private keys.
func validateCertificateBucketValue(name, value string) error {
	// Validate namespace before parsing any supplied certificate material.
	if err := validateCertificateBucketName(name); err != nil {
		return err
	}
	remaining := strings.TrimSpace(value)
	// Empty material cannot represent a certificate entry.
	if remaining == "" {
		return errors.New("PEM certificate is required")
	}
	for remaining != "" {
		// PEM decoding must not silently skip private keys, prose, or other leading material.
		if !strings.HasPrefix(remaining, "-----BEGIN CERTIFICATE-----") {
			return errors.New("certificate entries accept PEM certificates only")
		}
		block, rest := pem.Decode([]byte(remaining))
		// Every block in a chain must be a valid certificate, not a key or arbitrary ASN.1 value.
		if block == nil || block.Type != "CERTIFICATE" {
			return errors.New("invalid PEM certificate")
		}
		if _, err := x509.ParseCertificate(block.Bytes); err != nil {
			return errors.New("invalid X.509 certificate")
		} // Never include certificate input in validation errors.
		remaining = strings.TrimSpace(string(rest))
	}
	return nil
}

// validateCertificateBucketName reserves a collision-free namespace for public certificate entries.
func validateCertificateBucketName(name string) error {
	publicName, ok := strings.CutPrefix(name, "cert:")
	// A mandatory namespace prevents a certificate from replacing an ordinary environment entry.
	if !ok || publicName == "" {
		return errors.New("certificate name must use the cert: namespace")
	}
	for _, char := range publicName {
		// Public names must fit one bucket reference segment without creating a second namespace.
		if unicode.IsSpace(char) || strings.ContainsRune(".:${}", char) {
			return errors.New("invalid certificate name")
		}
	}
	return nil
}
