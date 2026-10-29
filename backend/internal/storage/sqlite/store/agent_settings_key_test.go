package store_test

import (
	"bytes"
	"errors"
	"sync"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

func TestAgentSettingsSigningKeyIsStableAtomicAndPrivate(t *testing.T) {
	s, _, rec := settingsFixture(t)
	before, err := s.SessionAgentSettings(t.Context(), rec.ID)
	if err != nil {
		t.Fatal(err)
	}
	candidate := bytes.Repeat([]byte{1}, 32)
	key, err := s.EnsureAgentSettingsSigningKey(t.Context(), candidate)
	if err != nil || !bytes.Equal(key, candidate) {
		t.Fatal("initial key was not persisted")
	}
	key[0] = 9
	candidate[0] = 9
	var wg sync.WaitGroup
	for i := byte(2); i < 18; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			key, err := s.EnsureAgentSettingsSigningKey(t.Context(), bytes.Repeat([]byte{i}, 32))
			if err != nil || !bytes.Equal(key, bytes.Repeat([]byte{1}, 32)) {
				t.Error("parallel startup replaced signing key")
			}
		}()
	}
	wg.Wait()
	if _, err := s.EnsureAgentSettingsSigningKey(t.Context(), []byte("short")); !errors.Is(err, ports.ErrAgentSettingsInvalid) {
		t.Fatalf("invalid key accepted: %v", err)
	}
	after, err := s.SessionAgentSettings(t.Context(), rec.ID)
	if err != nil || before != after {
		t.Fatal("private key initialization changed session facts")
	}
}

func TestAgentSettingsSigningKeyInsertFailureRollsBack(t *testing.T) {
	s, db, _ := settingsFixture(t)
	if _, err := db.Exec(`CREATE TRIGGER reject_settings_key BEFORE INSERT ON agent_settings_authority_keys BEGIN SELECT RAISE(ABORT,'key-injected'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.EnsureAgentSettingsSigningKey(t.Context(), bytes.Repeat([]byte{1}, 32)); err == nil {
		t.Fatal("injected key write failure ignored")
	}
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM agent_settings_authority_keys`).Scan(&count); err != nil || count != 0 {
		t.Fatal("failed key write left partial state")
	}
	if _, err := db.Exec(`DROP TRIGGER reject_settings_key`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.EnsureAgentSettingsSigningKey(t.Context(), bytes.Repeat([]byte{2}, 32)); err != nil {
		t.Fatal(err)
	}
}
