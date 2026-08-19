package store

import (
	"context"
	"fmt"
)

const connectBrandingColumns = `connect_display_name, connect_logo_url,
connect_primary_color, connect_support_url, connect_privacy_url`

// upsertConnectBrandingSQL retains prior customization evidence and marks only
// a real colour change while replacing every public branding field atomically.
const upsertConnectBrandingSQL = `UPDATE fused_workspaces SET
	connect_display_name = $1,
	connect_logo_url = $2,
	connect_primary_color_customized = connect_primary_color_customized OR connect_primary_color IS DISTINCT FROM $3,
	connect_primary_color = $3,
	connect_support_url = $4,
	connect_privacy_url = $5,
	updated_at = NOW()
	WHERE singleton_key = 1
	RETURNING ` + connectBrandingColumns

// GetConnectBranding loads the one Engine workspace presentation row without
// loading unrelated workspace state into memory.
func (s *postgresStore) GetConnectBranding(ctx context.Context) (ConnectBranding, error) {
	query := `SELECT ` + connectBrandingColumns + `
		FROM fused_workspaces
		WHERE singleton_key = 1`
	return scanConnectBranding(s.db.QueryRow(ctx, query))
}

// UpsertConnectBranding replaces every public branding field together so
// hosted pages never observe a partially updated visual identity.
func (s *postgresStore) UpsertConnectBranding(ctx context.Context, branding ConnectBranding) (ConnectBranding, error) {
	return scanConnectBranding(s.db.QueryRow(ctx, upsertConnectBrandingSQL,
		branding.DisplayName,
		branding.LogoURL,
		branding.PrimaryColor,
		branding.SupportURL,
		branding.PrivacyURL,
	))
}

// connectBrandingRow is the narrow scan capability shared by query and update.
type connectBrandingRow interface {
	Scan(dest ...any) error
}

// scanConnectBranding keeps the SQL column order and public projection in one place.
func scanConnectBranding(row connectBrandingRow) (ConnectBranding, error) {
	var branding ConnectBranding
	if err := row.Scan(
		&branding.DisplayName,
		&branding.LogoURL,
		&branding.PrimaryColor,
		&branding.SupportURL,
		&branding.PrivacyURL,
	); err != nil {
		// SQL errors stay wrapped at the storage boundary without changing the
		// public projection or loading a fallback row in application memory.
		return ConnectBranding{}, fmt.Errorf("read connect branding: %w", err)
	}
	return branding, nil
}
