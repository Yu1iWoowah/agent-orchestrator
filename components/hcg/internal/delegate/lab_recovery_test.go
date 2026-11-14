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

func TestLabRejectedDescriptorPreservesGrantAndAllowsRetry(t *testing.T) {
	for _, family := range []string{"openai", "anthropic"} {
		t.Run(family, func(t *testing.T) {
			c := NewCoordinator(t.TempDir(), &receiptSink{}, nil, nil)
			defer c.Close()
			d := deploymentFixture(t, family, "p", "synthetic source")
			approveFixture(t, c, d)
			paths := []string{filepath.Join(c.dir, "registry.json"), filepath.Join(c.budget.Dir, "budget.json")}
			before := make([]string, len(paths))
			for i, path := range paths {
				raw, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				before[i] = string(raw)
			}
			g := c.LabGrant()
			g.Bindings = append(g.Bindings, LabSession{ProjectID: "p", SessionID: "another-approved-session"})
			request := Registration{Grant: g, Deployment: d, ExternalSpend: map[string]ExternalSpend{"openai": {Known: true, OutsideGateOnly: true}, "anthropic": {Known: true, OutsideGateOnly: true}}}
			request.Deployment.Workspace = "mistaken-workspace"
			if err := c.Register(t.Context(), request); err == nil {
				t.Fatal("conflicting immutable descriptor accepted")
			}
			for i, path := range paths {
				raw, err := os.ReadFile(path)
				if err != nil || string(raw) != before[i] {
					t.Fatal("rejected descriptor changed persistent grant or journal")
				}
			}
			request.Deployment = d
			if err := c.Register(t.Context(), request); err != nil {
				t.Fatalf("corrected registration could not retry: %v", err)
			}
			if !c.LabGrant().bound("p", "another-approved-session") {
				t.Fatal("successful retry lost added binding")
			}
			if _, err := c.checkJournal(t.Context()); err != nil {
				t.Fatalf("successful retry left inconsistent grants: %v", err)
			}
		})
	}
}
