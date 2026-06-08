package mysql_test

import (
	"context"
	"os"
	"testing"

	"github.com/tamnd/dbrest/backend/mysql"
)

// TestIntegration requires a live MySQL/MariaDB server. Set DBREST_MYSQL_DSN
// to enable these tests:
//
//	DBREST_MYSQL_DSN="dbrest:Dbrest!Passw0rd@tcp(127.0.0.1:3306)/dbrest?parseTime=true" \
//	  go test ./backend/mysql/ -v -run TestIntegration
//
// Start the server with:
//
//	podman compose -f docker/dbrest-mysql/compose.yaml up -d
func TestIntegration(t *testing.T) {
	dsn := os.Getenv("DBREST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("DBREST_MYSQL_DSN not set; skipping MySQL integration tests")
	}
	ctx := context.Background()

	be, err := mysql.Open(dsn)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer be.Close()

	t.Run("ServerVersion", func(t *testing.T) {
		v := be.ServerVersion()
		if v.Major < 5 {
			t.Errorf("unexpected version %+v", v)
		}
		t.Logf("version: %+v", v)
	})

	t.Run("Capabilities", func(t *testing.T) {
		caps := be.Capabilities()
		t.Logf("Returning=%v MariaDB=%v", caps.Returning, be.ServerVersion().MariaDB)
	})

	t.Run("Introspect", func(t *testing.T) {
		model, err := be.Introspect(ctx)
		if err != nil {
			t.Fatalf("Introspect: %v", err)
		}
		if model.Len() == 0 {
			t.Error("Introspect returned empty model")
		}
		t.Logf("relations: %d", model.Len())
		for _, rel := range model.Relations() {
			t.Logf("  %s (%d cols, pk=%v, fks=%d)", rel.Name, len(rel.Columns), rel.PrimaryKey, len(rel.ForeignKeys))
		}
	})
}
