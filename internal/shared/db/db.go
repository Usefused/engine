package db

import (
	"context"
	"log"

	"github.com/jackc/pgx/v5/pgxpool"
)

func InitEnginePostgres(ctx context.Context, dsn string) (*pgxpool.Pool, error) {
	return initPostgresWithSchema(ctx, dsn, initEngineSchema)
}

func initPostgresWithSchema(ctx context.Context, dsn string, schemaInitFunc func(context.Context, *pgxpool.Pool) error) (*pgxpool.Pool, error) {
	config, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, err
	}

	config.MaxConns = 10

	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		return nil, err
	}

	// Engine owns its local runtime tables, so startup must reconcile that schema
	// even when Registry migrations are disabled in a neighbouring deployment.
	if err := schemaInitFunc(ctx, pool); err != nil {
		log.Printf("Schema initialization failed: %v", err)
		return nil, err
	}

	return pool, nil
}
