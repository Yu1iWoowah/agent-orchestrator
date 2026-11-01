package sqlite

import "testing"

func TestOperatorConsentMigrationUpDownUp(t *testing.T) {
	const previous int64 = 20261101143810287
	const version int64 = 20261101165111979
	db := openMigratedDatabaseCopy(t, previous)
	for i := 0; i < 2; i++ {
		upTo(t, db, version)
		for _, table := range []string{"operator_confirmation_requests", "operator_consents"} {
			var count int
			if err := db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&count); err != nil || count != 1 {
				t.Fatalf("%s count %d: %v", table, count, err)
			}
		}
		if i == 0 {
			downTo(t, db, previous)
		}
	}
	var violations int
	rows, err := db.Query(`PRAGMA foreign_key_check`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		violations++
	}
	if err = rows.Err(); err != nil || violations != 0 {
		t.Fatalf("foreign key violations %d: %v", violations, err)
	}
}
