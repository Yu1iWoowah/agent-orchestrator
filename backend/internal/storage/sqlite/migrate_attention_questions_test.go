package sqlite

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

func TestAttentionQuestionsUpgradeReopenAndCDC(t *testing.T) {
	directory := t.TempDir()
	db := openMigratedDatabaseCopyAt(t, directory, 20261028204037417, pragmas)
	for _, statement := range []string{
		`INSERT INTO projects(id,path,registered_at) VALUES('question-project','/tmp/question-project',CURRENT_TIMESTAMP)`,
		`INSERT INTO sessions(id,project_id,num,activity_last_at,created_at,updated_at) VALUES('question-session','question-project',1,CURRENT_TIMESTAMP,CURRENT_TIMESTAMP,CURRENT_TIMESTAMP)`,
	} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	if err := migrate(db); err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`INSERT INTO attention_questions(id,project_id,origin_session_id,origin_harness,client_key,audience,kind,payload_json,create_fingerprint,created_at,updated_at) VALUES('question','question-project','question-session','claude-code','key','user','question','{}','hash',CURRENT_TIMESTAMP,CURRENT_TIMESTAMP)`,
		`INSERT INTO attention_answers(question_id,client_answer_id,question_version,answers_json,fingerprint,message,saved_at) VALUES('question','answer',1,'[]','hash','Human answer',CURRENT_TIMESTAMP)`,
		`UPDATE attention_questions SET state='answered' WHERE id='question'`,
		`UPDATE attention_answers SET delivery_state='delivered',delivered_at=CURRENT_TIMESTAMP WHERE question_id='question'`,
	} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	var events int
	if err := db.QueryRow(`SELECT COUNT(*) FROM change_log WHERE json_extract(payload,'$.questionId')='question'`).Scan(&events); err != nil || events != 3 {
		t.Fatalf("question CDC events=%d %v", events, err)
	}
	if _, err := db.Exec(`UPDATE attention_answers SET delivery_attempts=delivery_attempts+1 WHERE question_id='question'`); err != nil {
		t.Fatal(err)
	}
	var after int
	if err := db.QueryRow(`SELECT COUNT(*) FROM change_log WHERE json_extract(payload,'$.questionId')='question'`).Scan(&after); err != nil || after != events {
		t.Fatalf("attempt-only writes emitted CDC=%d %v", after, err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", "file:"+filepath.Join(directory, "ao.db")+pragmas)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if err = migrate(db); err != nil {
		t.Fatal(err)
	}
	var receipt string
	if err = db.QueryRowContext(context.Background(), `SELECT delivery_state FROM attention_answers WHERE question_id='question'`).Scan(&receipt); err != nil || receipt != "delivered" {
		t.Fatalf("reopen lost receipt=%q %v", receipt, err)
	}
}
