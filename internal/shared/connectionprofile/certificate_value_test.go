package connectionprofile

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"math/big"
	"testing"
	"time"
)

// TestCertificateBucketValueValidation admits only namespaced public X.509 PEM, including certificate chains.
func TestCertificateBucketValueValidation(t *testing.T) {
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	} // Tests require a valid ephemeral signing key.
	template := &x509.Certificate{SerialNumber: big.NewInt(1), NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, template, template, public, private)
	if err != nil {
		t.Fatal(err)
	} // Do not substitute malformed fixture data.
	cert := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	for _, test := range []struct {
		name, value string
		valid       bool
	}{
		{"cert:root", cert, true}, {"cert:chain", cert + cert, true},
		{"root", cert, false}, {"cert:", cert, false}, {"cert:bad:name", cert, false},
		{"cert:root", "", false}, {"cert:root", "not PEM", false},
		{"cert:root", "-----BEGIN PRIVATE KEY-----\nkey\n-----END PRIVATE KEY-----", false},
		{"cert:root", cert + "private data", false}, {"cert:root", "prefix" + cert, false},
	} {
		err := ValidateLiteralBucketValue("cert", test.name, test.value)
		if (err == nil) != test.valid {
			t.Fatalf("name=%q valid=%t err=%v", test.name, test.valid, err)
		} // CLI and direct API callers share the same admission contract.
	}
}
