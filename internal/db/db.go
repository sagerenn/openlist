// Package db provides the extension's own persistence layer.
//
// It is intentionally a SEPARATE database from OpenList's. All extension
// state (per-user domain, TTL config, list-permission flags, API keys,
// load-balance groups, file TTL records) is stored here, keyed by the
// OpenList user ID. This keeps the extension forward-compatible with
// future OpenList versions: OpenList's schema and migrations are never
// touched, and an OpenList upgrade cannot break or be broken by these
// tables.
package db

import (
	"sync"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// Models is populated by internal/migrate with every extension model that
// must be auto-migrated. Keeping the list out of this package avoids an
// import cycle (feature packages import db; db must not import them).
var Models []interface{}

var (
	mu  sync.Mutex
	db  *gorm.DB
	err error
)

// DB returns the shared extension database handle. It is initialized
// exactly once by Init.
func DB() *gorm.DB {
	return db
}

// Init opens (or creates) the extension SQLite database at path and
// auto-migrates all extension models. It is safe to call multiple times;
// only the first call performs initialization.
func Init(path string) error {
	mu.Lock()
	defer mu.Unlock()
	if db != nil {
		return nil
	}
	db, err = gorm.Open(sqlite.Open(path), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		return err
	}
	collectModels()
	if len(Models) == 0 {
		return nil
	}
	return db.AutoMigrate(Models...)
}

// Close closes the extension database.
func Close() error {
	mu.Lock()
	defer mu.Unlock()
	if db == nil {
		return nil
	}
	sqlDB, e := db.DB()
	if e != nil {
		return e
	}
	if e := sqlDB.Close(); e != nil {
		return e
	}
	db = nil
	return nil
}

// ResetForTest drops all rows from every extension table. Test helper only.
func ResetForTest() error {
	if db == nil {
		return nil
	}
	for _, m := range Models {
		if e := db.Where("1 = 1").Delete(m).Error; e != nil {
			return e
		}
	}
	return nil
}
