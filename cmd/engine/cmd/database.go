package cmd

import (
	"os"

	"github.com/Usefused/engine/internal/shared/config"
)

func engineDatabaseURL(cfg *config.Config) string {
	if databaseURL := os.Getenv("FUSED_DATABASE_URL"); databaseURL != "" {
		return databaseURL
	}
	if databaseURL := os.Getenv("DATABASE_URL"); databaseURL != "" {
		return databaseURL
	}
	if cfg == nil {
		return ""
	}
	return cfg.Database.URL
}
