package sqlite

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
)

const removeProjectAgentAccountsVersion int64 = 20261115050041608

// prepareRemoveProjectAgentAccounts stops startup before the cleanup migration
// when a project config cannot be cleaned as stored. The error names every such
// project and the shape problem, never the config content. Nothing is written,
// so the previous AO version still starts against the same database.
func prepareRemoveProjectAgentAccounts(db *sql.DB) error {
	var tables int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name IN ('goose_db_version', 'projects')`,
	).Scan(&tables); err != nil {
		return err
	}
	if tables != 2 {
		return nil
	}
	// projects.config arrives in 0008; an older schema has no agentAccounts.
	var configColumn int
	if err := db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('projects') WHERE name = 'config'`).Scan(&configColumn); err != nil {
		return err
	}
	if configColumn == 0 {
		return nil
	}
	var applied int
	if err := db.QueryRow(`
SELECT COALESCE((
    SELECT is_applied FROM goose_db_version
    WHERE version_id = ? ORDER BY id DESC LIMIT 1
), 0)`, removeProjectAgentAccountsVersion).Scan(&applied); err != nil {
		return err
	}
	if applied != 0 {
		return nil
	}
	rows, err := db.Query(`SELECT id, CAST(config AS TEXT) FROM projects WHERE config IS NOT NULL AND config <> '' ORDER BY id`)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	var problems []string
	for rows.Next() {
		var id, config string
		if err := rows.Scan(&id, &config); err != nil {
			return err
		}
		if problem := projectAgentAccountsShapeProblem([]byte(config)); problem != "" {
			problems = append(problems, fmt.Sprintf("project %q: %s", id, problem))
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(problems) > 0 {
		return fmt.Errorf("cannot remove project agentAccounts: %s; fix these project configs, or reinstall the previous AO version, before upgrading", strings.Join(problems, "; "))
	}
	return nil
}

// projectAgentAccountsShapeProblem reports why a stored config is not a JSON
// object whose optional agentAccounts value is an object.
func projectAgentAccountsShapeProblem(config []byte) string {
	if !json.Valid(config) {
		return "config is not valid JSON"
	}
	var top map[string]json.RawMessage
	if !bytes.HasPrefix(bytes.TrimSpace(config), []byte("{")) || json.Unmarshal(config, &top) != nil {
		return "config is not a JSON object"
	}
	if value, ok := top["agentAccounts"]; ok && !bytes.HasPrefix(bytes.TrimSpace(value), []byte("{")) {
		return "agentAccounts is not a JSON object"
	}
	return ""
}
