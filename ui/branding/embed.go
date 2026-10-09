// Package branding shares hosted connection presentation assets with the standalone UI build.
package branding

import _ "embed"

// HostedConnectStyles is embedded from the same UI-local asset used by the Settings preview.
//
//go:embed hosted_connect.css
var HostedConnectStyles string
