package sqlite

import (
	"database/sql"
	_ "embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/pressly/goose/v3"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// Captured from 0ea30936 (u3), b2c8f3bb (F1), and 816d7820 (F).
// u4 a74c4d4493 has identical migration paths and bytes as F. By pinning
// the sets, a later import with an old version cannot enter an older fixture.
//
//go:embed testdata/upstream_sync_fixture_migrations.tsv
var upstreamSyncFixtureMigrations string

func upstreamSyncLegacyDatabase(t *testing.T, dir string, version int64) map[string]string {
	t.Helper()
	legacy := fstest.MapFS{}
	for _, entry := range strings.Split(strings.TrimSpace(upstreamSyncFixtureMigrations), "\n") {
		snapshot, filename, ok := strings.Cut(entry, "\t")
		firstSeen, err := strconv.ParseInt(snapshot, 10, 64)
		if !ok || err != nil {
			t.Fatalf("invalid historical fixture entry %q", entry)
		}
		if firstSeen > version {
			continue
		}
		name := "migrations/" + filename
		data, err := migrationsFS.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		legacy[name] = &fstest.MapFile{Data: data}
	}
	want := map[int64]int{20261114020404081: 241, 20261115050041608: 242, 20261116001259486: 284}[version]
	if want == 0 || len(legacy) != want {
		t.Fatalf("historical fixture %d has %d migrations, want %d", version, len(legacy), want)
	}
	db, err := sql.Open("sqlite", "file:"+filepath.Join(dir, "ao.db")+snapshotDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	db.SetMaxOpenConns(1)
	gooseMu.Lock()
	defer gooseMu.Unlock()
	goose.SetBaseFS(legacy)
	defer goose.SetBaseFS(migrationsFS)
	goose.SetLogger(goose.NopLogger())
	if err = goose.SetDialect("sqlite3"); err != nil {
		t.Fatal(err)
	}
	if err = migrateUpTo(db, goose.MaxVersion, goose.WithAllowMissing()); err != nil {
		t.Fatal(err)
	}
	seedSharedInputMigration(t, db)
	return seedUpstreamProtectedRows(t, db, version)
}

func TestUpstreamSyncInstallsAndUpgrades(t *testing.T) {
	var fresh map[string]string
	for _, tc := range []struct {
		name    string
		version int64
	}{
		{"fresh", 0}, {"u3", 20261114020404081}, {"F1", 20261115050041608}, {"u4-and-F", 20261116001259486},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			var protected map[string]string
			if tc.version != 0 {
				protected = upstreamSyncLegacyDatabase(t, dir, tc.version)
			}
			store, err := Open(dir)
			if err != nil {
				t.Fatalf("production Open: %v", err)
			}
			if err = store.Close(); err != nil {
				t.Fatal(err)
			}
			db, err := sql.Open("sqlite", "file:"+filepath.Join(dir, "ao.db")+pragmas)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = db.Close() }()
			for _, field := range []struct{ table, column string }{
				{"conversation_turns", "failure_code"},
				{"model_usage_events", "service_tier"},
				{"model_usage_events", "provider_message_id"},
				{"review_run", "worker_notice"},
				{"shell_terminals", "agent_conversation_id"},
				{"kept_artifact_sessions", "kept_at"},
			} {
				var found int
				if err = db.QueryRow(`SELECT count(*) FROM pragma_table_info(?) WHERE name=?`, field.table, field.column).Scan(&found); err != nil || found != 1 {
					t.Fatalf("missing %s.%s: %d %v", field.table, field.column, found, err)
				}
			}
			if tc.version != 0 {
				assertSharedInputRows(t, db)
				for query, want := range protected {
					if got := upstreamSyncRows(t, db, query); got != want {
						t.Errorf("protected rows changed: %s\\n%s -> %s", query, want, got)
					}
				}
				var config string
				if err := db.QueryRow(`SELECT config FROM projects WHERE id='project'`).Scan(&config); err != nil || config != `{"worker":{"agentAccountId":"saved"},"agentRules":"keep"}` {
					t.Fatalf("PSS config changed: %s %v", config, err)
				}
			}
			schema := upstreamSyncSchema(t, db)
			if fresh == nil {
				fresh = schema
			} else {
				for name, want := range fresh {
					if schema[name] != want {
						t.Errorf("schema differs for %s:\\nwant %s\\ngot %s", name, want, schema[name])
					}
				}
				if len(schema) != len(fresh) {
					t.Errorf("schema objects %d, want %d", len(schema), len(fresh))
				}
			}
			assertUpstreamSyncLedger(t, db)
			assertUpstreamSyncConstraints(t, db)
			before := sharedInputObjects(t, db)
			ledger := upstreamSyncRows(t, db, `SELECT * FROM goose_db_version ORDER BY id`)
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			reopened, err := Open(dir)
			if err != nil {
				t.Fatalf("second production Open: %v", err)
			}
			if err := reopened.Close(); err != nil {
				t.Fatal(err)
			}
			db, err = sql.Open("sqlite", databaseURI(dir)+pragmas)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = db.Close() }()
			if !reflect.DeepEqual(before, sharedInputObjects(t, db)) {
				t.Fatal("second Open changed schema")
			}
			assertUpstreamSyncLedger(t, db)
			if ledger != upstreamSyncRows(t, db, `SELECT * FROM goose_db_version ORDER BY id`) {
				t.Fatal("second Open rewrote the migration ledger")
			}
		})
	}
}

func TestUpstreamSyncRebuildPreservesNonEmptyValues(t *testing.T) {
	t.Run("turn", func(t *testing.T) {
		db := openMigratedDatabaseCopy(t, sharedInputCustodyVersion-1)
		seedSharedInputMigration(t, db)
		mustExec(t, db, `UPDATE conversation_turns SET failure_code='provider_failure' WHERE id='retry'`)
		upTo(t, db, sharedInputCustodyVersion)
		var code string
		if err := db.QueryRow(`SELECT failure_code FROM conversation_turns WHERE id='retry'`).Scan(&code); err != nil || code != "provider_failure" {
			t.Fatalf("lost failure code: %q %v", code, err)
		}
	})
	t.Run("usage", func(t *testing.T) {
		db := openMigratedDatabaseCopy(t, 20261109083558284-1)
		seedSharedInputMigration(t, db)
		for _, statement := range []string{
			`INSERT INTO usage_bindings(id,session_id,harness,native_root_id,state,updated_at) VALUES(7,'worker','codex','native','active',CURRENT_TIMESTAMP)`,
			`INSERT INTO usage_sources(id,binding_id,kind,artifact_path,state,updated_at) VALUES(8,7,'codex_rollout','synthetic-usage','active',CURRENT_TIMESTAMP)`,
			`INSERT INTO model_usage_events(id,binding_id,usage_source_id,provider_id,model_id,usage_measurement_kind,source_event_key,service_tier,provider_message_id,input_tokens) VALUES(9,7,8,'openai','model','native_reported','event','priority','message',42)`,
		} {
			mustExec(t, db, statement)
		}
		upTo(t, db, 20261109083558284)
		var tier, message string
		var tokens int
		if err := db.QueryRow(`SELECT service_tier,provider_message_id,input_tokens FROM model_usage_events WHERE id=9`).Scan(&tier, &message, &tokens); err != nil || tier != "priority" || message != "message" || tokens != 42 {
			t.Fatalf("lost usage: %q %q %d %v", tier, message, tokens, err)
		}
	})
}

// Compare every schema object's full SQL, allowing only column ordinal changes
// in the three tables whose ALTERs interleave. CHECKs, FK actions, index keys,
// uniqueness, predicates, trigger bodies and view bodies remain in the comparison.
func upstreamSyncSchema(t *testing.T, db *sql.DB) map[string]string {
	t.Helper()
	result := sharedInputObjects(t, db)
	for _, table := range []string{"app_settings", "conversation_turns", "model_usage_events", "review_run", "shell_terminals"} {
		key := "table:" + table
		statement := result[key]
		start, end := strings.IndexByte(statement, '('), strings.LastIndexByte(statement, ')')
		if start < 0 || end <= start {
			t.Fatalf("invalid table SQL: %s", key)
		}
		body := statement[start+1 : end]
		var parts []string
		depth, last := 0, 0
		var quote byte
		for i := 0; i < len(body); i++ {
			c := body[i]
			if quote != 0 {
				if c == quote {
					if i+1 < len(body) && body[i+1] == quote {
						i++
					} else {
						quote = 0
					}
				}
				continue
			}
			if c == '-' && i+1 < len(body) && body[i+1] == '-' {
				for i < len(body) && body[i] != '\n' {
					i++
				}
				continue
			}
			switch c {
			case '\'', '"', '`':
				quote = c
			case '[':
				quote = ']'
			case '(':
				depth++
			case ')':
				depth--
			case ',':
				if depth == 0 {
					parts = append(parts, strings.TrimSpace(body[last:i]))
					last = i + 1
				}
			}
		}
		if depth != 0 || quote != 0 {
			t.Fatalf("unbalanced table SQL: %s", key)
		}
		parts = append(parts, strings.TrimSpace(body[last:]))
		sort.Strings(parts)
		// Only formatting between clauses varies when SQLite appends a column.
		for i := range parts {
			parts[i] = strings.Join(strings.Fields(parts[i]), " ")
		}
		sort.Strings(parts)
		result[key] = strings.Join(parts, "\n") + statement[end+1:]
		var order string
		if err := db.QueryRow(`SELECT group_concat(name,',') FROM (SELECT name FROM pragma_table_xinfo(?) ORDER BY cid)`, table).Scan(&order); err != nil {
			t.Fatal(err)
		}
		t.Logf("%s column order: %s", table, order)
	}
	return result
}

func assertUpstreamSyncLedger(t *testing.T, db *sql.DB) {
	t.Helper()
	assertUpstreamSyncLedgerThrough(t, db, goose.MaxVersion)
	var total int
	if err := db.QueryRow(`SELECT count(*) FROM goose_db_version WHERE version_id<>0`).Scan(&total); err != nil || total != len(shippedMigrations) {
		t.Fatalf("ledger has %d receipts, want fixed manifest of %d: %v", total, len(shippedMigrations), err)
	}
}

func assertUpstreamSyncLedgerThrough(t *testing.T, db *sql.DB, lastVersion int64) {
	t.Helper()
	entries, err := fs.ReadDir(migrationsFS, "migrations")
	if err != nil {
		t.Fatal(err)
	}
	var checked int
	for _, entry := range entries {
		version, err := strconv.ParseInt(strings.SplitN(entry.Name(), "_", 2)[0], 10, 64)
		if err != nil {
			t.Fatal(err)
		}
		if version > lastVersion {
			continue
		}
		checked++
		var applied, total int
		if err := db.QueryRow(`SELECT count(*),COALESCE(sum(is_applied),0) FROM goose_db_version WHERE version_id=?`, version).Scan(&total, &applied); err != nil {
			t.Fatal(err)
		}
		if total != 1 || applied != 1 {
			t.Fatalf("migration %s receipts=%d applied=%d", entry.Name(), total, applied)
		}
	}
	var copies, foreignKeys int
	if err := db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE name LIKE '_upstream_sync_%'`).Scan(&copies); err != nil || copies != 0 {
		t.Fatalf("left startup copies=%d %v", copies, err)
	}
	if err := db.QueryRow(`PRAGMA foreign_keys`).Scan(&foreignKeys); err != nil || foreignKeys != 1 {
		t.Fatalf("foreign_keys=%d %v", foreignKeys, err)
	}
	rows, err := db.Query(`PRAGMA foreign_key_check`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	if rows.Next() {
		t.Fatal("foreign_key_check found a violation")
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	t.Logf("verified one applied receipt for each of %d migrations", checked)
}

func assertUpstreamSyncConstraints(t *testing.T, db *sql.DB) {
	t.Helper()
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	// A provenance-valid event isolates the new CHECK/UNIQUE constraints.
	for _, statement := range []string{
		`INSERT INTO projects(id,path,registered_at) VALUES('check-p','isolated-check-project',CURRENT_TIMESTAMP)`,
		`INSERT INTO sessions(id,project_id,num,activity_last_at,created_at,updated_at) VALUES('check-s','check-p',1,CURRENT_TIMESTAMP,CURRENT_TIMESTAMP,CURRENT_TIMESTAMP)`,
		`INSERT INTO usage_execution_sources(id,project_id,session_id,billing_provider,profile_id,profile_revision,profile_digest,delegation_id,attempt_id,request_id,receipt_digest,identity_json) VALUES(180,'check-p','check-s','openai','profile','v1','digest','delegation','attempt','request','receipt','{}')`,
		`INSERT INTO model_usage_events(id,execution_source_id,provider_id,model_id,usage_measurement_kind,source_event_key,service_tier,provider_message_id,created_at) VALUES(180,180,'openai','model','provider_reported','check-event','priority','check-message',CURRENT_TIMESTAMP)`,
	} {
		if _, err := tx.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct{ column, value string }{
		{"service_tier", " "}, {"service_tier", strings.Repeat("x", 65)},
		{"provider_message_id", " "}, {"provider_message_id", strings.Repeat("x", 257)},
	} {
		if _, err := tx.Exec("UPDATE model_usage_events SET "+tc.column+"=? WHERE id=180", tc.value); err == nil {
			t.Fatalf("accepted invalid %s", tc.column)
		}
	}
	if _, err := tx.Exec(`INSERT INTO usage_execution_sources(id,project_id,session_id,billing_provider,profile_id,profile_revision,profile_digest,delegation_id,attempt_id,request_id,receipt_digest,identity_json) VALUES(181,'check-p','check-s','openai','profile','v1','digest','delegation','another-attempt','request','receipt','{}')`); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(`INSERT INTO model_usage_events(execution_source_id,provider_id,model_id,usage_measurement_kind,source_event_key,provider_message_id,created_at) VALUES(181,'openai','model','provider_reported','another-event','check-message',CURRENT_TIMESTAMP)`); err == nil {
		t.Fatal("accepted duplicate provider message")
	}
}

func TestUpstreamSyncRestartAfterIndexPreparation(t *testing.T) {
	for _, version := range []int64{upstreamDeleteIndexesVersion, forkLineageIndexesVersion} {
		t.Run(strconv.FormatInt(version, 10), func(t *testing.T) {
			dir := t.TempDir()
			db := openMigratedDatabaseCopyAt(t, dir, version-1, pragmas)
			seedSharedInputMigration(t, db)
			if err := prepareUpstreamTurnIndexes(db, version); err != nil {
				t.Fatal(err)
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			store, err := Open(dir)
			if err != nil {
				t.Fatal(err)
			}
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			db, err = sql.Open("sqlite", databaseURI(dir)+pragmas)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = db.Close() }()
			assertSharedInputRows(t, db)
			assertUpstreamSyncLedger(t, db)
			for _, table := range []string{"conversation_activities", "conversation_messages"} {
				var columns string
				if err := db.QueryRow(`SELECT group_concat(name,',') FROM pragma_index_info(?)`, "idx_"+table+"_turn").Scan(&columns); err != nil || columns != "conversation_id,turn_id,sequence" {
					t.Fatalf("lineage index %s: %s %v", table, columns, err)
				}
				if err := db.QueryRow(`SELECT group_concat(name,',') FROM pragma_index_info(?)`, "idx_"+table+"_turn_delete").Scan(&columns); err != nil || columns != "turn_id" {
					t.Fatalf("delete index %s: %s %v", table, columns, err)
				}
			}
			var reviewIndex int
			if err := db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE name='idx_conversation_provider_events_review'`).Scan(&reviewIndex); err != nil || reviewIndex != 1 {
				t.Fatalf("omitted other U migration effect: %d %v", reviewIndex, err)
			}
		})
	}
}

func TestUpstreamSyncSchemaAdoptsCRLF(t *testing.T) {
	db := openMigratedDatabaseCopy(t, upstreamSyncSchemaVersion-1)
	seedSharedInputMigration(t, db)
	seedUpstreamRepriceNativeEvent(t, db)
	mustExec(t, db, `UPDATE conversation_turns SET failure_code='retained' WHERE id='retry'`)
	mustExec(t, db, `UPDATE model_usage_events SET service_tier='priority',provider_message_id='message' WHERE id=81`)
	query := `SELECT failure_code,service_tier,provider_message_id FROM conversation_turns JOIN model_usage_events ON model_usage_events.id=81 WHERE conversation_turns.id='retry'`
	before := upstreamSyncRows(t, db, query)
	source, err := migrationsFS.ReadFile("migrations/20261116001259486_upstream_sync_schema.sql")
	if err != nil {
		t.Fatal(err)
	}
	crlf := strings.ReplaceAll(strings.ReplaceAll(string(source), "\r\n", "\n"), "\n", "\r\n")
	if err := applyUpstreamSyncSchema(db, []byte(crlf)); err != nil {
		t.Fatalf("adopt existing columns from CRLF SQL: %v", err)
	}
	if got := upstreamSyncRows(t, db, query); got != before {
		t.Fatalf("CRLF adoption changed values: %s -> %s", before, got)
	}
	assertUpstreamSyncLedgerThrough(t, db, upstreamSyncSchemaVersion)
	assertUpstreamSyncConstraints(t, db)
}

func TestUpstreamSyncSchemaReceiptFailureIsAtomic(t *testing.T) {
	dir := t.TempDir()
	db := openMigratedDatabaseCopyAt(t, dir, upstreamSyncSchemaVersion-1, pragmas)
	seedSharedInputMigration(t, db)
	mustExec(t, db, `UPDATE conversation_turns SET failure_code='retained' WHERE id='retry'`)
	mustExec(t, db, fmt.Sprintf(`CREATE TRIGGER fail_sync_receipt BEFORE INSERT ON goose_db_version WHEN NEW.version_id=%d BEGIN SELECT RAISE(ABORT,'injected receipt failure'); END`, upstreamSyncSchemaVersion))
	before := sharedInputObjects(t, db)
	facts := upstreamSyncRows(t, db, `SELECT * FROM conversation_turns ORDER BY id`) + upstreamSyncRows(t, db, `SELECT * FROM goose_db_version ORDER BY id`)
	if _, err := Open(dir); err == nil || !strings.Contains(err.Error(), "injected receipt failure") {
		t.Fatalf("unexpected Open result: %v", err)
	}
	if !reflect.DeepEqual(before, sharedInputObjects(t, db)) || facts != upstreamSyncRows(t, db, `SELECT * FROM conversation_turns ORDER BY id`)+upstreamSyncRows(t, db, `SELECT * FROM goose_db_version ORDER BY id`) {
		t.Fatal("failed Vsync changed schema, rows or ledger")
	}
	mustExec(t, db, `DROP TRIGGER fail_sync_receipt`)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	store, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = sql.Open("sqlite", databaseURI(dir)+pragmas)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	var value string
	if err := db.QueryRow(`SELECT failure_code FROM conversation_turns WHERE id='retry'`).Scan(&value); err != nil || value != "retained" {
		t.Fatalf("failure_code=%s %v", value, err)
	}
	assertUpstreamSyncLedger(t, db)
}

func TestUpstreamSyncRejectsUnknownShapes(t *testing.T) {
	for _, tc := range []struct {
		name    string
		version int64
		sql     string
	}{
		{"column_type", upstreamSyncSchemaVersion, `ALTER TABLE model_usage_events DROP COLUMN service_tier; ALTER TABLE model_usage_events ADD COLUMN service_tier INTEGER`},
		{"column_check", upstreamSyncSchemaVersion, `ALTER TABLE model_usage_events DROP COLUMN service_tier; ALTER TABLE model_usage_events ADD COLUMN service_tier TEXT`},
		{"column_check_literal", upstreamSyncSchemaVersion, `ALTER TABLE model_usage_events DROP COLUMN service_tier; ALTER TABLE model_usage_events ADD COLUMN service_tier TEXT CHECK (service_tier IS NULL OR (trim(service_tier) <> ' ' AND length(service_tier) <= 64))`},
		{"index_predicate_literal", upstreamSyncSchemaVersion, `DROP INDEX idx_conversation_turns_failed_recent; CREATE INDEX idx_conversation_turns_failed_recent ON conversation_turns(completed_at) WHERE state='FAILED'`},
		{"column_extra_check", upstreamSyncSchemaVersion, `ALTER TABLE model_usage_events DROP COLUMN service_tier; ALTER TABLE model_usage_events ADD COLUMN service_tier TEXT CHECK (service_tier IS NULL OR (trim(service_tier) <> '' AND length(service_tier) <= 64)) CHECK(length(service_tier)<8)`},
		{"column_default", upstreamSyncSchemaVersion, `DROP INDEX idx_conversation_turns_failed_recent; ALTER TABLE conversation_turns DROP COLUMN failure_code; ALTER TABLE conversation_turns ADD COLUMN failure_code TEXT NOT NULL DEFAULT 'other'`},
		{"index_shape", upstreamSyncSchemaVersion, `DROP INDEX idx_model_usage_events_provider_message; CREATE INDEX idx_model_usage_events_provider_message ON model_usage_events(provider_message_id)`},
		{"index_identifier_case", forkLineageIndexesVersion, `DROP INDEX idx_conversation_activities_turn; CREATE INDEX IDX_CONVERSATION_ACTIVITIES_TURN ON conversation_activities(sequence)`},
		{"index_preparation", forkLineageIndexesVersion, `DROP INDEX idx_conversation_activities_turn; CREATE INDEX idx_conversation_activities_turn ON conversation_activities(sequence)`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			db := openMigratedDatabaseCopyAt(t, dir, tc.version-1, pragmas)
			mustExec(t, db, tc.sql)
			before := sharedInputObjects(t, db)
			var receiptBefore int
			if err := db.QueryRow(`SELECT count(*) FROM goose_db_version`).Scan(&receiptBefore); err != nil {
				t.Fatal(err)
			}
			if store, err := Open(dir); err == nil {
				_ = store.Close()
				t.Fatal("accepted unknown schema")
			} else if !strings.Contains(err.Error(), "unknown definition") {
				t.Fatal(err)
			}
			var receiptAfter int
			if err := db.QueryRow(`SELECT count(*) FROM goose_db_version`).Scan(&receiptAfter); err != nil {
				t.Fatal(err)
			}
			if receiptBefore != receiptAfter || !reflect.DeepEqual(before, sharedInputObjects(t, db)) {
				after := sharedInputObjects(t, db)
				for name, want := range before {
					if after[name] != want {
						t.Logf("changed object %s", name)
					}
				}
				for name := range after {
					if _, ok := before[name]; !ok {
						t.Logf("new object %s", name)
					}
				}
				t.Fatalf("rejection changed schema or ledger: receipts %d -> %d", receiptBefore, receiptAfter)
			}
		})
	}
}

func upstreamSyncRows(t *testing.T, db *sql.DB, query string) string {
	t.Helper()
	rows, err := db.Query(query)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	columns, err := rows.Columns()
	if err != nil {
		t.Fatal(err)
	}
	var result [][]any
	for rows.Next() {
		values := make([]any, len(columns))
		pointers := make([]any, len(columns))
		for i := range values {
			pointers[i] = &values[i]
		}
		if err := rows.Scan(pointers...); err != nil {
			t.Fatal(err)
		}
		result = append(result, values)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

func seedUpstreamProtectedRows(t *testing.T, db *sql.DB, version int64) map[string]string {
	t.Helper()
	for _, statement := range []string{
		`INSERT INTO conversation_input_actions(action_key,turn_id,kind,expected_owner,expected_revision,target_owner,created_at) VALUES('register','base','register','',0,'owner',CURRENT_TIMESTAMP)`,
		`INSERT INTO conversation_input_actions(action_key,turn_id,kind,expected_owner,expected_revision,hold_key,hold_owner,hold_version,reason,created_at) VALUES('hold','base','hold','owner',1,'delivery','owner',0,'unknown',CURRENT_TIMESTAMP)`,
		`INSERT INTO conversation_plan_heads VALUES('conversation','branch','activity','base',1,3,'{"steps":[{"title":"kept","status":"pending"}]}',CURRENT_TIMESTAMP)`,
		`INSERT INTO conversation_plan_versions SELECT * FROM conversation_plan_heads`,
		`INSERT INTO agent_accounts(id,provider,home,created_at,profile_kind,label,disabled_at) VALUES('saved','codex','isolated-profile',CURRENT_TIMESTAMP,'external-linked','kept',CURRENT_TIMESTAMP)`,
		`INSERT INTO session_agent_accounts(session_id,provider,account_id,resolution_source,bound_at) VALUES('worker','codex','saved','project',CURRENT_TIMESTAMP)`,
		`INSERT INTO review(id,session_id,project_id,harness,created_at,updated_at) VALUES('review','worker','project','codex',CURRENT_TIMESTAMP,CURRENT_TIMESTAMP)`,
		`INSERT INTO reviewer_agent_accounts(review_id,provider,account_id,resolution_source,bound_at) VALUES('review','codex','saved','project',CURRENT_TIMESTAMP)`,
		`INSERT INTO chat_delivery_attempts VALUES('delivery',1,'worker','conversation','owner','unknown',0,1,'{"evidence":"unresolved"}',CURRENT_TIMESTAMP,CURRENT_TIMESTAMP)`,
		`INSERT INTO chat_delivery_attempt_members VALUES('delivery','base',0)`,
		`INSERT INTO chat_delivery_receipts VALUES('delivery',1,'{"disposition":"unknown"}',CURRENT_TIMESTAMP)`,
		`INSERT INTO chat_side_compactions VALUES('compact','side','isolated-native','native','epoch','unknown',1,0,'{"obligation":"kept"}')`,
		`INSERT INTO chat_side_contents VALUES('side','{"text":"kept"}')`,
		`INSERT INTO usage_execution_sources(id,project_id,session_id,billing_provider,profile_id,profile_revision,profile_digest,delegation_id,attempt_id,request_id,receipt_digest,identity_json) VALUES(80,'project','worker','openai','profile','v1','digest','delegation','attempt','request','receipt','{}')`,
		`INSERT INTO model_usage_events(id,execution_source_id,provider_id,billing_provider_id,billing_provider_source,model_id,usage_measurement_kind,source_event_key,input_tokens,input_cost_nanos,cached_input_cost_nanos,output_cost_nanos,estimated_cost_nanos,pricing_version,created_at) VALUES(80,80,'openai','openai','observed','gpt-5.1','provider_reported','execution-event',300000,600000,70000,8000,678000,'price-v1',CURRENT_TIMESTAMP)`,
	} {
		mustExec(t, db, statement)
	}
	config := `{"worker":{"agentAccountId":"saved"},"agentRules":"keep"}`
	if version < removeProjectAgentAccountsVersion {
		config = `{"agentAccounts":{"codex":"retired"},"worker":{"agentAccountId":"saved"},"agentRules":"keep"}`
	}
	if _, err := db.Exec(`UPDATE projects SET config=? WHERE id='project'`, config); err != nil {
		t.Fatal(err)
	}
	result := map[string]string{}
	for _, table := range []string{"conversation_input_actions", "conversation_input_custody", "conversation_input_holds", "conversation_plan_heads", "conversation_plan_versions", "agent_accounts", "session_agent_accounts", "reviewer_agent_accounts", "chat_delivery_attempts", "chat_delivery_attempt_members", "chat_delivery_receipts", "chat_side_compactions", "chat_side_contents", "usage_execution_sources"} {
		query := "SELECT * FROM " + table + " ORDER BY 1"
		result[query] = upstreamSyncRows(t, db, query)
	}
	for _, query := range []string{
		`SELECT id,state,controller_generation,plan_json,branch_id FROM conversation_turns ORDER BY id`,
		`SELECT id,execution_source_id,provider_id,billing_provider_id,billing_provider_source,model_id,usage_measurement_kind,source_event_key,input_tokens,input_cost_nanos,cached_input_cost_nanos,output_cost_nanos,estimated_cost_nanos,pricing_version FROM model_usage_events ORDER BY id`,
	} {
		result[query] = upstreamSyncRows(t, db, query)
	}
	return result
}

func seedUpstreamRepriceNativeEvent(t *testing.T, db *sql.DB) {
	t.Helper()
	mustExec(t, db, `INSERT INTO usage_bindings(id,session_id,harness,native_root_id,state,updated_at) VALUES(7,'worker','codex','native','active',CURRENT_TIMESTAMP)`)
	mustExec(t, db, `INSERT INTO usage_sources(id,binding_id,kind,artifact_path,state,updated_at) VALUES(8,7,'codex_rollout','synthetic-usage','active',CURRENT_TIMESTAMP)`)
	mustExec(t, db, `INSERT INTO model_usage_events(id,binding_id,usage_source_id,provider_id,billing_provider_id,billing_provider_source,model_id,usage_measurement_kind,source_event_key,input_tokens,input_cost_nanos,cached_input_cost_nanos,output_cost_nanos,estimated_cost_nanos,pricing_version,created_at) VALUES(81,7,8,'openai','openai','observed','gpt-5.1','native_reported','native-event',300000,600000,70000,8000,678000,'price-v1',CURRENT_TIMESTAMP)`)
}

func TestUpstreamSyncRepriceWithoutExecutionSchema(t *testing.T) {
	db := openMigratedDatabaseCopy(t, upstreamLongContextRepriceVersion-1)
	mustExec(t, db, `INSERT INTO projects(id,path,registered_at) VALUES('project','isolated-project',CURRENT_TIMESTAMP)`)
	mustExec(t, db, `INSERT INTO sessions(id,project_id,num,activity_last_at,created_at,updated_at) VALUES('worker','project',1,CURRENT_TIMESTAMP,CURRENT_TIMESTAMP,CURRENT_TIMESTAMP)`)
	seedUpstreamRepriceNativeEvent(t, db)
	upTo(t, db, upstreamLongContextRepriceVersion)
	query := `SELECT input_cost_nanos,cached_input_cost_nanos,output_cost_nanos,estimated_cost_nanos,pricing_version FROM model_usage_events WHERE id=81`
	if got := upstreamSyncRows(t, db, query); got != `[[null,null,null,null,""]]` {
		t.Fatalf("native event was not repriced: %s", got)
	}
}

func TestUpstreamSyncRepriceIsAtomic(t *testing.T) {
	for _, tc := range []struct{ name, trigger string }{
		{"restore", `CREATE TRIGGER fail_reprice BEFORE UPDATE ON model_usage_events
 WHEN NEW.execution_source_id IS NOT NULL AND NEW.pricing_version='price-v1'
 BEGIN SELECT RAISE(ABORT,'injected reprice failure'); END`},
		{"receipt", `CREATE TRIGGER fail_reprice BEFORE INSERT ON goose_db_version
 WHEN NEW.version_id=20261031173316629
 BEGIN SELECT RAISE(ABORT,'injected reprice failure'); END`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			upstreamSyncLegacyDatabase(t, dir, 20261115050041608)
			db, err := sql.Open("sqlite", databaseURI(dir)+pragmas)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = db.Close() }()
			db.SetMaxOpenConns(1)
			seedUpstreamRepriceNativeEvent(t, db)
			// NULL and zero are also receipt facts; do not coalesce them on restore.
			mustExec(t, db, `INSERT INTO usage_execution_sources(id,project_id,session_id,billing_provider,profile_id,profile_revision,profile_digest,delegation_id,attempt_id,request_id,receipt_digest,identity_json) VALUES(82,'project','worker','openai','profile','v1','digest','delegation','partial-attempt','partial-request','partial-receipt','{}')`)
			mustExec(t, db, `INSERT INTO model_usage_events(id,execution_source_id,provider_id,billing_provider_id,model_id,usage_measurement_kind,source_event_key,input_tokens,input_cost_nanos,output_cost_nanos,pricing_version,created_at) VALUES(82,82,'openai','openai','gpt-5.1','provider_reported','partial-event',300000,0,23,'partial',CURRENT_TIMESTAMP)`)
			costs := `SELECT id,input_cost_nanos,cached_input_cost_nanos,output_cost_nanos,estimated_cost_nanos,pricing_version FROM model_usage_events`
			allBefore := upstreamSyncRows(t, db, costs+` ORDER BY id`)
			executionBefore := upstreamSyncRows(t, db, costs+` WHERE execution_source_id IS NOT NULL ORDER BY id`)
			mustExec(t, db, tc.trigger)
			if store, err := Open(dir); err == nil {
				_ = store.Close()
				t.Fatal("expected injected migration failure")
			} else if !strings.Contains(err.Error(), "injected reprice failure") {
				t.Fatal(err)
			}
			if got := upstreamSyncRows(t, db, costs+` ORDER BY id`); got != allBefore {
				t.Fatalf("failed reprice changed costs: %s -> %s", allBefore, got)
			}
			var receipts int
			if err := db.QueryRow(`SELECT count(*) FROM goose_db_version WHERE version_id=?`, upstreamLongContextRepriceVersion).Scan(&receipts); err != nil || receipts != 0 {
				t.Fatalf("failed reprice wrote receipt: %d %v", receipts, err)
			}
			mustExec(t, db, `DROP TRIGGER fail_reprice`)
			store, err := Open(dir)
			if err != nil {
				t.Fatalf("retry production Open: %v", err)
			}
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			if got := upstreamSyncRows(t, db, costs+` WHERE execution_source_id IS NOT NULL ORDER BY id`); got != executionBefore {
				t.Fatalf("retry lost execution costs: %s -> %s", executionBefore, got)
			}
			if got := upstreamSyncRows(t, db, costs+` WHERE id=81`); got != `[[81,null,null,null,null,""]]` {
				t.Fatalf("native event was not repriced: %s", got)
			}
			assertUpstreamSyncLedger(t, db)
			// An applied migration must not clear later backfill on another Open.
			mustExec(t, db, `UPDATE model_usage_events SET input_cost_nanos=12,estimated_cost_nanos=12,pricing_version='new-price' WHERE id=81`)
			after := upstreamSyncRows(t, db, costs+` ORDER BY id`)
			store, err = Open(dir)
			if err != nil {
				t.Fatal(err)
			}
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			if got := upstreamSyncRows(t, db, costs+` ORDER BY id`); got != after {
				t.Fatalf("reopen changed costs: %s -> %s", after, got)
			}
			assertUpstreamSyncLedger(t, db)
		})
	}
}

func TestUpstreamSyncRestoresRebuildAfterRestart(t *testing.T) {
	for _, tc := range []struct {
		table   string
		version int64
	}{
		{"conversation_turns", sharedInputCustodyVersion}, {"model_usage_events", forkUsageRebuildVersion},
	} {
		for _, committed := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/committed=%t", tc.table, committed), func(t *testing.T) {
				dir := t.TempDir()
				db := openMigratedDatabaseCopyAt(t, dir, tc.version-1, pragmas)
				seedSharedInputMigration(t, db)
				query := `SELECT failure_code FROM conversation_turns WHERE id='retry'`
				if tc.table == "conversation_turns" {
					mustExec(t, db, `UPDATE conversation_turns SET failure_code='retained' WHERE id='retry'`)
				} else {
					mustExec(t, db, `INSERT INTO usage_bindings(id,session_id,harness,native_root_id,state,updated_at) VALUES(7,'worker','codex','native','active',CURRENT_TIMESTAMP)`)
					mustExec(t, db, `INSERT INTO usage_sources(id,binding_id,kind,artifact_path,state,updated_at) VALUES(8,7,'codex_rollout','synthetic-usage','active',CURRENT_TIMESTAMP)`)
					mustExec(t, db, `INSERT INTO model_usage_events(id,binding_id,usage_source_id,provider_id,model_id,usage_measurement_kind,source_event_key,service_tier,provider_message_id,input_tokens) VALUES(9,7,8,'openai','model','native_reported','event','priority','message',42)`)
					query = `SELECT service_tier,provider_message_id,input_tokens FROM model_usage_events WHERE id=9`
				}
				want := upstreamSyncRows(t, db, query)
				if saved, err := saveUpstreamRebuildColumns(db, tc.table); err != nil || !saved {
					t.Fatalf("save startup values: %t %v", saved, err)
				}
				if committed {
					// Simulate process exit after Goose commits its original SQL and
					// receipt, before the runner can restore the extension values.
					mustExec(t, db, `PRAGMA foreign_keys=OFF`)
					gooseMu.Lock()
					err := goose.UpTo(db, "migrations", tc.version)
					gooseMu.Unlock()
					if err != nil {
						t.Fatal(err)
					}
				}
				if err := db.Close(); err != nil {
					t.Fatal(err)
				}
				store, err := Open(dir)
				if err != nil {
					t.Fatal(err)
				}
				if err := store.Close(); err != nil {
					t.Fatal(err)
				}
				db, err = sql.Open("sqlite", databaseURI(dir)+pragmas)
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = db.Close() }()
				if got := upstreamSyncRows(t, db, query); got != want {
					t.Fatalf("lost restart values: %s -> %s", want, got)
				}
				assertSharedInputRows(t, db)
				assertUpstreamSyncLedger(t, db)
			})
		}
	}
}

func TestUpstreamSyncUsageFailureRollsBackAndRestoresForeignKeys(t *testing.T) {
	db := openMigratedDatabaseCopy(t, forkUsageRebuildVersion-1)
	seedSharedInputMigration(t, db)
	// Force failure inside the historical SQL's native transaction.
	mustExec(t, db, `CREATE TABLE usage_execution_sources(blocker TEXT)`)
	before := sharedInputObjects(t, db)
	ledger := upstreamSyncRows(t, db, `SELECT * FROM goose_db_version ORDER BY id`)
	if err := migrate(db); err == nil {
		t.Fatal("expected native rebuild failure")
	}
	if !reflect.DeepEqual(before, sharedInputObjects(t, db)) || ledger != upstreamSyncRows(t, db, `SELECT * FROM goose_db_version ORDER BY id`) {
		t.Fatal("failed native rebuild changed schema or ledger")
	}
	assertSharedInputRows(t, db)
}

func TestUpstreamSyncDownRefusesWithoutChangingFacts(t *testing.T) {
	db := openMigratedDatabaseCopy(t, upstreamSyncSchemaVersion)
	seedSharedInputMigration(t, db)
	before := sharedInputObjects(t, db)
	ledger := upstreamSyncRows(t, db, `SELECT * FROM goose_db_version ORDER BY id`)
	gooseMu.Lock()
	err := migrateDownTo(db, upstreamSyncSchemaVersion-1)
	gooseMu.Unlock()
	if err == nil {
		t.Fatal("Vsync allowed an unsafe downgrade")
	}
	if !reflect.DeepEqual(before, sharedInputObjects(t, db)) || ledger != upstreamSyncRows(t, db, `SELECT * FROM goose_db_version ORDER BY id`) {
		t.Fatal("rejected Down changed schema or ledger")
	}
	assertSharedInputRows(t, db)
}

func TestUpstreamSyncAddsMissingCanonicalColumnsAtomically(t *testing.T) {
	dir := t.TempDir()
	db := openMigratedDatabaseCopyAt(t, dir, upstreamSyncSchemaVersion-1, pragmas)
	seedSharedInputMigration(t, db)
	mustExec(t, db, `DROP INDEX idx_conversation_turns_failed_recent; DROP INDEX idx_model_usage_events_provider_message;
ALTER TABLE conversation_turns DROP COLUMN failure_code;
ALTER TABLE model_usage_events DROP COLUMN service_tier;
ALTER TABLE model_usage_events DROP COLUMN provider_message_id;`)
	mustExec(t, db, fmt.Sprintf(`CREATE TRIGGER fail_sync_receipt BEFORE INSERT ON goose_db_version WHEN NEW.version_id=%d BEGIN SELECT RAISE(ABORT,'injected receipt failure'); END`, upstreamSyncSchemaVersion))
	before := sharedInputObjects(t, db)
	if store, err := Open(dir); err == nil {
		_ = store.Close()
		t.Fatal("expected receipt failure")
	}
	if !reflect.DeepEqual(before, sharedInputObjects(t, db)) {
		t.Fatal("failed Vsync retained partial ADD COLUMN effects")
	}
	mustExec(t, db, `DROP TRIGGER fail_sync_receipt`)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	store, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = sql.Open("sqlite", databaseURI(dir)+pragmas)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	var code string
	var tier, message sql.NullString
	if err := db.QueryRow(`SELECT failure_code FROM conversation_turns WHERE id='retry'`).Scan(&code); err != nil || code != "" {
		t.Fatalf("failure_code default: %q %v", code, err)
	}
	if err := db.QueryRow(`SELECT service_tier,provider_message_id FROM model_usage_events LIMIT 1`).Scan(&tier, &message); err != sql.ErrNoRows {
		t.Fatalf("unexpected empty usage table: %v", err)
	}
	assertUpstreamSyncLedger(t, db)
	assertUpstreamSyncConstraints(t, db)
}

func TestWithdrawnReportDeliveryReceiptStopsBeforeMigration(t *testing.T) {
	dir := t.TempDir()
	upstreamSyncLegacyDatabase(t, dir, 20261116001259486)
	db, err := sql.Open("sqlite", databaseURI(dir)+pragmas)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	mustExec(t, db, `INSERT INTO goose_db_version (version_id, is_applied) VALUES (20261120084919464, 1)`)
	before := upstreamSyncRows(t, db, `SELECT * FROM goose_db_version ORDER BY id`)
	objects := sharedInputObjects(t, db)
	store, err := Open(dir)
	if err == nil {
		_ = store.Close()
		t.Fatal("production Open accepted the withdrawn migration")
	}
	if !strings.Contains(err.Error(), "withdrawn report delivery migration") {
		t.Fatalf("Open failed at the wrong boundary: %v", err)
	}
	if after := upstreamSyncRows(t, db, `SELECT * FROM goose_db_version ORDER BY id`); before != after || !reflect.DeepEqual(objects, sharedInputObjects(t, db)) {
		t.Fatal("refused preflight wrote migration receipts or schema")
	}
}

func TestReviewerStopMigrationSeedsOnlyExactCleanupOwners(t *testing.T) {
	dir := t.TempDir()
	ctx := t.Context()
	store, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := store.UpsertProject(ctx, domain.ProjectRecord{ID: "stop-fixture", Path: dir, RegisteredAt: now}); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		id       string
		reason   domain.ReviewFailureReason
		archived bool
		handle   string
		launch   string
		bound    bool
		seeded   bool
	}{
		{"unconfirmed", domain.ReviewFailureCancelUnconfirmed, false, "pane", "launch", true, true},
		{"never-started", domain.ReviewFailureNeverStarted, false, "pane", "launch", true, true},
		{"timed-out", domain.ReviewFailureTimedOut, false, "pane", "launch", true, true},
		{"archived", domain.ReviewFailureTimedOut, true, "pane", "launch", true, false},
		{"absent-pane", domain.ReviewFailureTimedOut, false, "", "launch", true, false},
		{"obsolete", domain.ReviewFailureTimedOut, false, "pane", "old-launch", true, false},
		{"no-binding", domain.ReviewFailureTimedOut, false, "pane", "launch", false, false},
		{"process-exited", domain.ReviewFailureProcessExited, false, "pane", "launch", true, false},
	}
	for _, tc := range cases {
		rec, err := store.CreateSession(ctx, domain.SessionRecord{
			ProjectID: "stop-fixture", Kind: domain.KindWorker, Harness: domain.HarnessCodex, CreatedAt: now, UpdatedAt: now,
		})
		if err != nil {
			t.Fatal(err)
		}
		review := domain.Review{ID: tc.id, SessionID: rec.ID, ProjectID: rec.ProjectID, Harness: domain.ReviewerClaudeCode,
			ReviewerHandleID: tc.handle, ReviewerLaunchID: "launch", ReviewerActivityState: domain.ActivityExited,
			IsArchived: tc.archived, CreatedAt: now, UpdatedAt: now,
		}
		if err := store.UpsertReview(ctx, review); err != nil {
			t.Fatal(err)
		}
		if err := store.InsertReviewRun(ctx, domain.ReviewRun{
			ID: tc.id, ReviewID: tc.id, SessionID: rec.ID, Harness: review.Harness, TargetSHA: "head",
			CreatedAt: now, Status: domain.ReviewRunRunning,
		}); err != nil {
			t.Fatal(err)
		}
		moved, err := store.TransitionReviewRun(ctx, tc.id, []domain.ReviewRunStatus{domain.ReviewRunRunning},
			domain.ReviewRunFailed, domain.ReviewRunPatch{FailureReason: tc.reason, LaunchID: tc.launch})
		if err != nil || !moved {
			t.Fatalf("set up legacy failed review %s: moved=%v (%v)", tc.id, moved, err)
		}
		if tc.bound {
			if err := store.BindReviewerAgentAccount(ctx, tc.id, domain.ResolvedAgentAccount{
				AccountID: tc.id, Provider: domain.AgentAccountProviderClaude, Source: domain.AgentAccountSourceRole,
			}, now); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", databaseURI(dir)+pragmas)
	if err != nil {
		t.Fatal(err)
	}
	// The dedicated migration is additive. Recreate its immediately preceding
	// schema over these rows without touching a released migration.
	mustExec(t, db, `DROP TABLE reviewer_stop_processes;
DROP TABLE reviewer_stop_accounts;
DROP TABLE reviewer_stop_receipts;
DELETE FROM goose_db_version WHERE version_id=20261120110913324;`)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	receipts, err := store.ListReviewerStopReceipts(ctx)
	if err != nil || len(receipts) != 3 {
		t.Fatalf("seeded receipts: %+v (%v)", receipts, err)
	}
	for _, tc := range cases {
		have, err := store.HasReviewerStop(ctx, domain.ReviewerStopKey{ReviewID: tc.id, LaunchID: "launch"})
		if err != nil || have != tc.seeded {
			t.Fatalf("seeded receipt %s = %v, want %v (%v)", tc.id, have, tc.seeded, err)
		}
	}
	for _, receipt := range receipts {
		if receipt.State != domain.ReviewerStopUnknown || receipt.Reason != "historical_missing_lineage" ||
			receipt.DataRoot != "" || receipt.BootID != "" || receipt.PIDNamespace != "" {
			t.Fatalf("historical receipt guessed a scope: %+v", receipt)
		}
	}
}
