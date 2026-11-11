package delegate

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func replyIdentityFixture(attempt string) replyIdentity {
	p := budgetProfile("openai")
	return replyIdentity{ProjectID: "p", SessionID: "s", DelegationID: "d", AttemptID: attempt, BudgetAttemptKey: strings.Repeat("k", 64), Provider: p.BillingProvider, Model: p.Model, ProfileID: p.ID, ProfileRevision: p.Revision, ProfileDigest: p.digest()}
}

func newReplyFixture(t *testing.T) *replyStore {
	t.Helper()
	return newReplyStore(filepath.Join(t.TempDir(), "hcg", "delegation", "replies"))
}

// readReply is the operator lookup: owner root + public usage_ref.attempt_id.
func readReply(t *testing.T, s *replyStore, attempt string) (replyRecord, []byte, bool) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(s.dir, attempt, "record.json"))
	if err != nil {
		t.Fatal(err)
	}
	var record replyRecord
	if err := json.Unmarshal(raw, &record); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(filepath.Join(s.dir, attempt, "body.bin"))
	if errors.Is(err, os.ErrNotExist) {
		return record, nil, false
	}
	if err != nil {
		t.Fatal(err)
	}
	return record, body, true
}

func TestReplyRecordKeepsEntityBodyVerbatimAndPrivate(t *testing.T) {
	s := newReplyFixture(t)
	// Synthetic secrets only: the canary, an escaped canary and a fixture marker.
	body := []byte("not json\r\n\xff\x00 HCG_CANARY_DO_NOT_SEND " + jsonEscape("H") + "CG_CANARY SYNTHETIC_TRANSPORT_MARKER \n")
	a, err := s.admit(t.Context(), replyIdentityFixture("attempt-a"))
	if err != nil {
		t.Fatal(err)
	}
	record, _, _ := readReply(t, s, "attempt-a")
	if record.State != "in_flight" || record.ReservedBytes != s.worst() {
		t.Fatalf("intent not persisted before send: %+v", record)
	}
	if err := a.capture(WireResponse{Status: 200, Headers: map[string]string{"x-request-id": "req-1", "request-id": ""}}, WireCapture{Outcome: "response", Body: body, Complete: true, ObservedAtLeast: int64(len(body)), Stage: "credential_scan", Code: "credential_echo"}); err != nil {
		t.Fatal(err)
	}
	receipt := Receipt{Execution: Execution{AttemptID: "attempt-a"}, RequestID: "req-1", ResponseID: "resp-1", UnknownReason: "usage_missing"}
	if err := a.receipt(receipt, ErrBudgetBlocked); err != nil {
		t.Fatal(err)
	}
	if err := a.finish(replyValidation{Result: "not_run", Failure: &replyFailure{Stage: "transport", Code: "transport_failed"}}); err != nil {
		t.Fatal(err)
	}
	record, stored, ok := readReply(t, s, "attempt-a")
	digest, _ := json.Marshal(receipt)
	c := record.Capture
	if !ok || !bytes.Equal(stored, body) || record.State != "sent_validation_not_run" || record.ReservedBytes != 0 || c == nil || !c.Complete || c.BodySHA256 != textHash(string(body)) || c.PrefixSHA256 != "" || c.BodyBytes != int64(len(body)) || c.RequestID != "req-1" || c.ParentRefusal == nil || c.ParentRefusal.Code != "credential_echo" {
		t.Fatalf("verbatim body/metadata lost: %+v %+v", record, c)
	}
	if record.Receipt == nil || record.Receipt.Canonical != "blocked" || record.Receipt.ReceiptSHA256 != textHash(string(digest)) || record.Receipt.ResponseID != "resp-1" || record.Validation.Failure.Code != "transport_failed" || record.Identity != replyIdentityFixture("attempt-a") {
		t.Fatalf("attempt/receipt association lost: %+v", record)
	}
	for path, mode := range map[string]os.FileMode{s.dir: 0700, filepath.Join(s.dir, "attempt-a"): 0700, filepath.Join(s.dir, "attempt-a", "body.bin"): 0600, filepath.Join(s.dir, "attempt-a", "record.json"): 0600} {
		info, err := os.Lstat(path)
		if err != nil || info.Mode().Perm() != mode {
			t.Fatalf("%s mode=%v err=%v", path, info.Mode(), err)
		}
	}
	entries, _ := os.ReadDir(filepath.Join(s.dir, "attempt-a"))
	if len(entries) != 2 {
		t.Fatalf("atomic temp left behind: %v", entries)
	}
}

func TestReplyRecordPartialAndMissingResponsesAreNotComplete(t *testing.T) {
	s := newReplyFixture(t)
	s.bodyLimit = 8
	for name, tc := range map[string]struct {
		capture        WireCapture
		stored         string
		hasBody        bool
		observed       int64
		prefix, digest string
	}{
		"over_limit":  {WireCapture{Outcome: "response", Body: []byte("0123456789"), Complete: true, ObservedAtLeast: 10}, "01234567", true, 10, textHash("01234567"), ""},
		"read_failed": {WireCapture{Outcome: "read_failed", Body: []byte("abc"), ObservedAtLeast: 3, Stage: "body_read", Code: "read_failed"}, "abc", true, 3, textHash("abc"), ""},
		"no_response": {WireCapture{Outcome: "no_response", Stage: "network", Code: "request_failed"}, "", false, 0, "", ""},
	} {
		t.Run(name, func(t *testing.T) {
			a, err := s.admit(t.Context(), replyIdentityFixture(name))
			if err != nil {
				t.Fatal(err)
			}
			if err := a.capture(WireResponse{Status: 502}, tc.capture); err != nil {
				t.Fatal(err)
			}
			record, stored, ok := readReply(t, s, name)
			c := record.Capture
			if ok != tc.hasBody || string(stored) != tc.stored || c.Complete || c.ObservedAtLeast != tc.observed || c.PrefixSHA256 != tc.prefix || c.BodySHA256 != tc.digest || c.Outcome != tc.capture.Outcome {
				t.Fatalf("partial/no response claimed complete: %+v body=%q", c, stored)
			}
			if tc.capture.Outcome == "no_response" && c.HTTPStatus != 0 {
				t.Fatalf("no_response invented a status: %+v", c)
			}
		})
	}
}

func TestReplyRecordRejectsUnsafeOrConflictingLocations(t *testing.T) {
	t.Run("symlink_root", func(t *testing.T) {
		s := newReplyFixture(t)
		target := t.TempDir()
		if err := os.MkdirAll(filepath.Dir(s.dir), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, s.dir); err != nil {
			t.Fatal(err)
		}
		if _, err := s.admit(t.Context(), replyIdentityFixture("a")); !errors.Is(err, ErrReplyRecord) {
			t.Fatalf("symlinked store accepted: %v", err)
		}
	})
	t.Run("shared_mode", func(t *testing.T) {
		s := newReplyFixture(t)
		if err := os.MkdirAll(s.dir, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(s.dir, 0755); err != nil {
			t.Fatal(err)
		}
		if _, err := s.admit(t.Context(), replyIdentityFixture("a")); !errors.Is(err, ErrReplyRecord) {
			t.Fatalf("shared store accepted: %v", err)
		}
	})
	t.Run("existing_attempt_and_file_mode", func(t *testing.T) {
		s := newReplyFixture(t)
		a, err := s.admit(t.Context(), replyIdentityFixture("a"))
		if err != nil {
			t.Fatal(err)
		}
		if err := a.notSent("authorization_withdrawn"); err != nil {
			t.Fatal(err)
		}
		before, _, _ := readReply(t, s, "a")
		if _, err := s.admit(t.Context(), replyIdentityFixture("a")); !errors.Is(err, ErrReplyRecord) {
			t.Fatalf("existing attempt overwritten: %v", err)
		}
		after, _, _ := readReply(t, s, "a")
		if before.UpdatedAt != after.UpdatedAt || after.State != "not_sent" {
			t.Fatal("conflicting admission changed the existing record")
		}
		if err := os.Chmod(filepath.Join(s.dir, "a", "record.json"), 0644); err != nil {
			t.Fatal(err)
		}
		if _, err := s.admit(t.Context(), replyIdentityFixture("b")); !errors.Is(err, ErrReplyRecord) {
			t.Fatalf("shared record mode accepted: %v", err)
		}
	})
}

func TestReplyRecordWriteFailuresFreezeOnlyLaterAdmissions(t *testing.T) {
	s := newReplyFixture(t)
	writer := s.write
	s.write = func(string, string, []byte) error { return errors.New("synthetic disk full") }
	if _, err := s.admit(t.Context(), replyIdentityFixture("intent")); !errors.Is(err, ErrReplyRecord) {
		t.Fatalf("intent write failure ignored: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(s.dir, "intent")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("unsent failed intent left a record directory")
	}
	s.write = writer
	b, errB := s.admit(t.Context(), replyIdentityFixture("b"))
	c, errC := s.admit(t.Context(), replyIdentityFixture("c"))
	if errB != nil || errC != nil {
		t.Fatalf("unsent intent failure or a peer in flight froze admission: %v %v", errB, errC)
	}
	s.write = func(dir, name string, data []byte) error {
		if name == "body.bin" && filepath.Base(dir) == "b" {
			return errors.New("synthetic sync failure")
		}
		return writer(dir, name, data)
	}
	if err := b.capture(WireResponse{Status: 200}, WireCapture{Outcome: "response", Body: []byte("x"), Complete: true, ObservedAtLeast: 1}); !errors.Is(err, ErrReplyRecord) {
		t.Fatalf("body write failure not raised: %v", err)
	}
	if _, err := s.admit(t.Context(), replyIdentityFixture("d")); !errors.Is(err, ErrReplyRecord) {
		t.Fatal("record failure did not freeze later admission")
	}
	// The other already-sent attempt still completes its bounded cleanup.
	if err := c.capture(WireResponse{Status: 200}, WireCapture{Outcome: "response", Body: []byte("y"), Complete: true, ObservedAtLeast: 1}); err != nil {
		t.Fatal(err)
	}
	if err := c.receipt(Receipt{}, nil); err != nil {
		t.Fatal(err)
	}
	if err := c.finish(replyValidation{Result: "accepted"}); err != nil {
		t.Fatal(err)
	}
	if err := b.finish(replyValidation{Result: "accepted"}); !errors.Is(err, ErrReplyRecord) {
		t.Fatal("failed attempt could still finalize as normal")
	}
	failed, _, _ := readReply(t, s, "b")
	done, _, _ := readReply(t, s, "c")
	if failed.State != "record_failed" || failed.RecordFailure != "body_write_failed" || done.State != "sent_validation_done" {
		t.Fatalf("failure/cleanup states: %s/%s %s", failed.State, failed.RecordFailure, done.State)
	}
	if _, err := newReplyStore(s.dir).admit(t.Context(), replyIdentityFixture("e")); !errors.Is(err, ErrReplyRecord) {
		t.Fatal("restart cleared a persisted record failure")
	}
	if err := writeReplyFile(filepath.Join(s.dir, "c"), ".", []byte("z")); err == nil {
		t.Fatal("rename failure not returned")
	}
	if entries, _ := os.ReadDir(filepath.Join(s.dir, "c")); len(entries) != 2 {
		t.Fatalf("failed atomic write left a temp: %v", entries)
	}
}

func TestReplyRecordCapacityIsAtomicAndCountsTemps(t *testing.T) {
	s := newReplyFixture(t)
	s.bodyLimit, s.metadataLimit = 1024, 4096
	s.ceiling = s.worst() + s.metadataLimit
	var wg sync.WaitGroup
	results := make([]*replyAttempt, 2)
	errs := make([]error, 2)
	for i := range 2 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = s.admit(context.Background(), replyIdentityFixture([]string{"a", "b"}[i]))
		}(i)
	}
	wg.Wait()
	if (errs[0] == nil) == (errs[1] == nil) {
		t.Fatalf("both or neither contender got the last reservation: %v %v", errs[0], errs[1])
	}
	winner := results[0]
	if winner == nil {
		winner = results[1]
	}
	// Provably unsent: unused reservation returns; small metadata stays charged.
	if err := winner.notSent("transport_not_sent"); err != nil {
		t.Fatal(err)
	}
	next, err := s.admit(t.Context(), replyIdentityFixture("c"))
	if err != nil {
		t.Fatalf("not_sent did not release unused capacity: %v", err)
	}
	if err := next.capture(WireResponse{Status: 200}, WireCapture{Outcome: "response", Body: []byte("{}"), Complete: true, ObservedAtLeast: 2}); err != nil {
		t.Fatal(err)
	}
	if err := next.receipt(Receipt{}, nil); err != nil {
		t.Fatal(err)
	}
	if err := next.finish(replyValidation{Result: "accepted"}); err != nil {
		t.Fatal(err)
	}
	temp := filepath.Join(s.dir, "c", ".tmp-left")
	if err := os.WriteFile(temp, bytes.Repeat([]byte("t"), int(s.metadataLimit)), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.admit(t.Context(), replyIdentityFixture("d")); !errors.Is(err, ErrReplyRecord) {
		t.Fatal("leftover atomic temp was not counted")
	}
	if err := os.Remove(temp); err != nil {
		t.Fatal(err)
	}
	if _, err := s.admit(t.Context(), replyIdentityFixture("d")); err != nil {
		t.Fatalf("capacity not available after temp removal: %v", err)
	}
}

func TestReplyRecordRestartWithUnfinishedIntentStops(t *testing.T) {
	s := newReplyFixture(t)
	a, err := s.admit(t.Context(), replyIdentityFixture("a"))
	if err != nil {
		t.Fatal(err)
	}
	if err := a.capture(WireResponse{Status: 200}, WireCapture{Outcome: "response", Body: []byte("kept"), Complete: true, ObservedAtLeast: 4}); err != nil {
		t.Fatal(err)
	}
	restarted := newReplyStore(s.dir)
	if _, err := restarted.admit(t.Context(), replyIdentityFixture("b")); !errors.Is(err, ErrReplyRecord) {
		t.Fatal("restart guessed an unfinished intent was free")
	}
	record, body, _ := readReply(t, s, "a")
	if record.State != "in_flight" || string(body) != "kept" || record.ReservedBytes != s.worst() {
		t.Fatalf("restart changed unfinished data/reservation: %+v", record)
	}
}

func bridgeFixture(t *testing.T, actual Transport, authorize func(context.Context) bool) (*budgetBridge, *replyStore, *receiptSink, WireRequest) {
	t.Helper()
	b := newBudget(t, 0, 0)
	p := budgetProfile("openai")
	store := newReplyFixture(t)
	sink := &receiptSink{}
	bridge := &budgetBridge{budget: b, replies: store, execution: budgetExecution("p", "attempt-x"), profile: p, actual: actual, canonical: sink, authorize: authorize}
	payload, _ := json.Marshal(map[string]any{"model": p.Model, "max_completion_tokens": p.OutputTokens, "messages": []map[string]string{{"role": "system", "content": "fixed"}, {"role": "user", "content": "synthetic"}}})
	return bridge, store, sink, WireRequest{Provider: p.BillingProvider, Endpoint: p.endpoint(), Model: p.Model, Payload: payload, ScopeDigest: textHash("synthetic-scope")}
}

func TestReplyRecordBridgeTerminalStates(t *testing.T) {
	t.Run("authorization_withdrawn", func(t *testing.T) {
		calls := 0
		bridge, store, _, request := bridgeFixture(t, transportFunc(func(context.Context, WireRequest) (WireResponse, error) { calls++; return WireResponse{}, nil }), func(context.Context) bool { return false })
		if _, err := bridge.Do(t.Context(), request); err == nil || calls != 0 {
			t.Fatalf("withdrawn authorization sent: %v calls=%d", err, calls)
		}
		record, _, hasBody := readReply(t, store, "attempt-x")
		if record.State != "not_sent" || record.NotSentReason != "authorization_withdrawn" || record.ReservedBytes != 0 || hasBody || readBudget(t, bridge.budget).Attempts[bridge.key].State != "released" || bridge.finishReply(replyValidation{Result: "not_run"}) != nil {
			t.Fatalf("unsent attempt not terminal: %+v", record)
		}
	})
	t.Run("transport_not_sent", func(t *testing.T) {
		bridge, store, sink, request := bridgeFixture(t, transportFunc(func(context.Context, WireRequest) (WireResponse, error) { return WireResponse{}, ErrNotSent }), func(context.Context) bool { return true })
		if _, err := bridge.Do(t.Context(), request); !errors.Is(err, ErrNotSent) {
			t.Fatal(err)
		}
		if err := bridge.Record(t.Context(), Receipt{}); err != nil || len(sink.receipts) != 0 {
			t.Fatal("not-sent attempt invented a receipt")
		}
		record, _, _ := readReply(t, store, "attempt-x")
		if record.State != "not_sent" || record.NotSentReason != "transport_not_sent" {
			t.Fatalf("trusted not-sent lost: %+v", record)
		}
	})
	t.Run("sent_caller_left_first", func(t *testing.T) {
		response := providerFixtureReply("fixture-model", "answer")
		bridge, store, sink, request := bridgeFixture(t, transportFunc(func(context.Context, WireRequest) (WireResponse, error) { return response, nil }), func(context.Context) bool { return true })
		got, err := bridge.Do(t.Context(), request)
		if err != nil || got.Capture != nil {
			t.Fatalf("capture forwarded toward projection: %+v %v", got, err)
		}
		if err := bridge.finishReply(replyValidation{Result: "not_run", Failure: &replyFailure{Stage: "cancel", Code: "cancelled"}}); err != nil {
			t.Fatal(err)
		}
		if record, _, _ := readReply(t, store, "attempt-x"); record.State != "in_flight" {
			t.Fatalf("terminal before bounded send-side cleanup: %s", record.State)
		}
		receipt := budgetReceipt(bridge.execution, bridge.profile)
		if err := bridge.Record(t.Context(), receipt); err != nil {
			t.Fatal(err)
		}
		record, body, _ := readReply(t, store, "attempt-x")
		if record.State != "sent_validation_not_run" || record.Validation.Failure.Code != "cancelled" || record.Receipt.Canonical != "recorded" || record.Identity.BudgetAttemptKey != bridge.key || !bytes.Equal(body, response.Body) || len(sink.receipts) != 1 {
			t.Fatalf("abandoned sent attempt not finished: %+v", record)
		}
	})
	t.Run("capture_failure_keeps_accounting", func(t *testing.T) {
		bridge, store, sink, request := bridgeFixture(t, transportFunc(func(context.Context, WireRequest) (WireResponse, error) {
			return providerFixtureReply("fixture-model", "answer"), nil
		}), func(context.Context) bool { return true })
		writer := store.write
		store.write = func(dir, name string, data []byte) error {
			if name == "body.bin" {
				return errors.New("synthetic disk full")
			}
			return writer(dir, name, data)
		}
		if _, err := bridge.Do(t.Context(), request); !errors.Is(err, ErrReplyRecord) {
			t.Fatalf("capture failure hidden: %v", err)
		}
		// The answer stays withheld, but canonical accounting still completes.
		if err := bridge.Record(t.Context(), budgetReceipt(bridge.execution, bridge.profile)); !errors.Is(err, ErrReplyRecord) {
			t.Fatalf("record failure not raised after accounting: %v", err)
		}
		if len(sink.receipts) != 1 || readBudget(t, bridge.budget).Attempts[bridge.key].State != "settled" {
			t.Fatal("sent attempt accounting was not completed")
		}
		if _, err := store.admit(t.Context(), replyIdentityFixture("later")); !errors.Is(err, ErrReplyRecord) {
			t.Fatal("record failure did not freeze the owner")
		}
	})
}
