// Package branding shares hosted connection presentation assets with the standalone UI build.
package branding

import _ "embed"

// HostedConnectStyles is embedded from the same UI-local asset used by the Settings preview.
//
//go:embed hosted_connect.css
var HostedConnectStyles string

// OAuthConsentStyles follows the app builder's service-card presentation in the native consent form.
//
//go:embed oauth_consent.css
var OAuthConsentStyles string

// OAuthConsentScript only filters and counts existing native inputs; its exact bytes are CSP-hashed.
//
//go:embed oauth_consent.js
var OAuthConsentScript string
