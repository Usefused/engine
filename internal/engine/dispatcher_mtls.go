package engine

import (
	"crypto/tls"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/Usefused/engine/internal/engine/mtlsauth"
	"github.com/Usefused/engine/internal/shared/models"
)

var errMTLSRedirectBlocked = errors.New("mTLS cross-host redirect blocked")

// providerClientForAuth returns the shared provider client unless the selected
// auth config needs mTLS; client certificates must never mutate shared transport.
func (d *Dispatcher) providerClientForAuth(auths models.AuthConfigs, credentials map[string]any) (*http.Client, error) {
	if len(auths) == 0 || !isMutualTLSAuth(auths[0]) {
		return d.client, nil
	}
	return newProviderMTLSClient(auths[0], credentials)
}

// isMutualTLSAuth accepts both imported and public spellings because Registry
// metadata may arrive before public auth-family normalization.
func isMutualTLSAuth(auth models.AuthConfig) bool {
	normalized := strings.ToLower(strings.TrimSpace(auth.Type))
	normalized = strings.ReplaceAll(normalized, "-", "_")
	return normalized == "mutualtls" || normalized == "mutual_tls" || normalized == "mtls"
}

// newProviderMTLSClient scopes the certificate to one request execution and
// blocks cross-host redirects so client certs cannot leak to another provider.
func newProviderMTLSClient(auth models.AuthConfig, credentials map[string]any) (*http.Client, error) {
	cert, err := mtlsCertificate(auth, credentials)
	if err != nil {
		return nil, err
	}
	tlsConfig := &tls.Config{
		MinVersion:   tls.VersionTLS12,
		Certificates: []tls.Certificate{cert},
	}
	return &http.Client{
		Timeout:       30 * time.Second,
		Transport:     newProviderHTTPTransport(tlsConfig),
		CheckRedirect: sameHostRedirectOnly,
	}, nil
}

// mtlsCertificate validates the pair before dispatch so provider errors never
// need to carry secret-adjacent parser details back to callers or telemetry.
func mtlsCertificate(auth models.AuthConfig, credentials map[string]any) (tls.Certificate, error) {
	name := mtlsCredentialName(auth)
	certPEM, _ := credentials[name+"_cert"].(string)
	keyPEM, _ := credentials[name+"_key"].(string)
	return mtlsauth.CertificatePair(certPEM, keyPEM, time.Now().UTC())
}

// mtlsCredentialName mirrors sandbox auth mapping for direct dispatcher tests
// and future non-sandbox callers that may pass unnamed mutualTLS metadata.
func mtlsCredentialName(auth models.AuthConfig) string {
	if name := strings.TrimSpace(auth.Name); name != "" {
		return name
	}
	return "mtls"
}

// sameHostRedirectOnly preserves normal same-host redirects while preventing a
// client certificate from being presented to a different redirect target.
func sameHostRedirectOnly(req *http.Request, via []*http.Request) error {
	if len(via) == 0 {
		return nil
	}
	if !strings.EqualFold(req.URL.Host, via[0].URL.Host) {
		return errMTLSRedirectBlocked
	}
	return nil
}
