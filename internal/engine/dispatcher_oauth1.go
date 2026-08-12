package engine

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"hash"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Usefused/engine/internal/shared/models"
)

type oauth1Pair struct{ key, value string }

// applyOAuth1 signs exactly the reviewed authorization-header strategy so an
// imported scheme cannot silently choose another credential placement.
func applyOAuth1(req *http.Request, auth models.AuthConfig, credentials map[string]any) error {
	if auth.Strategy == nil || auth.Strategy.OAuth1 == nil || auth.Strategy.Kind != "oauth1_signature" {
		return authRoutingError("unsupported_strategy")
	}
	strategy := auth.Strategy.OAuth1
	if strategy.ParameterLocation != "authorization_header" {
		return authRoutingError("unsupported_strategy")
	}
	protocol, err := oauth1ProtocolParameters(auth, credentials, strategy.SignatureMethod)
	if err != nil {
		return err
	}
	all := append(oauth1RequestParameters(req), protocol...)
	signature, err := oauth1Signature(req, all, auth, credentials, strategy.SignatureMethod)
	if err != nil {
		return err
	}
	protocol = append(protocol, oauth1Pair{"oauth_signature", signature})
	req.Header.Set("Authorization", oauth1AuthorizationHeader(protocol))
	return nil
}

func oauth1ProtocolParameters(auth models.AuthConfig, credentials map[string]any, method string) ([]oauth1Pair, error) {
	consumerKey := credentialValue(credentials, auth.Name+"_consumer_key")
	if consumerKey == "" || credentialValue(credentials, auth.Name+"_consumer_secret") == "" {
		return nil, authRoutingError("unsatisfied")
	}
	label, ok := oauth1SignatureLabel(method)
	if !ok {
		return nil, authRoutingError("unsupported_strategy")
	}
	values := []oauth1Pair{
		{"oauth_consumer_key", consumerKey}, {"oauth_nonce", oauth1Nonce()},
		{"oauth_signature_method", label}, {"oauth_timestamp", strconv.FormatInt(time.Now().Unix(), 10)},
		{"oauth_version", "1.0"},
	}
	if token := credentialValue(credentials, auth.Name+"_token"); token != "" {
		values = append(values, oauth1Pair{"oauth_token", token})
	}
	return values, nil
}

func oauth1SignatureLabel(method string) (string, bool) {
	switch method {
	case "hmac_sha1":
		return "HMAC-SHA1", true
	case "hmac_sha256":
		return "HMAC-SHA256", true
	default:
		return "", false
	}
}

func oauth1Nonce() string {
	value := make([]byte, 16)
	if _, err := io.ReadFull(rand.Reader, value); err != nil {
		return hex.EncodeToString([]byte(time.Now().UTC().String()))
	}
	return hex.EncodeToString(value)
}

// oauth1RequestParameters includes form fields only when GetBody proves they can
// be read without consuming the provider request that will actually be sent.
func oauth1RequestParameters(req *http.Request) []oauth1Pair {
	values := oauth1Pairs(req.URL.Query())
	if req.GetBody == nil || !strings.HasPrefix(strings.ToLower(req.Header.Get("Content-Type")), "application/x-www-form-urlencoded") {
		return values
	}
	body, err := req.GetBody()
	if err != nil {
		return values
	}
	defer body.Close()
	raw, err := io.ReadAll(io.LimitReader(body, 1<<20))
	if err != nil {
		return values
	}
	parsed, err := url.ParseQuery(string(raw))
	if err == nil {
		values = append(values, oauth1Pairs(parsed)...)
	}
	return values
}

func oauth1Pairs(values url.Values) []oauth1Pair {
	pairs := make([]oauth1Pair, 0, len(values))
	for key, items := range values {
		for _, value := range items {
			pairs = append(pairs, oauth1Pair{key, value})
		}
	}
	return pairs
}

func oauth1Signature(req *http.Request, pairs []oauth1Pair, auth models.AuthConfig, credentials map[string]any, method string) (string, error) {
	baseURL, err := oauth1BaseURL(req.URL)
	if err != nil {
		return "", err
	}
	base := strings.ToUpper(req.Method) + "&" + oauth1Escape(baseURL) + "&" + oauth1Escape(oauth1NormalizedParameters(pairs))
	key := oauth1Escape(credentialValue(credentials, auth.Name+"_consumer_secret")) + "&" + oauth1Escape(credentialValue(credentials, auth.Name+"_token_secret"))
	var digest func() hash.Hash
	if method == "hmac_sha1" {
		digest = sha1.New
	} else if method == "hmac_sha256" {
		digest = sha256.New
	} else {
		return "", authRoutingError("unsupported_strategy")
	}
	mac := hmac.New(digest, []byte(key))
	_, _ = mac.Write([]byte(base))
	return base64.StdEncoding.EncodeToString(mac.Sum(nil)), nil
}

func oauth1BaseURL(value *url.URL) (string, error) {
	if value == nil || value.Scheme == "" || value.Host == "" {
		return "", errors.New("OAuth1 request URL is invalid")
	}
	host := strings.ToLower(value.Host)
	if (value.Scheme == "http" && strings.HasSuffix(host, ":80")) || (value.Scheme == "https" && strings.HasSuffix(host, ":443")) {
		host = host[:strings.LastIndex(host, ":")]
	}
	path := value.EscapedPath()
	if path == "" {
		path = "/"
	}
	return strings.ToLower(value.Scheme) + "://" + host + path, nil
}

// oauth1NormalizedParameters sorts escaped pairs because OAuth1 signs a
// canonical multimap rather than the incidental order of Go maps.
func oauth1NormalizedParameters(pairs []oauth1Pair) string {
	encoded := make([]oauth1Pair, 0, len(pairs))
	for _, pair := range pairs {
		if pair.key != "oauth_signature" {
			encoded = append(encoded, oauth1Pair{oauth1Escape(pair.key), oauth1Escape(pair.value)})
		}
	}
	sort.Slice(encoded, func(i, j int) bool {
		return encoded[i].key < encoded[j].key || (encoded[i].key == encoded[j].key && encoded[i].value < encoded[j].value)
	})
	values := make([]string, 0, len(encoded))
	for _, pair := range encoded {
		values = append(values, pair.key+"="+pair.value)
	}
	return strings.Join(values, "&")
}

func oauth1AuthorizationHeader(pairs []oauth1Pair) string {
	sort.Slice(pairs, func(i, j int) bool { return pairs[i].key < pairs[j].key })
	values := make([]string, 0, len(pairs))
	for _, pair := range pairs {
		values = append(values, oauth1Escape(pair.key)+"=\""+oauth1Escape(pair.value)+"\"")
	}
	return "OAuth " + strings.Join(values, ", ")
}

func oauth1Escape(value string) string {
	return strings.ReplaceAll(url.QueryEscape(value), "+", "%20")
}
