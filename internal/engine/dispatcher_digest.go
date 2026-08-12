package engine

import (
	"context"
	"crypto/md5"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/Usefused/engine/internal/shared/authrouting"
	"github.com/Usefused/engine/internal/shared/models"
)

// retryHTTPChallenge sends credentials only after the provider proves that the
// reviewed Digest challenge applies; preemptive Digest would bypass negotiation.
func retryHTTPChallenge(ctx context.Context, client *http.Client, req *http.Request, resp *http.Response, auths models.AuthConfigs, credentials map[string]any) (*http.Response, error) {
	return retryHTTPChallengeWithDo(ctx, req, resp, auths, credentials, client.Do)
}

func retryHTTPChallengeWithDo(ctx context.Context, req *http.Request, resp *http.Response, auths models.AuthConfigs, credentials map[string]any, do func(*http.Request) (*http.Response, error)) (*http.Response, error) {
	auth, ok := selectedDigestAuth(auths)
	if !ok || resp == nil || resp.StatusCode != http.StatusUnauthorized {
		return resp, nil
	}
	challenge, ok := digestChallenge(resp.Header.Values("WWW-Authenticate"))
	if !ok {
		return resp, nil
	}
	authorization, err := digestAuthorization(req, auth, credentials, challenge)
	if err != nil {
		return nil, err
	}
	retry, err := cloneRequestBody(ctx, req)
	if err != nil {
		return nil, err
	}
	retry.Header.Set("Authorization", authorization)
	_ = resp.Body.Close()
	return do(retry)
}

func selectedDigestAuth(auths models.AuthConfigs) (models.AuthConfig, bool) {
	for _, auth := range auths {
		if authrouting.CanonicalType(auth.Type, auth.Scheme) == "digest" && auth.Strategy != nil && auth.Strategy.Kind == "http_challenge" && auth.Strategy.Challenge != nil && auth.Strategy.Challenge.Scheme == "digest" {
			return auth, true
		}
	}
	return models.AuthConfig{}, false
}

// cloneRequestBody fails closed for one-shot bodies because challenge handling
// must never turn a consumed payload into a different provider request.
func cloneRequestBody(ctx context.Context, req *http.Request) (*http.Request, error) {
	retry := req.Clone(ctx)
	if req.GetBody == nil {
		if req.Body != nil && req.Body != http.NoBody {
			return nil, errors.New("HTTP challenge request body cannot be replayed")
		}
		return retry, nil
	}
	body, err := req.GetBody()
	if err != nil {
		return nil, errors.New("HTTP challenge request body cannot be replayed")
	}
	retry.Body = body
	return retry, nil
}

func digestChallenge(headers []string) (map[string]string, bool) {
	for _, header := range headers {
		index := strings.Index(strings.ToLower(header), "digest ")
		if index >= 0 {
			return parseDigestParameters(header[index+len("digest "):]), true
		}
	}
	return nil, false
}

func parseDigestParameters(raw string) map[string]string {
	values := make(map[string]string)
	for _, part := range splitDigestParts(raw) {
		key, value, ok := strings.Cut(part, "=")
		if ok {
			values[strings.ToLower(strings.TrimSpace(key))] = strings.Trim(strings.TrimSpace(value), "\"")
		}
	}
	return values
}

func splitDigestParts(raw string) []string {
	parts := make([]string, 0, 8)
	start, quoted := 0, false
	for index, char := range raw {
		if char == '"' {
			quoted = !quoted
		}
		if char == ',' && !quoted {
			parts = append(parts, strings.TrimSpace(raw[start:index]))
			start = index + 1
		}
	}
	return append(parts, strings.TrimSpace(raw[start:]))
}

func digestAuthorization(req *http.Request, auth models.AuthConfig, credentials map[string]any, challenge map[string]string) (string, error) {
	username := credentialValue(credentials, auth.Name+"_username")
	password := credentialValue(credentials, auth.Name+"_password")
	realm, nonce := challenge["realm"], challenge["nonce"]
	if username == "" || password == "" || realm == "" || nonce == "" {
		return "", authRoutingError("challenge_invalid")
	}
	algorithm := strings.ToUpper(challenge["algorithm"])
	if algorithm == "" {
		algorithm = "MD5"
	}
	qop, err := digestQOP(challenge["qop"])
	if err != nil {
		return "", err
	}
	cnonce, nc, uri := digestNonce(), "00000001", req.URL.RequestURI()
	response, err := digestResponse(algorithm, username, password, realm, nonce, req.Method, uri, qop, nc, cnonce)
	if err != nil {
		return "", err
	}
	return formatDigestAuthorization(username, realm, nonce, uri, response, algorithm, qop, nc, cnonce, challenge["opaque"]), nil
}

// digestQOP accepts only auth because auth-int would require hashing a replayable
// entity body, a wire contract the Engine does not currently claim.
func digestQOP(raw string) (string, error) {
	if strings.TrimSpace(raw) == "" {
		return "", nil
	}
	for _, value := range strings.Split(raw, ",") {
		if strings.EqualFold(strings.TrimSpace(value), "auth") {
			return "auth", nil
		}
	}
	return "", authRoutingError("challenge_unsupported")
}

func digestResponse(algorithm, username, password, realm, nonce, method, uri, qop, nc, cnonce string) (string, error) {
	hashValue, sess, err := digestHash(algorithm)
	if err != nil {
		return "", err
	}
	ha1 := hashValue(username + ":" + realm + ":" + password)
	if sess {
		ha1 = hashValue(ha1 + ":" + nonce + ":" + cnonce)
	}
	ha2 := hashValue(method + ":" + uri)
	if qop == "" {
		return hashValue(ha1 + ":" + nonce + ":" + ha2), nil
	}
	return hashValue(ha1 + ":" + nonce + ":" + nc + ":" + cnonce + ":" + qop + ":" + ha2), nil
}

func digestHash(algorithm string) (func(string) string, bool, error) {
	sess := strings.HasSuffix(algorithm, "-SESS")
	base := strings.TrimSuffix(algorithm, "-SESS")
	switch base {
	case "MD5":
		return func(value string) string { sum := md5.Sum([]byte(value)); return hex.EncodeToString(sum[:]) }, sess, nil
	case "SHA-256":
		return func(value string) string { sum := sha256.Sum256([]byte(value)); return hex.EncodeToString(sum[:]) }, sess, nil
	default:
		return nil, false, authRoutingError("challenge_unsupported")
	}
}

func digestNonce() string {
	value := make([]byte, 12)
	if _, err := io.ReadFull(rand.Reader, value); err != nil {
		return "fused"
	}
	return hex.EncodeToString(value)
}

func formatDigestAuthorization(username, realm, nonce, uri, response, algorithm, qop, nc, cnonce, opaque string) string {
	values := []string{
		`username="` + digestQuote(username) + `"`, `realm="` + digestQuote(realm) + `"`,
		`nonce="` + digestQuote(nonce) + `"`, `uri="` + digestQuote(uri) + `"`,
		`response="` + response + `"`, `algorithm=` + algorithm,
	}
	if opaque != "" {
		values = append(values, `opaque="`+digestQuote(opaque)+`"`)
	}
	if qop != "" {
		values = append(values, `qop=`+qop, `nc=`+nc, `cnonce="`+cnonce+`"`)
	}
	return "Digest " + strings.Join(values, ", ")
}

func digestQuote(value string) string {
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(value)
}
