// Package testutil provides shared helpers for extension tests: a temp
// SQLite database, migration, and cleanup.
package testutil

import (
	"path/filepath"
	"testing"

	"github.com/sagerenn/openlist/internal/db"
)

// SetupDB initializes a fresh extension database in a temp directory.
// Feature packages self-register their models via init(), so db.Init
// auto-migrates them automatically. Returns a cleanup function.
//
// A blank import of at least one feature package is required in the test
// binary to trigger init() registration; each feature's own _test.go
// guarantees that.
func SetupDB(t *testing.T) (cleanup func()) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "ext-test.db")
	if err := db.Init(path); err != nil {
		t.Fatalf("init db: %v", err)
	}
	return func() {
		if err := db.Close(); err != nil {
			t.Logf("close db: %v", err)
		}
	}
}

// Reset wipes all rows from extension tables between subtests.
func Reset(t *testing.T) {
	t.Helper()
	if err := db.ResetForTest(); err != nil {
		t.Fatalf("reset db: %v", err)
	}
}
