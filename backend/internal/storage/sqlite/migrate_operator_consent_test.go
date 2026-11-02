package sqlite

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	sqlitestore "github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/store"
)

func TestOperatorConsentMigrationUpDownUp(t *testing.T) {
	const previous int64 = 20261101144300192
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

func TestOperatorConsentPrepareOnFullSchemaEmitsSafeCDC(t *testing.T) {
	db := openMigratedDatabaseCopy(t, 20261101165111979)
	store := sqlitestore.NewStore(db, db)
	ctx := context.Background()
	const projectID = "c0-project"
	if err := store.UpsertProject(ctx, domain.ProjectRecord{ID: projectID, Path: "/projects/c0", RegisteredAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	plan := domain.OperatorActionPlan{
		Protocol: domain.OperatorConsentProtocol, Purpose: domain.ConsentHandoffContext, Domain: "handoff",
		ActionKey: "full-schema-action", SubjectRef: "operation-1", ParticipantRefs: []string{"source-1"},
		Scope:             domain.OperatorActionScope{ProjectRef: projectID, OwnerRef: "owner-1", BranchRef: "branch-1", NativeRef: "native-1"},
		ExpectedRevisions: map[string]int64{"owner": 1}, PayloadDigest: strings.Repeat("a", 64), EffectDigest: strings.Repeat("b", 64),
		ValidUntil: time.Now().UTC().Add(10 * time.Minute), RiskCodes: []string{"REQUIRED_CONTEXT"},
	}
	receipt, err := store.PrepareOperatorAction(ctx, plan)
	if err != nil {
		t.Fatal(err)
	}
	var project, payload string
	var session sql.NullString
	if err = db.QueryRow(`SELECT project_id,session_id,payload FROM change_log WHERE event_type='session_updated' AND json_extract(payload,'$.operatorRequestRef')=?`, string(receipt.RequestRef)).Scan(&project, &session, &payload); err != nil {
		t.Fatal(err)
	}
	expected := `{"operatorRequestRef":"` + string(receipt.RequestRef) + `"}`
	if project != projectID || session.Valid || payload != expected {
		t.Fatalf("CDC project=%q session=%+v payload=%s", project, session, payload)
	}
	var count int
	if err = db.QueryRow(`SELECT count(*) FROM change_log WHERE json_extract(payload,'$.operatorRequestRef')=?`, string(receipt.RequestRef)).Scan(&count); err != nil || count != 1 {
		t.Fatalf("CDC count=%d: %v", count, err)
	}
}
