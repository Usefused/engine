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

// SearchableSelectScript is the same module imported by the React Select adapter.
//
//go:embed searchable_select.js
var SearchableSelectScript string

// SelectStyles is shared by the dashboard and standalone consent page.
//
//go:embed select.css
var SelectStyles string

// oauthConsentScript initializes shared controls and enhances the operation picker.
//
//go:embed oauth_consent.js
var oauthConsentScript string

// OAuthConsentScript embeds the shared control without a separate build or a copied implementation.
var OAuthConsentScript = SearchableSelectScript + "\n" + oauthConsentScript
