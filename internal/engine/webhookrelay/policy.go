// Package webhookrelay adapts the existing WEBHOOKS stream to authenticated remote Engine receivers.
package webhookrelay

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"regexp"
	"strings"

	"github.com/google/uuid"
	"github.com/tidwall/gjson"
)

var ErrDenied = errors.New("webhook relay authorization unavailable")
var literalPath = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*(\.[A-Za-z_][A-Za-z0-9_]*)*$`)

// Config belongs to the existing webhook registration; it declares either verified export or remote input.
type Config struct {
	Publish *Routing `json:"publish,omitempty"`
	Source  *Source  `json:"source,omitempty"`
}

// Routing contains reviewed provider facts, never a customer assertion of resource ownership.
type Routing struct {
	AuthName          string `json:"auth_name"`
	TokenResourcePath string `json:"token_resource_path"`
	TokenAppPath      string `json:"token_app_path"`
	EventResourcePath string `json:"event_resource_path"`
	EventAppPath      string `json:"event_app_path"`
	EventIDPath       string `json:"event_id_path"`
}

// Source identifies a local connection and a broker registration, without any provider secret or destination URL.
type Source struct {
	Bucket         string    `json:"bucket"`
	ConnectionID   uuid.UUID `json:"connection_id"`
	RegistrationID uuid.UUID `json:"registration_id"`
}

// Decode fails closed on unsupported policy shapes rather than silently enabling broader routing.
func Decode(raw []byte) (Config, error) {
	var c Config
	// Absent relay configuration preserves ordinary local webhook behavior.
	if len(raw) == 0 || string(raw) == "null" {
		return c, nil
	}
	d := json.NewDecoder(strings.NewReader(string(raw)))
	d.DisallowUnknownFields()
	// Invalid configuration cannot authorize transport or provider proof extraction.
	if err := d.Decode(&c); err != nil {
		return c, ErrDenied
	}
	return c, c.Validate()
}

// Validate requires mutually exclusive input/output roles and literal provider object paths.
func (c Config) Validate() error {
	// A registration cannot re-export delegated events as original provider verification.
	if c.Source != nil && c.Publish != nil {
		return ErrDenied
	}
	// Remote sources must select one exact local connection and one exact broker registration.
	if s := c.Source; s != nil {
		return validateSource(*s)
	}
	// An ordinary local registration has no relay behavior.
	if c.Publish == nil {
		return nil
	}
	return c.Publish.Validate()
}

// validateSource requires complete explicit references before any connection lookup.
func validateSource(s Source) error {
	// Zero IDs and absent buckets must never become wildcard selectors.
	if strings.TrimSpace(s.Bucket) == "" || s.ConnectionID == uuid.Nil || s.RegistrationID == uuid.Nil {
		return ErrDenied
	}
	return nil
}

// Validate rejects query operators, arrays and fallback paths at the ownership boundary.
func (p Routing) Validate() error {
	// A named OAuth publication is required to identify the app whose response proves ownership.
	if p.AuthName == "" || len(p.AuthName) > 128 {
		return ErrDenied
	}
	// Simple object traversal has one unambiguous value and cannot fan out across resources.
	for _, path := range []string{p.TokenResourcePath, p.TokenAppPath, p.EventResourcePath, p.EventAppPath, p.EventIDPath} {
		// Queries and unbounded expressions are inappropriate for authorization decisions.
		if len(path) > 256 || !literalPath.MatchString(path) {
			return ErrDenied
		}
	}
	return nil
}

// StringClaim accepts only bounded nonempty JSON strings; numbers and compound values are not identities.
func StringClaim(body []byte, path string) (string, error) {
	v := gjson.GetBytes(body, path)
	// Missing, coerced or oversized claims cannot authorize a provider resource.
	if v.Type != gjson.String || v.Str == "" || len(v.Str) > 512 {
		return "", ErrDenied
	}
	return v.Str, nil
}

// Digest creates non-secret stable identity fences without placing provider identities in subjects or logs.
func Digest(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

// eventIdentity admits only the exact app/resource proven by the broker's token exchange.
func eventIdentity(payload []byte, p Routing, resource, app string) (string, error) {
	gotResource, e1 := StringClaim(payload, "body."+p.EventResourcePath)
	gotApp, e2 := StringClaim(payload, "body."+p.EventAppPath)
	// Both dimensions are necessary: a provider workspace alone does not identify an OAuth app installation.
	if e1 != nil || e2 != nil || gotResource != resource || gotApp != app {
		return "", ErrDenied
	}
	return StringClaim(payload, "body."+p.EventIDPath)
}
