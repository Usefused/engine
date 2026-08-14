package db

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

const defaultEngineDatabaseMaxConns int32 = 10

const defaultEngineDatabaseMaxConnIdleTime = 30 * time.Minute

func InitEnginePostgres(ctx context.Context, dsn string) (*pgxpool.Pool, error) {
	return initPostgresWithSchema(ctx, dsn, initEngineSchema)
}

func initPostgresWithSchema(
	ctx context.Context,
	dsn string,
	schemaInitFunc func(context.Context, *pgxpool.Pool) error,
) (*pgxpool.Pool, error) {
	config, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, err
	}
	if err := applyEngineDatabasePoolPolicy(config); err != nil {
		return nil, err
	}
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		return nil, err
	}

	// Engine owns its local runtime tables, so startup must reconcile that schema
	// even when Registry migrations are disabled in a neighbouring deployment.
	if err := schemaInitFunc(ctx, pool); err != nil {
		pool.Close()
		return nil, fmt.Errorf("initialize Engine schema: %w", err)
	}

	return pool, nil
}

func applyEngineDatabasePoolPolicy(config *pgxpool.Config) error {
	maxConns, err := databaseMaxConns("FUSED_DATABASE_MAX_CONNS", defaultEngineDatabaseMaxConns)
	if err != nil {
		return err
	}
	idleTime, err := databaseMaxConnIdleTime("FUSED_DATABASE_MAX_CONN_IDLE_TIME", defaultEngineDatabaseMaxConnIdleTime)
	if err != nil {
		return err
	}
	config.MaxConns = maxConns
	config.MaxConnIdleTime = idleTime
	// Hosted and self-hosted Engines should pay the connection cost only while
	// doing work; DSN parameters must not silently keep provider slots warm.
	config.MinConns = 0
	config.MinIdleConns = 0
	return nil
}

func databaseMaxConns(name string, fallback int32) (int32, error) {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback, nil
	}
	value, err := strconv.ParseInt(raw, 10, 32)
	if err != nil || value < 1 {
		return 0, fmt.Errorf("%s must be a positive integer", name)
	}
	return int32(value), nil
}

func databaseMaxConnIdleTime(name string, fallback time.Duration) (time.Duration, error) {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback, nil
	}
	value, err := time.ParseDuration(raw)
	if err != nil || value <= 0 {
		return 0, fmt.Errorf("%s must be a positive duration", name)
	}
	return value, nil
}
