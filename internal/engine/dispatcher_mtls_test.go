package engine

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/Usefused/engine/internal/engine/mtlsauth"
	"github.com/Usefused/engine/internal/shared/models"
)

// TestMTLSCertificateValidatesPair covers the happy path before transport
// creation so later tests can focus on failure modes.
func TestMTLSCertificateValidatesPair(t *testing.T) {
	certPEM, keyPEM := testMTLSPair(t, time.Now().Add(time.Hour))

	_, err := mtlsCertificate(models.AuthConfig{Name: "clientCert", Type: "mutualTLS"}, map[string]any{
		"clientCert_cert": certPEM,
		"clientCert_key":  keyPEM,
	})

	if err != nil {
		t.Fatalf("mtlsCertificate failed: %v", err)
	}
}

// TestMTLSCertificateRejectsMissingOrMismatchedPair ensures partial or wrong
// bucket state fails before the provider sees a request.
func TestMTLSCertificateRejectsMissingOrMismatchedPair(t *testing.T) {
	certPEM, _ := testMTLSPair(t, time.Now().Add(time.Hour))
	_, otherKeyPEM := testMTLSPair(t, time.Now().Add(time.Hour))

	tests := []struct {
		name        string
		credentials map[string]any
		wantErr     error
	}{
		{
			name:        "missing key",
			credentials: map[string]any{"clientCert_cert": certPEM},
			wantErr:     mtlsauth.ErrMaterialMissing,
		},
		{
			name: "mismatched key",
			credentials: map[string]any{
				"clientCert_cert": certPEM,
				"clientCert_key":  otherKeyPEM,
			},
			wantErr: mtlsauth.ErrMaterialInvalid,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := mtlsCertificate(models.AuthConfig{Name: "clientCert", Type: "mutualTLS"}, tt.credentials)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("error = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

// TestMTLSCertificateRejectsExpiredCertificate enforces certificate NotAfter
// independently of bucket secret expires_at metadata.
func TestMTLSCertificateRejectsExpiredCertificate(t *testing.T) {
	certPEM, keyPEM := testMTLSPair(t, time.Now().Add(-time.Hour))

	_, err := mtlsCertificate(models.AuthConfig{Name: "clientCert", Type: "mutualTLS"}, map[string]any{
		"clientCert_cert": certPEM,
		"clientCert_key":  keyPEM,
	})

	if !errors.Is(err, mtlsauth.ErrCertExpired) {
		t.Fatalf("error = %v, want expired certificate", err)
	}
}

// TestMTLSCertificateRejectsEncryptedPrivateKey locks MVP behavior: encrypted
// private keys need a passphrase field before they can be supported safely.
func TestMTLSCertificateRejectsEncryptedPrivateKey(t *testing.T) {
	certPEM, keyPEM := testMTLSPair(t, time.Now().Add(time.Hour))
	encryptedKeyPEM := testEncryptedPrivateKey(t, keyPEM)

	_, err := mtlsCertificate(models.AuthConfig{Name: "clientCert", Type: "mutualTLS"}, map[string]any{
		"clientCert_cert": certPEM,
		"clientCert_key":  encryptedKeyPEM,
	})

	if !errors.Is(err, mtlsauth.ErrMaterialInvalid) {
		t.Fatalf("error = %v, want invalid material", err)
	}
}

// TestProviderClientForMTLSUsesScopedTransport guards against accidentally
// attaching client certs to the shared provider HTTP transport.
func TestProviderClientForMTLSUsesScopedTransport(t *testing.T) {
	certPEM, keyPEM := testMTLSPair(t, time.Now().Add(time.Hour))
	dispatcher := NewDispatcher()

	client, err := dispatcher.providerClientForAuth(models.AuthConfigs{{Name: "clientCert", Type: "mutualTLS"}}, map[string]any{
		"clientCert_cert": certPEM,
		"clientCert_key":  keyPEM,
	})

	if err != nil {
		t.Fatalf("providerClientForAuth failed: %v", err)
	}
	if client == dispatcher.client {
		t.Fatal("mTLS client must not reuse the shared provider client")
	}
	if client.CheckRedirect == nil {
		t.Fatal("mTLS client must install redirect policy")
	}
}

// TestSameHostRedirectOnlyBlocksCrossHost prevents client certificates from
// being presented to an unrelated redirect target.
func TestSameHostRedirectOnlyBlocksCrossHost(t *testing.T) {
	req := &http.Request{URL: mustURL(t, "https://other.example.test/callback")}
	via := []*http.Request{{URL: mustURL(t, "https://api.example.test/start")}}

	err := sameHostRedirectOnly(req, via)

	if !errors.Is(err, errMTLSRedirectBlocked) {
		t.Fatalf("error = %v, want cross-host redirect blocked", err)
	}
}

// testMTLSPair creates a syntactically valid client certificate/key pair so
// tests do not depend on checked-in PEM secrets.
func testMTLSPair(t *testing.T, notAfter time.Time) (string, string) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: "fused-test-client"},
		NotBefore:    notAfter.Add(-time.Hour),
		NotAfter:     notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	return string(certPEM), string(keyPEM)
}

// testEncryptedPrivateKey keeps passphrase handling explicit: encrypted keys
// are rejected in MVP rather than silently accepted without a password path.
func testEncryptedPrivateKey(t *testing.T, keyPEM string) string {
	t.Helper()
	block, _ := pem.Decode([]byte(keyPEM))
	if block == nil {
		t.Fatal("test key PEM did not decode")
	}
	encrypted, err := x509.EncryptPEMBlock(rand.Reader, block.Type, block.Bytes, []byte("passphrase"), x509.PEMCipherAES256)
	if err != nil {
		t.Fatalf("encrypt private key: %v", err)
	}
	return string(pem.EncodeToMemory(encrypted))
}

// mustURL keeps redirect tests focused on policy, not repetitive URL parse
// error handling.
func mustURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse url %q: %v", raw, err)
	}
	return parsed
}
