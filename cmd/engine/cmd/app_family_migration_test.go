package cmd

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRunAppFamilyMigrationRequiresOneMode(t *testing.T) {
	originalDryRun, originalApply := appFamilyDryRun, appFamilyApply
	t.Cleanup(func() {
		appFamilyDryRun, appFamilyApply = originalDryRun, originalApply
	})

	for _, modes := range [][2]bool{{false, false}, {true, true}} {
		appFamilyDryRun, appFamilyApply = modes[0], modes[1]
		err := runAppFamilyMigration(migrateAppFamiliesCmd, nil)
		require.Error(t, err)
		assert.ErrorContains(t, err, "exactly one")
	}
}

func TestWriteMigrationReportWritesIndentedJSON(t *testing.T) {
	var output bytes.Buffer
	require.NoError(t, writeMigrationReport(&output, map[string]int{"families": 2}))

	var decoded map[string]int
	require.NoError(t, json.Unmarshal(output.Bytes(), &decoded))
	assert.Equal(t, 2, decoded["families"])
	assert.Contains(t, output.String(), "\n  \"families\"")
}

func TestEngineDatabaseURLPrecedence(t *testing.T) {
	t.Setenv("FUSED_DATABASE_URL", "postgres://fused")
	t.Setenv("DATABASE_URL", "postgres://generic")
	assert.Equal(t, "postgres://fused", engineDatabaseURL(nil))

	t.Setenv("FUSED_DATABASE_URL", "")
	assert.Equal(t, "postgres://generic", engineDatabaseURL(nil))
}
