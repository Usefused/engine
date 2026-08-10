package messaging

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"

	server "github.com/nats-io/nats-server/v2/server"
)

func TestConnectNATSAuthenticatesWithTokenAndMTLS(t *testing.T) {
	clearNATSEnv(t)
	certificates := createTestNATSCertificates(t)
	serverCertificate, err := tls.LoadX509KeyPair(certificates.serverCert, certificates.serverKey)
	if err != nil {
		t.Fatalf("load server certificate: %v", err)
	}
	caPEM, err := os.ReadFile(certificates.caCert)
	if err != nil {
		t.Fatalf("read CA certificate: %v", err)
	}
	clientCAs := x509.NewCertPool()
	if !clientCAs.AppendCertsFromPEM(caPEM) {
		t.Fatal("parse test CA certificate")
	}
	natsServer := startAuthenticatedNATSServer(t, &server.Options{
		Authorization: "mtls-token",
		TLSConfig: &tls.Config{
			MinVersion:   tls.VersionTLS12,
			Certificates: []tls.Certificate{serverCertificate},
			ClientAuth:   tls.RequireAndVerifyClientCert,
			ClientCAs:    clientCAs,
		},
	})

	t.Setenv("NATS_URL", natsServer.ClientURL())
	t.Setenv("NATS_TOKEN", "mtls-token")
	t.Setenv("NATS_TLS_CA_FILE", certificates.caCert)
	t.Setenv("NATS_TLS_CERT_FILE", certificates.clientCert)
	t.Setenv("NATS_TLS_KEY_FILE", certificates.clientKey)
	t.Setenv("NATS_TLS_SERVER_NAME", "nats.test")
	client, err := ConnectNATS()
	if err != nil {
		t.Fatalf("connect with token and mTLS: %v", err)
	}
	t.Cleanup(client.Close)
	if err := client.InitStream("AUTH_MTLS_TEST", []string{"auth.mtls"}); err != nil {
		t.Fatalf("mTLS-authenticated JetStream access: %v", err)
	}
}

type testNATSCertificateFiles struct {
	caCert     string
	serverCert string
	serverKey  string
	clientCert string
	clientKey  string
}

func createTestNATSCertificates(t *testing.T) testNATSCertificateFiles {
	t.Helper()
	dir := t.TempDir()
	caKey := createTestRSAKey(t)
	caTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "Fused test NATS CA"},
		NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatalf("create CA certificate: %v", err)
	}
	caCert := filepath.Join(dir, "ca.pem")
	writeTestPEM(t, caCert, "CERTIFICATE", caDER, 0o644)
	caCertificate, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatalf("parse CA certificate: %v", err)
	}
	serverCert, serverKey := createSignedTestCertificate(t, dir, "server", "nats.test", x509.ExtKeyUsageServerAuth, caCertificate, caKey)
	clientCert, clientKey := createSignedTestCertificate(t, dir, "client", "fused-engine", x509.ExtKeyUsageClientAuth, caCertificate, caKey)
	return testNATSCertificateFiles{caCert: caCert, serverCert: serverCert, serverKey: serverKey, clientCert: clientCert, clientKey: clientKey}
}

func createSignedTestCertificate(t *testing.T, dir, name, commonName string, usage x509.ExtKeyUsage, ca *x509.Certificate, caKey *rsa.PrivateKey) (string, string) {
	t.Helper()
	key := createTestRSAKey(t)
	template := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: commonName}, DNSNames: []string{commonName},
		NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment, ExtKeyUsage: []x509.ExtKeyUsage{usage},
	}
	certificateDER, err := x509.CreateCertificate(rand.Reader, template, ca, &key.PublicKey, caKey)
	if err != nil {
		t.Fatalf("create %s certificate: %v", name, err)
	}
	certFile := filepath.Join(dir, name+".pem")
	keyFile := filepath.Join(dir, name+"-key.pem")
	writeTestPEM(t, certFile, "CERTIFICATE", certificateDER, 0o644)
	writeTestPEM(t, keyFile, "RSA PRIVATE KEY", x509.MarshalPKCS1PrivateKey(key), 0o600)
	return certFile, keyFile
}

func createTestRSAKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("create test RSA key: %v", err)
	}
	return key
}

func writeTestPEM(t *testing.T, path, blockType string, bytes []byte, mode os.FileMode) {
	t.Helper()
	encoded := pem.EncodeToMemory(&pem.Block{Type: blockType, Bytes: bytes})
	if err := os.WriteFile(path, encoded, mode); err != nil {
		t.Fatalf("write %s: %v", filepath.Base(path), err)
	}
}
