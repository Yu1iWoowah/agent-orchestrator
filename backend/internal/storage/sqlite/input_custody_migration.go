package sqlite

import (
	"database/sql"
	"errors"
	"fmt"

	"github.com/pressly/goose/v3"
)

const sharedInputCustodyVersion int64 = 20261101152248341

// migrateUpTo runs under gooseMu, before publishing either connection pool to
// services. The daemon's data-dir/lifecycle lock excludes an older controller.
// Only the turn rebuild disables FK enforcement, before Goose opens its native
// transaction; its SQL checks every FK before committing schema and version.
func migrateUpTo(db *sql.DB, version int64, opts ...goose.OptionsFunc) error {
	if version < sharedInputCustodyVersion {
		return goose.UpTo(db, "migrations", version, opts...)
	}
	if err := goose.UpTo(db, "migrations", sharedInputCustodyVersion-1, opts...); err != nil {
		return err
	}
	if err := withInputRebuildForeignKeys(db, func() error {
		return goose.UpTo(db, "migrations", sharedInputCustodyVersion, opts...)
	}); err != nil {
		return err
	}
	return goose.UpTo(db, "migrations", version, opts...)
}

func withInputRebuildForeignKeys(db *sql.DB, run func() error) (result error) {
	// There are no borrowers at this startup boundary. Pin all operations to
	// the writer connection so pragma and transaction cannot use different ones.
	db.SetMaxOpenConns(1)
	var foreignKeys int
	if err := db.QueryRow(`PRAGMA foreign_keys`).Scan(&foreignKeys); err != nil {
		return err
	}
	if _, err := db.Exec(`PRAGMA foreign_keys=OFF`); err != nil {
		return err
	}
	defer func() {
		if foreignKeys != 0 {
			if _, err := db.Exec(`PRAGMA foreign_keys=ON`); err != nil {
				result = errors.Join(result, fmt.Errorf("restore input-rebuild foreign keys: %w", err))
			}
		}
	}()
	return run()
}
