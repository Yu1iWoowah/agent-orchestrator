package sqlite

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"unicode"

	"github.com/pressly/goose/v3"
)

const sharedInputCustodyVersion int64 = 20261101152248341

// migrateUpTo runs under gooseMu, before publishing either connection pool to
// services. The daemon's data-dir/lifecycle lock excludes an older controller.
// Only the turn rebuild disables FK enforcement, before Goose opens its native
// transaction; its SQL rejects new FK violations in the rebuilt dependency set.
const (
	forkUsageRebuildVersion      int64 = 20261109083558284
	upstreamDeleteIndexesVersion int64 = 20261107181854835
	forkLineageIndexesVersion    int64 = 20261113162757663
	upstreamSyncSchemaVersion    int64 = 20261116001259486
)

// Only boundaries present in the active FS participate. Old-version fixtures
// and partial migration sets use the same runner without adopting future SQL.
func migrateUpTo(db *sql.DB, version int64, opts ...goose.OptionsFunc) error {
	migrations, err := goose.CollectMigrations("migrations", 0, version)
	if err != nil {
		return err
	}
	for _, migration := range migrations {
		boundary := migration.Version
		switch boundary {
		case sharedInputCustodyVersion, forkUsageRebuildVersion, upstreamDeleteIndexesVersion, forkLineageIndexesVersion, upstreamSyncSchemaVersion:
		default:
			continue
		}
		if err := goose.UpTo(db, "migrations", boundary-1, opts...); err != nil {
			return err
		}
		if boundary == sharedInputCustodyVersion || boundary == forkUsageRebuildVersion {
			var applied, saved int
			table := "conversation_turns"
			if boundary == forkUsageRebuildVersion {
				table = "model_usage_events"
			}
			if err := db.QueryRow(`SELECT COALESCE((SELECT is_applied FROM goose_db_version WHERE version_id=? ORDER BY id DESC LIMIT 1),0)`, boundary).Scan(&applied); err != nil {
				return err
			}
			if err := db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?`, "_upstream_sync_"+table).Scan(&saved); err != nil {
				return err
			}
			if applied != 0 && saved == 0 {
				continue
			}
		}
		switch boundary {
		case forkUsageRebuildVersion:
			if err := withUpstreamRebuildColumns(db, "model_usage_events", func() error {
				return goose.UpTo(db, "migrations", boundary, opts...)
			}); err != nil {
				return err
			}
		case sharedInputCustodyVersion:
			if err := withInputRebuildForeignKeys(db, func() error {
				return goose.UpTo(db, "migrations", boundary, opts...)
			}); err != nil {
				return err
			}
		case upstreamDeleteIndexesVersion, forkLineageIndexesVersion:
			if err := prepareUpstreamTurnIndexes(db, boundary); err != nil {
				return err
			}
			if err := goose.UpTo(db, "migrations", boundary, opts...); err != nil {
				return err
			}
		case upstreamSyncSchemaVersion:
			if err := applyUpstreamSyncSchema(db); err != nil {
				return err
			}
		}
	}
	return goose.UpTo(db, "migrations", version, opts...)
}

func upstreamMigrationApplied(tx *sql.Tx, version int64) (bool, error) {
	var applied int
	err := tx.QueryRow(`SELECT COALESCE((SELECT is_applied FROM goose_db_version
 WHERE version_id=? ORDER BY id DESC LIMIT 1),0)`, version).Scan(&applied)
	return applied != 0, err
}

// Startup owns the single writer. Committing removal before Goose is restartable:
// no receipt exists until the original migration creates all of its objects.
func prepareUpstreamTurnIndexes(db *sql.DB, version int64) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	applied, err := upstreamMigrationApplied(tx, version)
	if err != nil || applied {
		return err
	}
	for _, table := range []string{"conversation_messages", "conversation_activities"} {
		name := "idx_" + table + "_turn"
		if err := validateUpstreamIndex(tx, name,
			"CREATE INDEX "+name+" ON "+table+"(conversation_id,turn_id,sequence)",
			"CREATE INDEX "+name+" ON "+table+"(turn_id) WHERE turn_id IS NOT NULL",
		); err != nil {
			return err
		}
	}
	for _, table := range []string{"conversation_messages", "conversation_activities"} {
		if _, err := tx.Exec("DROP INDEX IF EXISTS idx_" + table + "_turn"); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func normalizedUpstreamSQL(statement string) string {
	var result strings.Builder
	quoted := false
	for _, character := range statement {
		if character == '\'' {
			quoted = !quoted
		}
		if quoted || character == '\'' {
			result.WriteRune(character)
		} else if !unicode.IsSpace(character) {
			result.WriteRune(unicode.ToLower(character))
		}
	}
	return result.String()
}

func validateUpstreamIndex(tx *sql.Tx, name string, known ...string) error {
	var definition string
	err := tx.QueryRow(`SELECT sql FROM sqlite_master WHERE type='index' AND name=?`, name).Scan(&definition)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, expected := range known {
		if normalizedUpstreamSQL(definition) == normalizedUpstreamSQL(expected) {
			return nil
		}
	}
	return fmt.Errorf("upstream sync: unknown definition for index %s", name)
}

// The SQL migration is the code-generation source of truth. On an upgraded
// fork, apply all of its effects and its receipt atomically while retaining
// columns that upstream migrations have already supplied.
func applyUpstreamSyncSchema(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	applied, err := upstreamMigrationApplied(tx, upstreamSyncSchemaVersion)
	if err != nil || applied {
		return err
	}
	source, err := migrationsFS.ReadFile("migrations/20261116001259486_upstream_sync_schema.sql")
	if err != nil {
		return err
	}
	statement, _, ok := strings.Cut(string(source), "-- +goose Down")
	if !ok {
		return fmt.Errorf("upstream sync: missing SQL boundary")
	}
	for _, column := range upstreamSyncColumns {
		present, err := upstreamColumnPresent(tx, column)
		if err != nil {
			return err
		}
		if !present {
			continue
		}
		add := "ALTER TABLE " + column.table + " ADD COLUMN " + column.name + " " + column.definition + ";"
		if strings.Count(statement, add) != 1 {
			return fmt.Errorf("upstream sync: missing SQL for %s.%s", column.table, column.name)
		}
		statement = strings.Replace(statement, add, "", 1)
	}

	indexes := []struct{ name, definition string }{
		{"idx_conversation_turns_failed_recent", "CREATE INDEX idx_conversation_turns_failed_recent ON conversation_turns(completed_at) WHERE state = 'failed'"},
		{"idx_model_usage_events_provider_message", "CREATE UNIQUE INDEX idx_model_usage_events_provider_message ON model_usage_events(provider_id,provider_message_id) WHERE provider_message_id IS NOT NULL"},
		{"idx_model_usage_events_priced_version", "CREATE INDEX idx_model_usage_events_priced_version ON model_usage_events(pricing_version,id) WHERE estimated_cost_nanos IS NOT NULL"},
		{"idx_model_usage_events_model", "CREATE INDEX idx_model_usage_events_model ON model_usage_events(model_id,id)"},
	}
	for _, index := range indexes {
		if err := validateUpstreamIndex(tx, index.name, index.definition); err != nil {
			return err
		}
	}
	for _, table := range []string{"conversation_messages", "conversation_activities"} {
		name := "idx_" + table + "_turn"
		if err := validateUpstreamIndex(tx, name, "CREATE INDEX "+name+" ON "+table+"(conversation_id,turn_id,sequence)",
			"CREATE INDEX "+name+" ON "+table+"(turn_id) WHERE turn_id IS NOT NULL"); err != nil {
			return err
		}
		name += "_delete"
		if err := validateUpstreamIndex(tx, name, "CREATE INDEX "+name+" ON "+table+"(turn_id) WHERE turn_id IS NOT NULL"); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(statement); err != nil {
		return fmt.Errorf("upstream sync schema: %w", err)
	}
	if _, err := tx.Exec(`INSERT INTO goose_db_version(version_id,is_applied) VALUES(?,1)`, upstreamSyncSchemaVersion); err != nil {
		return err
	}
	return tx.Commit()
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
	return withUpstreamRebuildColumns(db, "conversation_turns", run)
}

type upstreamSyncColumn struct {
	table, name, definition string
	notNull                 int
	defaultValue            sql.NullString
}

var upstreamSyncColumns = []upstreamSyncColumn{
	{"conversation_turns", "failure_code", "TEXT NOT NULL DEFAULT ''", 1, sql.NullString{String: "''", Valid: true}},
	{"model_usage_events", "service_tier", "TEXT\n    CHECK (service_tier IS NULL OR (trim(service_tier) <> '' AND length(service_tier) <= 64))", 0, sql.NullString{}},
	{"model_usage_events", "provider_message_id", "TEXT\n    CHECK (provider_message_id IS NULL OR (trim(provider_message_id) <> '' AND length(provider_message_id) <= 256))", 0, sql.NullString{}},
}

var upstreamRebuildIndexes = map[string]string{
	"idx_conversation_turns_failed_recent":    "CREATE INDEX idx_conversation_turns_failed_recent ON conversation_turns(completed_at) WHERE state = 'failed'",
	"idx_model_usage_events_provider_message": "CREATE UNIQUE INDEX idx_model_usage_events_provider_message ON model_usage_events(provider_id,provider_message_id) WHERE provider_message_id IS NOT NULL",
	"idx_model_usage_events_priced_version":   "CREATE INDEX idx_model_usage_events_priced_version ON model_usage_events(pricing_version,id) WHERE estimated_cost_nanos IS NOT NULL",
	"idx_model_usage_events_model":            "CREATE INDEX idx_model_usage_events_model ON model_usage_events(model_id,id)",
}

func upstreamColumnPresent(tx *sql.Tx, column upstreamSyncColumn) (bool, error) {
	var typ string
	var notNull, pk, hidden int
	var defaultValue sql.NullString
	err := tx.QueryRow(`SELECT type,"notnull",dflt_value,pk,hidden FROM pragma_table_xinfo(?) WHERE name=?`, column.table, column.name).Scan(&typ, &notNull, &defaultValue, &pk, &hidden)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var tableSQL string
	if err := tx.QueryRow(`SELECT sql FROM sqlite_master WHERE type='table' AND name=?`, column.table).Scan(&tableSQL); err != nil {
		return false, err
	}
	definition := normalizedUpstreamSQL(column.name + " " + column.definition)
	schema := normalizedUpstreamSQL(tableSQL)
	if !strings.EqualFold(typ, "TEXT") || notNull != column.notNull || defaultValue != column.defaultValue || pk != 0 || hidden != 0 ||
		!strings.Contains(schema, definition+",") && !strings.Contains(schema, definition+")") {
		return false, fmt.Errorf("upstream sync: unknown definition for %s.%s", column.table, column.name)
	}
	return true, nil
}

// The historical rebuilds cannot copy columns they predate. Keep their extension
// values in durable startup-only tables before running the original SQL. If the
// process exits after the rebuild commits, its next startup restores this copy;
// it never overwrites it with the rebuilt table's empty defaults.
func withUpstreamRebuildColumns(db *sql.DB, table string, run func() error) (result error) {
	db.SetMaxOpenConns(1)
	var foreignKeys int
	if err := db.QueryRow(`PRAGMA foreign_keys`).Scan(&foreignKeys); err != nil {
		return err
	}
	defer func() {
		pragma := "PRAGMA foreign_keys=OFF"
		if foreignKeys != 0 {
			pragma = "PRAGMA foreign_keys=ON"
		}
		if _, err := db.Exec(pragma); err != nil {
			result = errors.Join(result, fmt.Errorf("restore upstream-rebuild foreign keys: %w", err))
		}
	}()
	saved, err := saveUpstreamRebuildColumns(db, table)
	if err != nil {
		return err
	}
	if err := run(); err != nil {
		// Goose's transactional rebuild rolls back itself. The usage rebuild owns
		// BEGIN/COMMIT in its SQL; release a failed native transaction as well.
		_, _ = db.Exec("ROLLBACK")
		if saved {
			// Retain the durable copy if the original SQL committed before its receipt.
			// A caller must not mistake a partial historical migration for completion.
			if cleanupErr := discardUnchangedUpstreamCopy(db, table); cleanupErr != nil {
				return errors.Join(err, cleanupErr)
			}
		}
		return err
	}
	if !saved {
		return nil
	}
	return restoreUpstreamRebuildColumns(db, table)
}

func saveUpstreamRebuildColumns(db *sql.DB, table string) (bool, error) {
	tx, err := db.Begin()
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	backup := "_upstream_sync_" + table
	var exists int
	if err := tx.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?`, backup).Scan(&exists); err != nil {
		return false, err
	}
	if exists != 0 {
		return true, nil
	}
	columns := []string{}
	for _, column := range upstreamSyncColumns {
		if column.table != table {
			continue
		}
		present, err := upstreamColumnPresent(tx, column)
		if err != nil {
			return false, err
		}
		if present {
			columns = append(columns, column.name)
		}
	}
	if len(columns) == 0 {
		return false, nil
	}
	for name, definition := range upstreamRebuildIndexes {
		if strings.HasPrefix(name, "idx_"+table+"_") {
			if err := validateUpstreamIndex(tx, name, definition); err != nil {
				return false, err
			}
		}
	}
	if _, err := tx.Exec("CREATE TABLE " + backup + " AS SELECT id," + strings.Join(columns, ",") + " FROM " + table); err != nil {
		return false, err
	}
	if _, err := tx.Exec("CREATE TABLE " + backup + "_indexes(name TEXT PRIMARY KEY, definition TEXT NOT NULL)"); err != nil {
		return false, err
	}
	for name := range upstreamRebuildIndexes {
		if strings.HasPrefix(name, "idx_"+table+"_") {
			// #nosec G202 -- backup derives only from the two literal rebuild table names; index name is bound.
			if _, err := tx.Exec("INSERT INTO "+backup+"_indexes SELECT name,sql FROM sqlite_master WHERE type='index' AND name=?", name); err != nil {
				return false, err
			}
		}
	}
	return true, tx.Commit()
}

func upstreamSavedColumns(tx *sql.Tx, table string) ([]upstreamSyncColumn, error) {
	backup := "_upstream_sync_" + table
	rows, err := tx.Query(`SELECT name FROM pragma_table_xinfo(?) WHERE name<>'id' ORDER BY cid`, backup)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var columns []upstreamSyncColumn
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		found := false
		for _, column := range upstreamSyncColumns {
			if column.table == table && column.name == name {
				columns = append(columns, column)
				found = true
				break
			}
		}
		if !found {
			return nil, fmt.Errorf("upstream sync: unknown saved column for %s", table)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(columns) == 0 {
		return nil, fmt.Errorf("upstream sync: empty saved column set for %s", table)
	}
	return columns, nil
}

func restoreUpstreamRebuildColumns(db *sql.DB, table string) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	backup := "_upstream_sync_" + table
	columns, err := upstreamSavedColumns(tx, table)
	if err != nil {
		return err
	}
	var lost int
	if err := tx.QueryRow("SELECT count(*) FROM " + backup + " b LEFT JOIN " + table + " t ON t.id=b.id WHERE t.id IS NULL").Scan(&lost); err != nil {
		return err
	}
	if lost != 0 {
		return fmt.Errorf("upstream sync: %s rebuild lost %d rows", table, lost)
	}
	for _, column := range columns {
		present, err := upstreamColumnPresent(tx, column)
		if err != nil {
			return err
		}
		if !present {
			if _, err := tx.Exec("ALTER TABLE " + table + " ADD COLUMN " + column.name + " " + column.definition); err != nil {
				return err
			}
		}
		value := "(SELECT b." + column.name + " FROM " + backup + " b WHERE b.id=" + table + ".id)"
		// #nosec G202 -- table is a literal rebuild target; column.name is selected from upstreamSyncColumns.
		if _, err := tx.Exec("UPDATE " + table + " SET " + column.name + "=" + value + " WHERE id IN(SELECT id FROM " + backup + ") AND " + column.name + " IS NOT " + value); err != nil {
			return err
		}
	}
	// #nosec G202 -- backup derives only from the two literal rebuild table names.
	rows, err := tx.Query("SELECT name,definition FROM " + backup + "_indexes ORDER BY name")
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	saved := map[string]string{}
	for rows.Next() {
		var name, definition string
		if err := rows.Scan(&name, &definition); err != nil {
			_ = rows.Close()
			return err
		}
		expected, known := upstreamRebuildIndexes[name]
		if !known || !strings.HasPrefix(name, "idx_"+table+"_") || normalizedUpstreamSQL(expected) != normalizedUpstreamSQL(definition) {
			_ = rows.Close()
			return fmt.Errorf("upstream sync: unknown saved index for %s", table)
		}
		saved[name] = definition
	}
	rowErr := rows.Err()
	if err := rows.Close(); err != nil {
		return err
	}
	if rowErr != nil {
		return rowErr
	}
	for name, definition := range saved {
		if err := validateUpstreamIndex(tx, name, definition); err != nil {
			return err
		}
		var present int
		if err := tx.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='index' AND name=?`, name).Scan(&present); err != nil {
			return err
		}
		if present == 0 {
			if _, err := tx.Exec(definition); err != nil {
				return err
			}
		}
	}
	if _, err := tx.Exec("DROP TABLE " + backup + "_indexes; DROP TABLE " + backup); err != nil {
		return err
	}
	return tx.Commit()
}

func discardUnchangedUpstreamCopy(db *sql.DB, table string) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	columns, err := upstreamSavedColumns(tx, table)
	if err != nil {
		return err
	}
	backup := "_upstream_sync_" + table
	for _, column := range columns {
		present, err := upstreamColumnPresent(tx, column)
		if err != nil {
			return err
		}
		if !present {
			return nil
		}
		var changed int
		if err := tx.QueryRow("SELECT count(*) FROM " + backup + " b LEFT JOIN " + table + " t ON t.id=b.id WHERE t.id IS NULL OR t." + column.name + " IS NOT b." + column.name).Scan(&changed); err != nil {
			return err
		}
		if changed != 0 {
			return nil
		}
	}
	if _, err := tx.Exec("DROP TABLE " + backup + "_indexes; DROP TABLE " + backup); err != nil {
		return err
	}
	return tx.Commit()
}
