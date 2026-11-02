package store_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/gen"
)

func TestInputCustodyReplacementRowIDs(t *testing.T) {
	for _, emptyKeys := range []bool{false, true} {
		t.Run(fmt.Sprintf("fixture-isolation/empty=%t", emptyKeys), func(t *testing.T) {
			_, first := inputReplacementFixture(t, "registered", emptyKeys)
			secondStore, second := inputReplacementFixture(t, "ordinary", emptyKeys)
			before := inputPayloadRow(t, second, "conversation_messages", "message-ordinary")
			if _, err := first.Exec(`UPDATE conversation_messages SET text='first fixture only' WHERE id='message-ordinary'`); err != nil {
				t.Fatal(err)
			}
			var custody int
			if err := second.QueryRow(`SELECT count(*) FROM conversation_input_custody`).Scan(&custody); err != nil || custody != 0 {
				t.Fatalf("fixture inherited custody: count=%d err=%v", custody, err)
			}
			if got := inputPayloadRow(t, second, "conversation_messages", "message-ordinary"); got != before {
				t.Fatal("fixture shares writable payload with another case")
			}
			_, third := inputReplacementFixture(t, "ordinary", emptyKeys)
			if got := inputPayloadRow(t, third, "conversation_messages", "message-ordinary"); got != before {
				t.Fatal("fixture mutation changed the seed template")
			}
			if err := secondStore.InputCustodyStorageCompatibility(t.Context()); err != nil {
				t.Fatal(err)
			}
		})
	}
	for _, mode := range []string{"held", "registered", "ordinary"} {
		for _, c := range []inputReplacementCase{inputReplacementCases[0], inputReplacementCases[3]} {
			for _, negative := range []bool{false, true} {
				for _, verb := range []string{"INSERT", "UPDATE"} {
					for _, alias := range []string{"rowid", "_rowid_", "oid"} {
						t.Run(fmt.Sprintf("%s/%s/negative=%t/%s/%s", mode, c.table, negative, verb, alias), func(t *testing.T) {
							s, db := inputReplacementFixture(t, "ordinary", false)
							if negative {
								if _, err := db.Exec("UPDATE "+c.table+" SET rowid=-1 WHERE id=?", c.original); err != nil {
									t.Fatal(err)
								}
							}
							if mode != "ordinary" && negative {
								// Historical S0 registration bypasses the newer API sentinel rejection.
								if err := gen.New(db).InsertInputCustodyAction(t.Context(), gen.InsertInputCustodyActionParams{ActionKey: "register", TurnID: "protected", Kind: "register", TargetOwner: "owner", CreatedAt: testNow}); err != nil {
									t.Fatal(err)
								}
							} else if mode != "ordinary" {
								if _, err := s.RegisterInputCustody(t.Context(), "protected", "owner", "register", testNow); err != nil {
									t.Fatal(err)
								}
							}
							if mode == "held" {
								if _, err := s.HoldConversationInput(t.Context(), domain.InputCustodyVersion{TurnID: "protected", Owner: "owner", Revision: 1}, domain.InputHoldVersion{Key: "delivery", Owner: "owner"}, "outcome_unknown", "hold", testNow); err != nil {
									t.Fatal(err)
								}
							}
							var rowID int64
							if err := db.QueryRow("SELECT rowid FROM "+c.table+" WHERE id=?", c.original).Scan(&rowID); err != nil {
								t.Fatal(err)
							}
							before := inputPayloadRow(t, db, c.table, c.original)
							replacement := c
							replacement.column = alias
							replacement.value = rowID
							query, args := inputReplacementSQL(replacement, verb)
							_, err := db.Exec(query, args...)
							var count int
							if scanErr := db.QueryRow("SELECT COUNT(*) FROM "+c.table+" WHERE id=?", c.original).Scan(&count); scanErr != nil {
								t.Fatal(scanErr)
							}
							if mode == "ordinary" {
								if err != nil || count != 0 {
									t.Fatalf("ordinary rowid replacement changed: originalRows=%d err=%v", count, err)
								}
								incoming := c.ordinary
								if verb == "INSERT" {
									incoming = "incoming"
								}
								var actual int64
								if err := db.QueryRow("SELECT rowid FROM "+c.table+" WHERE id=?", incoming).Scan(&actual); err != nil || actual != rowID {
									t.Fatalf("ordinary replacement rowid=%d want=%d err=%v", actual, rowID, err)
								}
							} else {
								if err == nil || !strings.Contains(err.Error(), "INPUT_HELD") || count != 1 {
									t.Fatalf("protected rowid replacement accepted: originalRows=%d err=%v", count, err)
								}
								if after := inputPayloadRow(t, db, c.table, c.original); after != before {
									t.Fatal("rowid replacement changed original payload")
								}
								var actual int64
								if err := db.QueryRow("SELECT rowid FROM "+c.table+" WHERE id=?", c.original).Scan(&actual); err != nil || actual != rowID {
									t.Fatalf("protected rowid=%d want=%d err=%v", actual, rowID, err)
								}
							}
							if err := s.InputCustodyStorageCompatibility(t.Context()); err != nil {
								t.Fatal(err)
							}
						})
					}
				}
			}
		}
	}
}

func TestInputCustodyOrdinaryRowIDWrites(t *testing.T) {
	for _, mode := range []string{"ordinary", "registered", "held"} {
		for _, c := range []inputReplacementCase{inputReplacementCases[0], inputReplacementCases[3]} {
			t.Run(mode+"/"+c.table, func(t *testing.T) {
				s, db := inputReplacementFixture(t, mode, false)
				if mode == "ordinary" {
					if _, err := db.Exec("UPDATE "+c.table+" SET rowid=-1 WHERE id=?", c.original); err != nil {
						t.Fatal(err)
					}
				}
				before := inputPayloadRow(t, db, c.table, c.original)
				normal := c
				normal.value = ""
				query, args := inputReplacementSQL(normal, "INSERT")
				if _, err := db.Exec(query, args...); err != nil {
					t.Fatalf("ordinary auto-rowid insert: %v", err)
				}
				for i, alias := range []string{"rowid", "_rowid_", "oid"} {
					want := int64(100 + i)
					if _, err := db.Exec("UPDATE OR REPLACE "+c.table+" SET "+alias+"=? WHERE id='incoming'", want); err != nil {
						t.Fatal(err)
					}
					var actual int64
					if err := db.QueryRow("SELECT rowid FROM " + c.table + " WHERE id='incoming'").Scan(&actual); err != nil || actual != want {
						t.Fatalf("ordinary rowid=%d want=%d err=%v", actual, want, err)
					}
				}
				if after := inputPayloadRow(t, db, c.table, c.original); after != before {
					t.Fatal("ordinary rowid writes changed protected payload")
				}
				if err := s.InputCustodyStorageCompatibility(t.Context()); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}

// SQLite leaves NEW.rowid undefined before an INSERT that does not supply it.
// A registered rowid=-1 therefore fences ambiguous inserts conservatively.
func TestInputCustodyRowIDDefaultSentinelFailsClosed(t *testing.T) {
	for _, c := range []inputReplacementCase{inputReplacementCases[0], inputReplacementCases[3]} {
		t.Run(c.table, func(t *testing.T) {
			s, db := inputReplacementFixture(t, "ordinary", false)
			if _, err := db.Exec("UPDATE "+c.table+" SET rowid=-1 WHERE id=?", c.original); err != nil {
				t.Fatal(err)
			}
			// Reproduce already-registered S0 data: the new API rejects new sentinel registration.
			if err := gen.New(db).InsertInputCustodyAction(t.Context(), gen.InsertInputCustodyActionParams{ActionKey: "register", TurnID: "protected", Kind: "register", TargetOwner: "owner", CreatedAt: testNow}); err != nil {
				t.Fatal(err)
			}
			before := inputPayloadRow(t, db, c.table, c.original)
			normal := c
			normal.value = ""
			query, args := inputReplacementSQL(normal, "INSERT")
			if _, err := db.Exec(query, args...); err == nil || !strings.Contains(err.Error(), "INPUT_HELD") {
				t.Fatalf("ambiguous rowid collision was not fenced: %v", err)
			}
			if after := inputPayloadRow(t, db, c.table, c.original); after != before {
				t.Fatal("ambiguous insert changed protected payload")
			}
			if err := s.InputCustodyStorageCompatibility(t.Context()); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestInputCustodyLedgersCannotReplaceByRowID(t *testing.T) {
	for _, table := range []string{"conversation_input_actions", "conversation_input_custody", "conversation_input_holds"} {
		for _, alias := range []string{"rowid", "_rowid_", "oid"} {
			t.Run(table+"/"+alias, func(t *testing.T) {
				s, db := inputReplacementFixture(t, "registered", false)
				predicate := "turn_id='protected'"
				if table == "conversation_input_actions" {
					predicate = "action_key='register'"
					if _, err := s.TransferInputCustody(t.Context(), domain.InputCustodyVersion{TurnID: "protected", Owner: "owner", Revision: 1}, "next-owner", "transfer", testNow); err != nil {
						t.Fatal(err)
					}
				} else {
					if _, err := s.RegisterInputCustody(t.Context(), "ordinary", "other-owner", "register-ordinary", testNow); err != nil {
						t.Fatal(err)
					}
					if table == "conversation_input_holds" {
						for _, item := range []struct{ turn, owner string }{{"protected", "owner"}, {"ordinary", "other-owner"}} {
							if _, err := s.HoldConversationInput(t.Context(), domain.InputCustodyVersion{TurnID: item.turn, Owner: item.owner, Revision: 1}, domain.InputHoldVersion{Key: "delivery", Owner: item.owner}, "unknown", "hold-"+item.turn, testNow); err != nil {
								t.Fatal(err)
							}
						}
					}
				}
				rows, err := db.Query(`SELECT name FROM pragma_table_info(?) ORDER BY cid`, table)
				if err != nil {
					t.Fatal(err)
				}
				defer rows.Close()
				var columns []string
				for rows.Next() {
					var name string
					if err := rows.Scan(&name); err != nil {
						t.Fatal(err)
					}
					columns = append(columns, `"`+strings.ReplaceAll(name, `"`, `""`)+`"`)
				}
				if err := rows.Err(); err != nil {
					t.Fatal(err)
				}
				if err := rows.Close(); err != nil {
					t.Fatal(err)
				}
				fields := strings.Join(columns, ",")
				var before, after string
				if err := db.QueryRow("SELECT json_array(" + fields + ") FROM " + table + " WHERE " + predicate).Scan(&before); err != nil {
					t.Fatal(err)
				}
				query := "INSERT OR REPLACE INTO " + table + "(" + alias + "," + fields + ") SELECT (SELECT " + alias + " FROM " + table + " WHERE " + predicate + ")," + fields + " FROM " + table + " WHERE turn_id='ordinary'"
				if table == "conversation_input_actions" {
					query = "INSERT OR REPLACE INTO " + table + "(" + alias + ",action_key,turn_id,kind,expected_owner,expected_revision,target_owner,created_at) SELECT " + alias + ",'new-register','ordinary','register','',0,'another-owner',CURRENT_TIMESTAMP FROM " + table + " WHERE " + predicate
				}
				if _, err := db.Exec(query); err == nil {
					t.Fatal("ledger accepts hidden rowid replacement")
				}
				if err := db.QueryRow("SELECT json_array(" + fields + ") FROM " + table + " WHERE " + predicate).Scan(&after); err != nil || after != before {
					t.Fatalf("rowid replacement changed ledger: err=%v", err)
				}
				var rowIDless int
				if err := db.QueryRow(`SELECT wr FROM pragma_table_list WHERE schema='main' AND name=?`, table).Scan(&rowIDless); err != nil || rowIDless != 1 {
					t.Fatalf("ledger still exposes physical rowid: wr=%d err=%v", rowIDless, err)
				}
				if err := s.InputCustodyStorageCompatibility(t.Context()); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}

func TestInputCustodyReplacementFromNullIDUsesPhysicalIdentity(t *testing.T) {
	for _, mode := range []string{"registered", "held"} {
		for _, c := range inputReplacementCases {
			t.Run(mode+"/"+c.name, func(t *testing.T) {
				s, db := inputReplacementFixture(t, mode, false)
				before := inputPayloadRow(t, db, c.table, c.original)
				var sourceRowID int64
				if err := db.QueryRow("SELECT rowid FROM "+c.table+" WHERE id=?", c.ordinary).Scan(&sourceRowID); err != nil {
					t.Fatal(err)
				}
				if _, err := db.Exec("UPDATE "+c.table+" SET id=NULL WHERE rowid=?", sourceRowID); err != nil {
					t.Fatal(err)
				}
				_, err := db.Exec("UPDATE OR REPLACE "+c.table+" SET "+c.column+"=? WHERE rowid=?", c.value, sourceRowID)
				var count int
				if e := db.QueryRow("SELECT COUNT(*) FROM "+c.table+" WHERE id=?", c.original).Scan(&count); e != nil {
					t.Fatal(e)
				}
				if err == nil || count != 1 {
					t.Fatalf("nullable source id bypassed replacement guard: originalRows=%d err=%v", count, err)
				}
				if after := inputPayloadRow(t, db, c.table, c.original); after != before {
					t.Fatal("replacement changed protected payload")
				}
				if err := s.InputCustodyStorageCompatibility(t.Context()); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}
