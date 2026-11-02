package sqlite

import (
	"reflect"
	"testing"

	"github.com/pressly/goose/v3"
)

const ownerInboxCoreVersion int64 = 20261102125818565

func TestHOwnerInboxMigrationPreservesLegacyObjects(t *testing.T) {
	db := openMigratedDatabaseCopy(t, ownerInboxCoreVersion-1)
	seedSharedInputMigration(t, db)
	before := sharedInputObjects(t, db)
	upTo(t, db, ownerInboxCoreVersion)
	after := sharedInputObjects(t, db)
	for key, definition := range before {
		if after[key] != definition {
			t.Errorf("legacy schema changed: %s", key)
		}
	}
	assertSharedInputRows(t, db)
	downTo(t, db, ownerInboxCoreVersion-1)
	if !reflect.DeepEqual(sharedInputObjects(t, db), before) {
		t.Fatal("unused core Down changed legacy schema")
	}
	upTo(t, db, ownerInboxCoreVersion)
	if !reflect.DeepEqual(sharedInputObjects(t, db), after) {
		t.Fatal("core up/down/up drift")
	}
}

func TestHOwnerInboxMigrationRefusesDownAfterUse(t *testing.T) {
	db := openMigratedDatabaseCopy(t, ownerInboxCoreVersion)
	seedSharedInputMigration(t, db)
	if _, err := db.Exec(`INSERT INTO owner_inbox_actions(project_id,action_key,stage,fingerprint,receipt_json) VALUES('project','retained','accept','fingerprint','{}')`); err != nil {
		t.Fatal(err)
	}
	before := sharedInputObjects(t, db)
	gooseMu.Lock()
	goose.SetBaseFS(migrationsFS)
	err := goose.DownTo(db, "migrations", ownerInboxCoreVersion-1)
	gooseMu.Unlock()
	if err == nil {
		t.Fatal("critical receipts allowed Down")
	}
	if !reflect.DeepEqual(sharedInputObjects(t, db), before) {
		t.Fatal("refused Down changed schema")
	}
	var count int
	if err := db.QueryRow(`SELECT count(*) FROM owner_inbox_actions WHERE action_key='retained'`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("lost receipt: count=%d err=%v", count, err)
	}
}
