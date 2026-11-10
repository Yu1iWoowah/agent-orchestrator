package delegate

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestLabMissingJournalCannotReregisterAndReset(t *testing.T) {
	root := t.TempDir()
	sink := &receiptSink{}
	d := deploymentFixture(t, "openai", "p", "synthetic source")
	c := NewCoordinator(root, sink, nil, nil)
	approveFixture(t, c, d)
	g := c.LabGrant()
	path := filepath.Join(c.budget.Dir, "budget.json")
	c.Close()
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	restarted := NewCoordinator(root, sink, nil, nil)
	defer restarted.Close()
	err := restarted.Register(t.Context(), Registration{Grant: g, Deployment: d, ExternalSpend: map[string]ExternalSpend{"openai": {Known: true, OutsideGateOnly: true}, "anthropic": {Known: true, OutsideGateOnly: true}}})
	if err == nil {
		t.Fatal("missing journal was recreated and authorization reset")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("refusal wrote a new journal")
	}
}

func TestLabMissingPendingReceiptBlocksResume(t *testing.T) {
	root := t.TempDir()
	sink := &receiptSink{err: errors.New("synthetic canonical unavailable")}
	d := deploymentFixture(t, "openai", "p", "synthetic source")
	c := NewCoordinator(root, sink, nil, nil)
	approveFixture(t, c, d)
	var p Profile
	for _, saved := range c.state.History {
		p = saved.Profile
	}
	e := budgetExecution("p", "pending")
	e.SessionID = "s"
	key, err := c.budget.reserve(t.Context(), e, p, nil, textHash("scope"))
	if err != nil {
		t.Fatal(err)
	}
	if err := c.budget.transition(t.Context(), key, "reserved", "dispatched"); err != nil {
		t.Fatal(err)
	}
	if err := c.budget.record(t.Context(), key, budgetReceipt(e, p), sink); !errors.Is(err, ErrUsagePending) {
		t.Fatalf("pending fixture: %v", err)
	}
	g := c.LabGrant()
	path := filepath.Join(c.budget.Dir, key+".pending")
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(c.budget.Dir, "budget.json"))
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
	sink.err = nil
	restarted := NewCoordinator(root, sink, nil, nil)
	defer restarted.Close()
	if err := restarted.Register(t.Context(), Registration{Grant: g, Deployment: d, ExternalSpend: map[string]ExternalSpend{"openai": {Known: true, OutsideGateOnly: true}, "anthropic": {Known: true, OutsideGateOnly: true}}}); err == nil {
		t.Fatal("missing pending receipt was accepted for resume")
	}
	after, err := os.ReadFile(filepath.Join(c.budget.Dir, "budget.json"))
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) || len(sink.receipts) != 0 {
		t.Fatal("incomplete recovery altered hold or invented usage")
	}
}
