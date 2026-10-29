package sqlite

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pressly/goose/v3"
)

func TestProjectAttentionUpgradeCDCDownAndReopen(t *testing.T) {
	directory := t.TempDir()
	const base = int64(20261029075720713)
	db := openMigratedDatabaseCopyAt(t, directory, base, pragmas)
	exec := func(statement string) {
		t.Helper()
		if _, err := db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	exec(`INSERT INTO projects(id,path,registered_at) VALUES('project','isolated-project',CURRENT_TIMESTAMP)`)
	exec(`INSERT INTO sessions(id,project_id,num,activity_last_at,created_at,updated_at) VALUES('worker','project',1,CURRENT_TIMESTAMP,CURRENT_TIMESTAMP,CURRENT_TIMESTAMP)`)
	exec(`INSERT INTO attention_questions(id,project_id,origin_session_id,origin_harness,client_key,audience,kind,payload_json,create_fingerprint,created_at,updated_at) VALUES('existing','project','worker','claude-code','key','user','question','{}','hash',CURRENT_TIMESTAMP,CURRENT_TIMESTAMP)`)
	if err := migrate(db); err != nil {
		t.Fatal(err)
	}
	revision := func() int64 {
		t.Helper()
		var value int64
		if err := db.QueryRow(`SELECT COALESCE((SELECT revision FROM project_attention_revisions WHERE project_id='project'),0)`).Scan(&value); err != nil {
			t.Fatal(err)
		}
		return value
	}
	exec(`INSERT INTO conversations(id,scope,session_id,current_session_id,active_branch_id,created_at,updated_at) VALUES('conversation','session','worker','worker','root',CURRENT_TIMESTAMP,CURRENT_TIMESTAMP)`)
	exec(`INSERT INTO conversation_branches(id,conversation_id,session_id,created_at) VALUES('root','conversation','worker',CURRENT_TIMESTAMP)`)
	exec(`DELETE FROM change_log`)
	before := revision()
	exec(`INSERT INTO conversation_activities(id,conversation_id,branch_id,sequence,kind,status,summary,created_at,updated_at) VALUES('approval','conversation','root',1,'approval','pending','Decide',CURRENT_TIMESTAMP,CURRENT_TIMESTAMP)`)
	if revision() <= before {
		t.Fatal("new provider source did not dirty the notification snapshot")
	}
	before = revision()
	exec(`INSERT INTO conversation_activities(id,conversation_id,branch_id,sequence,kind,status,summary,created_at,updated_at) VALUES('command','conversation','root',2,'command','running','Work',CURRENT_TIMESTAMP,CURRENT_TIMESTAMP)`)
	if revision() != before {
		t.Fatal("ordinary streaming activity dirtied the notification snapshot")
	}
	var total, marked int
	if err := db.QueryRow(`SELECT COUNT(*),COALESCE(SUM(json_extract(payload,'$.attentionChanged')=1),0) FROM change_log`).Scan(&total, &marked); err != nil || total != 2 || marked != 1 {
		t.Fatalf("activity CDC duplicated or incorrectly marked: %d/%d %v", total, marked, err)
	}
	exec(`DELETE FROM change_log`)
	exec(`INSERT INTO attention_reads(conversation_id,activity_id,last_seen_revision,seen_at) VALUES('conversation','approval',0,CURRENT_TIMESTAMP)`)
	if err := db.QueryRow(`SELECT COUNT(*) FROM change_log WHERE json_extract(payload,'$.attentionChanged')=1`).Scan(&marked); err != nil || marked != 1 {
		t.Fatalf("seen CDC: %d %v", marked, err)
	}
	exec(`INSERT INTO reports(id,project_id,session_id,state,note,created_at,available_at) VALUES('report','project','worker','needs_input','Coordinate',CURRENT_TIMESTAMP,CURRENT_TIMESTAMP)`)
	if err := db.QueryRow(`SELECT COUNT(*) FROM change_log WHERE json_extract(payload,'$.reportId')='report'`).Scan(&marked); err != nil || marked != 1 {
		t.Fatalf("report CDC: %d %v", marked, err)
	}
	exec(`INSERT INTO reports(id,project_id,session_id,state,note,created_at,available_at) VALUES('completed','project','worker','checkpoint','Progress',CURRENT_TIMESTAMP,CURRENT_TIMESTAMP)`)
	if err := db.QueryRow(`SELECT COUNT(*) FROM change_log WHERE json_extract(payload,'$.reportId')='completed'`).Scan(&marked); err != nil || marked != 1 {
		t.Fatalf("a newer checkpoint did not invalidate the superseded request: %d %v", marked, err)
	}
	exec(`DELETE FROM change_log`)
	exec(`UPDATE reports SET delivery_state='acknowledged',claim_token='test',claimed_at=CURRENT_TIMESTAMP,acknowledged_at=CURRENT_TIMESTAMP WHERE id='completed'`)
	if err := db.QueryRow(`SELECT COUNT(*) FROM change_log WHERE json_extract(payload,'$.reportId')='completed'`).Scan(&marked); err != nil || marked != 0 {
		t.Fatalf("non-attention delivery update invalidated project attention: %d %v", marked, err)
	}
	before = revision()
	exec(`DELETE FROM change_log`)
	if revision() != before {
		t.Fatal("CDC retention changed or discarded the durable source revision")
	}
	exec(`UPDATE attention_questions SET seen_version=version WHERE id='existing'`)
	if revision() <= before {
		t.Fatal("question read did not dirty the notification snapshot")
	}
	before = revision()
	exec(`DELETE FROM conversation_activities WHERE id='approval'`)
	if revision() <= before {
		t.Fatal("provider deletion did not dirty the notification snapshot")
	}
	exec(`INSERT INTO attention_report_questions(report_id,question_id,forwarded_by,created_at) VALUES('report','existing','historical-orchestrator',CURRENT_TIMESTAMP)`)
	if err := goose.DownTo(db, "migrations", base); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name LIKE 'project_attention_%' AND type IN ('table','trigger')`).Scan(&total); err != nil || total != 0 {
		t.Fatalf("down left notification revision schema behind: %d %v", total, err)
	}
	var trigger string
	if err := db.QueryRow(`SELECT sql FROM sqlite_master WHERE type='trigger' AND name='conversation_activities_cdc_insert'`).Scan(&trigger); err != nil || strings.Contains(trigger, "attentionChanged") {
		t.Fatalf("down failed to restore original trigger: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := sql.Open("sqlite", "file:"+filepath.Join(directory, "ao.db")+pragmas)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close() }()
	if err := migrate(reopened); err != nil {
		t.Fatal(err)
	}
	if err := reopened.QueryRow(`SELECT COUNT(*) FROM attention_questions WHERE id='existing'`).Scan(&total); err != nil || total != 1 {
		t.Fatalf("upgrade lost existing question: %d %v", total, err)
	}
}
