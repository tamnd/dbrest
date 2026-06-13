package main

import (
	"database/sql"
	"testing"

	"github.com/tamnd/dbrest/backend"
	"github.com/tamnd/dbrest/config"
)

func sqlPool(t *testing.T, be backend.Backend) *sql.DB {
	t.Helper()
	d, ok := be.(interface{ DB() *sql.DB })
	if !ok {
		t.Fatal("backend does not expose its sql.DB")
	}
	return d.DB()
}

// TestApplyPoolConfigSizesTheSQLPool checks the database/sql settings reach
// the pool. The sqlite driver is just a convenient pool carrier here; the
// config names another engine so the resize branch runs.
func TestApplyPoolConfigSizesTheSQLPool(t *testing.T) {
	be, err := backend.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer be.Close()

	cfg, err := config.FromMap(map[string]string{
		"db-uri": "x", "db-backend": "mysql",
		"db-pool": "7", "db-pool-max-idletime": "60", "db-pool-max-lifetime": "120",
	})
	if err != nil {
		t.Fatal(err)
	}
	applyPoolConfig(be, cfg)

	if got := sqlPool(t, be).Stats().MaxOpenConnections; got != 7 {
		t.Errorf("MaxOpenConnections = %d, want the configured db-pool 7", got)
	}
}

// TestApplyPoolConfigLeavesSQLiteAlone pins the exemption: the sqlite backend
// runs on one pinned connection so its foreign-key PRAGMA holds, and the pool
// options must not resize it.
func TestApplyPoolConfigLeavesSQLiteAlone(t *testing.T) {
	be, err := backend.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer be.Close()

	cfg, err := config.FromMap(map[string]string{"db-uri": "x", "db-pool": "7"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Backend != config.BackendSQLite {
		t.Fatalf("default backend = %q, want sqlite", cfg.Backend)
	}
	applyPoolConfig(be, cfg)

	if got := sqlPool(t, be).Stats().MaxOpenConnections; got != 1 {
		t.Errorf("MaxOpenConnections = %d, the sqlite single-connection pin was lost", got)
	}
}
