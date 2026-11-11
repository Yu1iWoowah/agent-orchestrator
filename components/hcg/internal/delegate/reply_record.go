package delegate

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// ErrReplyRecord is the typed private-recording failure. Callers project it as
// integration_unavailable; details live only in the attempt's private record.
var ErrReplyRecord = errors.New("delegation reply record unavailable")

const (
	replyRecordSchema  = "hcg.reply-record/1"
	replyBodyLimit     = 1 << 20
	replyMetadataLimit = 64 << 10
	replyOwnerLimit    = 64 << 20
)

// replyStore keeps every controlled provider attempt's bounded entity body and
// private diagnosis under the canonical owner's hcg/delegation/replies. It is
// a logical byte quota for this diagnosis data only, not a money ledger or a
// guarantee against a full filesystem. Nothing here deletes old records.
type replyStore struct {
	dir                               string
	bodyLimit, metadataLimit, ceiling int64
	now                               func() time.Time
	write                             func(dir, name string, data []byte) error
	mu                                sync.Mutex
	live                              map[string]bool
	frozen                            bool
}

func newReplyStore(dir string) *replyStore {
	return &replyStore{dir: dir, bodyLimit: replyBodyLimit, metadataLimit: replyMetadataLimit, ceiling: replyOwnerLimit, now: func() time.Time { return time.Now().UTC() }, write: writeReplyFile, live: map[string]bool{}}
}

// replyIdentity is limited to trusted owner facts; native/launch/generation
// and provider self-reports are deliberately absent.
type replyIdentity struct {
	RunID            string `json:"run_id,omitempty"`
	ProjectID        string `json:"project_id"`
	SessionID        string `json:"session_id"`
	DelegationID     string `json:"delegation_id"`
	AttemptID        string `json:"attempt_id"`
	BudgetAttemptKey string `json:"budget_attempt_key"`
	Provider         string `json:"provider"`
	Model            string `json:"model"`
	ProfileID        string `json:"profile_id"`
	ProfileRevision  string `json:"profile_revision"`
	ProfileDigest    string `json:"profile_digest"`
}
type replyCapture struct {
	Outcome            string        `json:"outcome"`
	HTTPStatus         int           `json:"http_status,omitempty"`
	RequestID          string        `json:"request_id,omitempty"`
	AlternateRequestID string        `json:"alternate_request_id,omitempty"`
	Complete           bool          `json:"complete"`
	BodyBytes          int64         `json:"body_bytes"`
	ObservedAtLeast    int64         `json:"observed_at_least"`
	BodySHA256         string        `json:"body_sha256,omitempty"`
	PrefixSHA256       string        `json:"prefix_sha256,omitempty"`
	ParentRefusal      *replyFailure `json:"parent_refusal,omitempty"`
}
type replyReceipt struct {
	ReceiptSHA256 string `json:"receipt_sha256"`
	RequestID     string `json:"request_id,omitempty"`
	ResponseID    string `json:"response_id,omitempty"`
	UnknownReason string `json:"unknown_reason,omitempty"`
	FinishReason  string `json:"finish_reason,omitempty"`
	Canonical     string `json:"canonical"`
}
type replyValidation struct {
	Result  string        `json:"result"`
	Failure *replyFailure `json:"failure,omitempty"`
}
type replyRecord struct {
	Schema        string           `json:"schema"`
	State         string           `json:"state"`
	ReservedBytes int64            `json:"reserved_bytes"`
	CreatedAt     time.Time        `json:"created_at"`
	UpdatedAt     time.Time        `json:"updated_at"`
	Identity      replyIdentity    `json:"identity"`
	NotSentReason string           `json:"not_sent_reason,omitempty"`
	Capture       *replyCapture    `json:"capture,omitempty"`
	Receipt       *replyReceipt    `json:"receipt,omitempty"`
	Validation    *replyValidation `json:"validation,omitempty"`
	RecordFailure string           `json:"record_failure,omitempty"`
}

// replyAttempt is the in-process owner of one in_flight intent. The dispatch
// goroutine and the caller may finish in either order; the later one commits.
type replyAttempt struct {
	store        *replyStore
	dir          string
	mu           sync.Mutex
	record       replyRecord
	dispatched   bool
	pending      *replyValidation
	failed, done bool
}

func (s *replyStore) worst() int64 { return 2*s.bodyLimit + 2*s.metadataLimit }

// admit atomically reserves the worst case for this attempt and persists the
// in_flight intent before any final authorization or send.
func (s *replyStore) admit(ctx context.Context, id replyIdentity) (*replyAttempt, error) {
	if !profileToken.MatchString(id.AttemptID) || id.ProjectID == "" || id.SessionID == "" || id.BudgetAttemptKey == "" {
		return nil, ErrReplyRecord
	}
	s.mu.Lock()
	frozen := s.frozen
	s.mu.Unlock()
	if frozen {
		return nil, ErrReplyRecord
	}
	if err := ensureReplyDir(filepath.Dir(s.dir), true); err != nil {
		return nil, err
	}
	if err := ensureReplyDir(s.dir, false); err != nil {
		return nil, err
	}
	lock, err := os.OpenFile(filepath.Join(s.dir, ".lock"), budgetLockFlags, 0600)
	if err != nil {
		return nil, ErrReplyRecord
	}
	defer lock.Close()
	for {
		locked, err := tryBudgetLock(lock)
		if err != nil {
			return nil, ErrReplyRecord
		}
		if locked {
			break
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
	used, err := s.usage()
	if err != nil || used > s.ceiling-s.worst() {
		return nil, ErrReplyRecord
	}
	dir := filepath.Join(s.dir, id.AttemptID)
	if err := os.Mkdir(dir, 0700); err != nil {
		return nil, ErrReplyRecord // an existing attempt is never overwritten
	}
	now := s.now()
	a := &replyAttempt{store: s, dir: dir, record: replyRecord{Schema: replyRecordSchema, State: "in_flight", ReservedBytes: s.worst(), CreatedAt: now, Identity: id}}
	if err := os.Chmod(dir, 0700); err != nil {
		_ = os.Remove(dir)
		return nil, ErrReplyRecord
	}
	if err := a.persist(); err != nil {
		if os.Remove(dir) != nil {
			s.latch()
		}
		return nil, ErrReplyRecord
	}
	s.mu.Lock()
	s.live[id.AttemptID] = true
	s.mu.Unlock()
	return a, nil
}

// usage counts stored files plus atomic temps of terminal records and the
// reservation of each live in_flight intent. An intent without a live owner
// (restart), a failed record or anything unexpected stops admission.
func (s *replyStore) usage() (int64, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return 0, ErrReplyRecord
	}
	s.mu.Lock()
	live := map[string]bool{}
	for k := range s.live {
		live[k] = true
	}
	s.mu.Unlock()
	total := int64(0)
	for _, entry := range entries {
		path := filepath.Join(s.dir, entry.Name())
		info, err := os.Lstat(path)
		if err != nil {
			return 0, ErrReplyRecord
		}
		if !info.IsDir() {
			if !info.Mode().IsRegular() {
				return 0, ErrReplyRecord
			}
			total += info.Size()
			continue
		}
		record, files, err := s.read(path)
		if err != nil {
			return 0, err
		}
		switch {
		case record.State == "in_flight" && live[entry.Name()]:
			total += max(record.ReservedBytes, files)
		case record.State == "not_sent" || record.State == "sent_validation_done" || record.State == "sent_validation_not_run":
			total += files
		default: // record_failed, or an unfinished intent without a live owner
			return 0, ErrReplyRecord
		}
	}
	return total, nil
}

func (s *replyStore) read(dir string) (replyRecord, int64, error) {
	var record replyRecord
	if err := checkReplyDir(dir); err != nil {
		return record, 0, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return record, 0, ErrReplyRecord
	}
	files := int64(0)
	for _, entry := range entries {
		info, err := os.Lstat(filepath.Join(dir, entry.Name()))
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || !ownedByProcess(info) {
			return record, 0, ErrReplyRecord
		}
		files += info.Size()
	}
	f, err := os.Open(filepath.Join(dir, "record.json"))
	if err != nil {
		return record, 0, ErrReplyRecord
	}
	defer f.Close()
	decoder := json.NewDecoder(io.LimitReader(f, s.metadataLimit))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&record) != nil || decoder.Decode(new(any)) != io.EOF || record.Schema != replyRecordSchema || record.Identity.AttemptID != filepath.Base(dir) {
		return record, 0, ErrReplyRecord
	}
	return record, files, nil
}

func (s *replyStore) latch() {
	s.mu.Lock()
	s.frozen = true
	s.mu.Unlock()
}

func (a *replyAttempt) persist() error {
	a.record.UpdatedAt = a.store.now()
	data, err := json.Marshal(a.record)
	if err != nil || int64(len(data)) > a.store.metadataLimit {
		return ErrReplyRecord
	}
	if a.store.write(a.dir, "record.json", data) != nil {
		return ErrReplyRecord
	}
	return nil
}

// fail freezes this owner's later admissions; already-sent accounting and
// cleanup elsewhere continue. A failure marker is best effort (disk may be full).
func (a *replyAttempt) fail(code string) error {
	a.store.latch()
	if !a.failed {
		a.failed = true
		a.record.State, a.record.ReservedBytes, a.record.RecordFailure = "record_failed", a.store.worst(), code
		_ = a.persist()
	}
	a.release()
	return ErrReplyRecord
}

func (a *replyAttempt) release() {
	a.done = true
	a.store.mu.Lock()
	delete(a.store.live, a.record.Identity.AttemptID)
	a.store.mu.Unlock()
}

// notSent is the terminal for provably unsent intents; only small metadata
// stays charged against the quota.
func (a *replyAttempt) notSent(reason string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.failed || a.done {
		return ErrReplyRecord
	}
	a.record.State, a.record.ReservedBytes, a.record.NotSentReason = "not_sent", 0, reason
	if a.persist() != nil {
		return a.fail("not_sent_write_failed")
	}
	a.release()
	return nil
}

// capture stores the received entity bytes verbatim, including bodies the
// parent refused to forward, before any projection or PAL step.
func (a *replyAttempt) capture(response WireResponse, c WireCapture) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.failed || a.done || a.record.Capture != nil {
		return ErrReplyRecord
	}
	body := c.Body
	out := &replyCapture{Outcome: c.Outcome, HTTPStatus: response.Status, RequestID: response.Headers["x-request-id"], AlternateRequestID: response.Headers["request-id"], Complete: c.Complete, ObservedAtLeast: max(c.ObservedAtLeast, int64(len(body)))}
	if c.Outcome == "no_response" {
		out.HTTPStatus = 0
	}
	if int64(len(body)) > a.store.bodyLimit {
		body, out.Complete = body[:a.store.bodyLimit], false
	}
	if c.Stage != "" {
		out.ParentRefusal = &replyFailure{Stage: c.Stage, Code: c.Code}
	}
	out.BodyBytes = int64(len(body))
	if out.Complete {
		out.BodySHA256 = textHash(string(body))
	} else if len(body) > 0 {
		out.PrefixSHA256 = textHash(string(body))
	}
	if len(body) > 0 || c.Outcome != "no_response" {
		if _, err := os.Lstat(filepath.Join(a.dir, "body.bin")); !errors.Is(err, os.ErrNotExist) {
			return a.fail("body_conflict")
		}
		if a.store.write(a.dir, "body.bin", body) != nil {
			return a.fail("body_write_failed")
		}
	}
	a.record.Capture = out
	if a.persist() != nil {
		return a.fail("capture_metadata_write_failed")
	}
	return nil
}

// receipt links the canonical usage receipt and marks the send side finished.
func (a *replyAttempt) receipt(r Receipt, canonicalErr error) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.failed || a.done {
		return ErrReplyRecord
	}
	digest, _ := json.Marshal(r)
	link := &replyReceipt{ReceiptSHA256: textHash(string(digest)), RequestID: r.RequestID, ResponseID: r.ResponseID, UnknownReason: r.UnknownReason, FinishReason: r.FinishReason, Canonical: "recorded"}
	switch {
	case errors.Is(canonicalErr, ErrUsagePending):
		link.Canonical = "usage_pending"
	case canonicalErr != nil:
		link.Canonical = "blocked"
	}
	a.record.Receipt, a.dispatched = link, true
	if a.pending != nil {
		return a.commit(*a.pending)
	}
	return nil
}

// finish records the verifier result or why it did not run. The send side may
// still be in its bounded cleanup; then that side commits the terminal record.
func (a *replyAttempt) finish(v replyValidation) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.failed {
		return ErrReplyRecord
	}
	if a.done || a.pending != nil {
		return nil
	}
	if !a.dispatched {
		a.pending = &v
		return nil
	}
	return a.commit(v)
}

func (a *replyAttempt) commit(v replyValidation) error {
	a.record.Validation = &v
	a.record.State, a.record.ReservedBytes = "sent_validation_done", 0
	if v.Result == "not_run" {
		a.record.State = "sent_validation_not_run"
	}
	if a.persist() != nil {
		return a.fail("terminal_write_failed")
	}
	a.release()
	return nil
}

func ensureReplyDir(path string, parents bool) error {
	var err error
	if parents {
		err = os.MkdirAll(path, 0700)
	} else {
		err = os.Mkdir(path, 0700)
		if errors.Is(err, os.ErrExist) {
			err = nil
		}
	}
	if err != nil {
		return ErrReplyRecord
	}
	return checkReplyDir(path)
}

// checkReplyDir rejects symlinks, shared modes and other owners.
func checkReplyDir(path string) error {
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() || info.Mode().Perm()&0077 != 0 || !ownedByProcess(info) {
		return ErrReplyRecord
	}
	return nil
}

// writeReplyFile is the only writer: private temp, write, sync, close, rename
// in the same directory and directory sync. Every error is returned.
func writeReplyFile(dir, name string, data []byte) error {
	f, err := os.CreateTemp(dir, ".tmp-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err := f.Chmod(0600); err != nil {
		f.Close()
		return err
	}
	if _, err := io.Copy(f, bytes.NewReader(data)); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(f.Name(), filepath.Join(dir, name)); err != nil {
		return err
	}
	return syncBudgetDirectory(dir)
}
