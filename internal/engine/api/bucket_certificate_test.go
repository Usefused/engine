package api

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
)

// TestBucketCertificateHTTPBoundary exercises public certificate and private-key rejection through the Engine route.
func TestBucketCertificateHTTPBoundary(t *testing.T) {
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	} // A valid key is needed solely to generate a public test certificate.
	template := &x509.Certificate{SerialNumber: big.NewInt(1), NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, template, template, public, private)
	if err != nil {
		t.Fatal(err)
	} // Never use invalid fixtures to prove successful admission.
	certificate := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	for _, test := range []struct {
		value  string
		status int
	}{{certificate, http.StatusNoContent}, {"-----BEGIN PRIVATE KEY-----\nkey\n-----END PRIVATE KEY-----", http.StatusBadRequest}} {
		fixture := newBucketValueFixture()
		router := buildConnectAdminRouter(fixture.store, fixture.store.accountID, fixture.masterKey)
		body, err := json.Marshal(map[string]string{"service_id": uuid.Nil.String(), "key_name": "cert:root", "location": "cert", "value": test.value})
		if err != nil {
			t.Fatal(err)
		} // Serialization errors must not create an empty test request.
		req := httptest.NewRequest(http.MethodPut, "/workspace/buckets/"+fixture.bucketID.String()+"/values", bytes.NewReader(body))
		req.Header.Set("X-API-Key", "test-key")
		response := httptest.NewRecorder()
		router.ServeHTTP(response, req)
		if response.Code != test.status {
			t.Fatalf("status=%d want=%d body=%s", response.Code, test.status, response.Body.String())
		} // HTTP admission must match certificate semantics.
		if (fixture.store.upsertedValue != nil) != (test.status == http.StatusNoContent) {
			t.Fatal("invalid material reached storage")
		} // A rejected private key cannot be persisted.
	}
}
