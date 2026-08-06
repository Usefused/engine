package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/Usefused/engine/internal/engine/store/migration"
	"github.com/Usefused/engine/internal/shared/config"
	"github.com/Usefused/engine/internal/shared/db"
	"github.com/spf13/cobra"
)

var (
	appFamilyDryRun bool
	appFamilyApply  bool
)

var migrateAppFamiliesCmd = &cobra.Command{
	Use:   "migrate-app-families",
	Short: "Migrate SDK and MCP state to version-aware app families",
	Args:  cobra.NoArgs,
	RunE:  runAppFamilyMigration,
}

func init() {
	RootCmd.AddCommand(migrateAppFamiliesCmd)
	migrateAppFamiliesCmd.Flags().BoolVar(&appFamilyDryRun, "dry-run", false, "Print the migration report without changing data")
	migrateAppFamiliesCmd.Flags().BoolVar(&appFamilyApply, "apply", false, "Apply all conflict-free migration groups")
	migrateAppFamiliesCmd.MarkFlagsMutuallyExclusive("dry-run", "apply")
}

func runAppFamilyMigration(cmd *cobra.Command, _ []string) error {
	if appFamilyDryRun == appFamilyApply {
		return fmt.Errorf("exactly one of --dry-run or --apply is required")
	}
	cfg, err := config.Load(ConfigPath)
	if err != nil {
		return fmt.Errorf("load engine config: %w", err)
	}
	databaseURL := engineDatabaseURL(cfg)
	if databaseURL == "" {
		return fmt.Errorf("database URL is required; set FUSED_DATABASE_URL, DATABASE_URL, or database.url")
	}

	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	pool, err := db.InitEnginePostgres(ctx, databaseURL)
	if err != nil {
		return fmt.Errorf("initialize engine database: %w", err)
	}
	defer pool.Close()

	var report any
	if appFamilyDryRun {
		report, err = migration.DryRun(ctx, pool)
	} else {
		report, err = migration.Apply(ctx, pool)
	}
	if err != nil {
		return err
	}
	return writeMigrationReport(os.Stdout, report)
}

func writeMigrationReport(output anyWriter, report any) error {
	encoder := json.NewEncoder(output)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(report); err != nil {
		return fmt.Errorf("write migration report: %w", err)
	}
	return nil
}

type anyWriter interface {
	Write([]byte) (int, error)
}
