package db

import (
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestEngineDatabaseMaxConns(t *testing.T) {
	for _, test := range []struct {
		name    string
		value   string
		want    int32
		wantErr bool
	}{
		{name: "default", want: defaultEngineDatabaseMaxConns},
		{name: "override", value: "2", want: 2},
		{name: "zero", value: "0", wantErr: true},
		{name: "invalid", value: "many", wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("FUSED_DATABASE_MAX_CONNS", test.value)
			got, err := databaseMaxConns("FUSED_DATABASE_MAX_CONNS", defaultEngineDatabaseMaxConns)
			if (err != nil) != test.wantErr {
				t.Fatalf("databaseMaxConns() error = %v, wantErr %v", err, test.wantErr)
			}
			if got != test.want {
				t.Fatalf("databaseMaxConns() = %d, want %d", got, test.want)
			}
		})
	}
}

func TestEnginePoolCanDrainCompletely(t *testing.T) {
	t.Setenv("FUSED_DATABASE_MAX_CONNS", "2")
	t.Setenv("FUSED_DATABASE_MAX_CONN_IDLE_TIME", "2m")
	config, err := pgxpool.ParseConfig("postgres://user:secret@db.example.test/engine?pool_min_conns=2&pool_min_idle_conns=2")
	if err != nil {
		t.Fatal(err)
	}
	if err := applyEngineDatabasePoolPolicy(config); err != nil {
		t.Fatal(err)
	}
	if config.MinConns != 0 || config.MinIdleConns != 0 {
		t.Fatalf("pool minimums must be zero: MinConns=%d MinIdleConns=%d", config.MinConns, config.MinIdleConns)
	}
}

func TestEngineDatabaseMaxConnIdleTime(t *testing.T) {
	for _, test := range []struct {
		name    string
		value   string
		want    time.Duration
		wantErr bool
	}{
		{name: "default", want: defaultEngineDatabaseMaxConnIdleTime},
		{name: "override", value: "2m", want: 2 * time.Minute},
		{name: "zero", value: "0s", wantErr: true},
		{name: "invalid", value: "later", wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("FUSED_DATABASE_MAX_CONN_IDLE_TIME", test.value)
			got, err := databaseMaxConnIdleTime("FUSED_DATABASE_MAX_CONN_IDLE_TIME", defaultEngineDatabaseMaxConnIdleTime)
			if (err != nil) != test.wantErr {
				t.Fatalf("databaseMaxConnIdleTime() error = %v, wantErr %v", err, test.wantErr)
			}
			if got != test.want {
				t.Fatalf("databaseMaxConnIdleTime() = %s, want %s", got, test.want)
			}
		})
	}
}
