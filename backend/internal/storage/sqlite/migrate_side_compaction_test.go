package sqlite

import (
	"reflect"
	"testing"

	"github.com/pressly/goose/v3"
)

func TestSideCompactionDownRefusesData(t *testing.T) {
	const version int64 = 20261104151736910
	for _, table := range []string{"chat_side_compactions", "chat_side_contents"} {
		t.Run(table, func(t *testing.T) {
			db := openMigratedDatabaseCopy(t, version-1)
			upTo(t, db, version)
			downTo(t, db, version-1)
			upTo(t, db, version)
			insert := `INSERT INTO chat_side_contents VALUES('side','{"draft":"saved"}')`
			if table == "chat_side_compactions" {
				insert = `INSERT INTO chat_side_compactions(id,side_id,native_home,native_id,epoch,state,record_json) VALUES('c','side','/home/provider','native','epoch','pending','{}')`
			}
			if _, err := db.Exec(insert); err != nil {
				t.Fatal(err)
			}
			before := migrationTableFacts(t, db, table)
			gooseMu.Lock()
			goose.SetBaseFS(migrationsFS)
			goose.SetLogger(goose.NopLogger())
			err := goose.SetDialect("sqlite3")
			if err == nil {
				err = migrateDownTo(db, version-1)
			}
			gooseMu.Unlock()
			if err == nil || !reflect.DeepEqual(before, migrationTableFacts(t, db, table)) {
				t.Fatalf("Down changed durable data: %v", err)
			}
		})
	}
}
