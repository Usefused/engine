package browserauth

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

const CSRFHeader = "X-Fused-CSRF"

var ErrAmbiguousCredential = errors.New("ambiguous browser credential")

type CredentialSource string

const (
	CredentialSourceNone   CredentialSource = "none"
	CredentialSourceHeader CredentialSource = "api_key"
	CredentialSourceCookie CredentialSource = "cookie"
)

type CookieManager struct {
	key    []byte
	secure bool
}

func NewCookieManager(key []byte) (*CookieManager, error) {
	if len(key) != 32 {
		return nil, errors.New("invalid browser cookie configuration")
	}
	return &CookieManager{key: append([]byte(nil), key...), secure: os.Getenv("FUSED_ENV") != "development"}, nil
}

func (m *CookieManager) SetSession(w http.ResponseWriter, rawCredential string, expiresAt time.Time) {
	http.SetCookie(w, m.sessionCookie(rawCredential, expiresAt))
	http.SetCookie(w, m.csrfCookie(m.token(rawCredential), expiresAt))
}

func (m *CookieManager) RefreshCSRF(w http.ResponseWriter, rawCredential string) {
	// Session status can repair a deleted/stale readable CSRF cookie without
	// rotating or exposing the HttpOnly credential. A CSRF cookie outliving an
	// expired session has no authority on its own.
	http.SetCookie(w, m.csrfCookie(m.token(rawCredential), time.Now().Add(8*time.Hour)))
}

func (m *CookieManager) SetLoginBinding(w http.ResponseWriter, transactionID, pollToken string, expiresAt time.Time) {
	http.SetCookie(w, m.loginCookie(m.loginToken(transactionID, pollToken), expiresAt))
}

func (m *CookieManager) ValidateLoginBinding(r *http.Request, transactionID, pollToken string) bool {
	value, err := oneCookieValue(r, m.loginName())
	return err == nil && constantTimeEqual(value, m.loginToken(transactionID, pollToken))
}

func (m *CookieManager) ClearLoginBinding(w http.ResponseWriter) {
	cookie := m.loginCookie("", time.Unix(1, 0).UTC())
	cookie.MaxAge = -1
	http.SetCookie(w, cookie)
}

func (m *CookieManager) ValidateSameOrigin(r *http.Request) bool {
	origin, err := url.Parse(strings.TrimSpace(r.Header.Get("Origin")))
	if err != nil || origin.Host == "" || !strings.EqualFold(origin.Host, r.Host) {
		return false
	}
	if m.secure {
		return origin.Scheme == "https"
	}
	return origin.Scheme == "http" || origin.Scheme == "https"
}

func (m *CookieManager) ClearSession(w http.ResponseWriter) {
	expired := time.Unix(1, 0).UTC()
	session := m.sessionCookie("", expired)
	csrf := m.csrfCookie("", expired)
	session.MaxAge, csrf.MaxAge = -1, -1
	http.SetCookie(w, session)
	http.SetCookie(w, csrf)
}

func (m *CookieManager) ValidateCSRF(r *http.Request, rawCredential string) bool {
	provided := strings.TrimSpace(r.Header.Get(CSRFHeader))
	cookieValue, err := oneCookieValue(r, m.csrfName())
	if err != nil || provided == "" || cookieValue == "" {
		return false
	}
	expected := m.token(rawCredential)
	return constantTimeEqual(provided, cookieValue) && constantTimeEqual(provided, expected)
}

func (m *CookieManager) token(rawCredential string) string {
	return m.signedToken("fused-browser-csrf-v1\x00", rawCredential)
}

func (m *CookieManager) loginToken(transactionID, pollToken string) string {
	return m.signedToken("fused-browser-login-v1\x00", transactionID+"\x00"+pollToken)
}

func (m *CookieManager) signedToken(domain, value string) string {
	digest := hmac.New(sha256.New, m.key)
	_, _ = digest.Write([]byte(domain))
	_, _ = digest.Write([]byte(value))
	return base64.RawURLEncoding.EncodeToString(digest.Sum(nil))
}

func (m *CookieManager) sessionCookie(value string, expiresAt time.Time) *http.Cookie {
	return &http.Cookie{
		Name: m.sessionName(), Value: value, Path: "/", Expires: expiresAt,
		MaxAge: cookieMaxAge(expiresAt), HttpOnly: true, Secure: m.secure,
		SameSite: http.SameSiteLaxMode,
	}
}

func (m *CookieManager) csrfCookie(value string, expiresAt time.Time) *http.Cookie {
	return &http.Cookie{
		Name: m.csrfName(), Value: value, Path: "/", Expires: expiresAt,
		MaxAge: cookieMaxAge(expiresAt), Secure: m.secure, SameSite: http.SameSiteLaxMode,
	}
}

func (m *CookieManager) loginCookie(value string, expiresAt time.Time) *http.Cookie {
	return &http.Cookie{
		Name: m.loginName(), Value: value, Path: "/", Expires: expiresAt,
		MaxAge: cookieMaxAge(expiresAt), HttpOnly: true, Secure: m.secure,
		SameSite: http.SameSiteLaxMode,
	}
}

func (m *CookieManager) sessionName() string {
	if m.secure {
		return "__Host-fused_session"
	}
	return "fused_session_dev"
}

func (m *CookieManager) csrfName() string {
	if m.secure {
		return "__Host-fused_csrf"
	}
	return "fused_csrf_dev"
}

func (m *CookieManager) loginName() string {
	if m.secure {
		return "__Host-fused_login"
	}
	return "fused_login_dev"
}

func cookieMaxAge(expiresAt time.Time) int {
	seconds := int(time.Until(expiresAt).Seconds())
	if seconds < 1 {
		return 1
	}
	return seconds
}

func constantTimeEqual(left, right string) bool {
	if len(left) != len(right) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(left), []byte(right)) == 1
}

func CredentialFromRequest(r *http.Request, manager *CookieManager) (string, CredentialSource, error) {
	header := strings.TrimSpace(r.Header.Get("X-API-Key"))
	cookie := ""
	if manager != nil {
		value, err := oneCookieValue(r, manager.sessionName())
		if err != nil && !errors.Is(err, http.ErrNoCookie) {
			return "", CredentialSourceNone, err
		}
		cookie = strings.TrimSpace(value)
	}
	if header != "" && cookie != "" {
		return "", CredentialSourceNone, ErrAmbiguousCredential
	}
	if header != "" {
		return header, CredentialSourceHeader, nil
	}
	if cookie != "" {
		return cookie, CredentialSourceCookie, nil
	}
	return "", CredentialSourceNone, nil
}

func oneCookieValue(r *http.Request, name string) (string, error) {
	value := ""
	found := false
	for _, cookie := range r.Cookies() {
		if cookie.Name != name {
			continue
		}
		if found {
			return "", ErrAmbiguousCredential
		}
		value, found = cookie.Value, true
	}
	if !found {
		return "", http.ErrNoCookie
	}
	return value, nil
}

func RequiresCSRF(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return false
	default:
		return true
	}
}
