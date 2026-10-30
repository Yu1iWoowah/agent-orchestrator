package projectdocuments_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

func TestReplacementBootstrapReadsOnlyVerifiedImmutableHandoff(t *testing.T) {
	for _, changed := range []string{"current", "handoff.md", "transcript.md", "manifest.json", "directory symlink", "living changed", "stale revision"} {
		t.Run(changed, func(t *testing.T) {
			f := setup(t)
			ctx := context.Background()
			op, out := replacementEvidence(t, f, true)
			op, err := f.service.PublishReplacementEvidence(ctx, op.ID, op.Revision, out)
			if err != nil {
				t.Fatal(err)
			}
			op.Phase = domain.ReplacementRetiring
			if saved, err := f.db.SaveOrchestratorReplacement(ctx, op); err != nil || !saved {
				t.Fatal(saved, err)
			}
			op, _, err = f.db.GetOrchestratorReplacement(ctx, op.ID)
			if err != nil {
				t.Fatal(err)
			}
			proof, err := f.service.PrepareReplacementRetirement(ctx, op.ID, op.Revision, out)
			if err != nil {
				t.Fatal(err)
			}
			op, applied, err := f.db.CommitReplacementRetirement(ctx, op.ID, op.Revision, proof.Source(), proof.DocumentRevision(), proof.Check)
			if err != nil || !applied {
				t.Fatal(applied, err)
			}
			// Publish a real target owner receipt; generic saves cannot invent readiness.
			op.SpawnRequestID, op.BootstrapClientID, op.ResumeProbeClientID = "replacement:"+op.ID+":successor", "replacement:"+op.ID+":bootstrap", "replacement:"+op.ID+":probe"
			op.SpawnRequestHash, err = domain.ReplacementSpawnRequestHash(op)
			if err != nil {
				t.Fatal(err)
			}
			if saved, err := f.db.SaveOrchestratorReplacement(ctx, op); err != nil || !saved {
				t.Fatal(saved, err)
			}
			seed, _, err := f.db.CreateClientRequestSession(ctx, domain.SessionRecord{ProjectID: op.ProjectID, Kind: domain.KindOrchestrator, Harness: domain.HarnessClaudeCode, Mode: domain.SessionModeChat, ClientRequestID: op.SpawnRequestID, ClientRequestHash: op.SpawnRequestHash, ProvisionState: domain.SessionProvisionProvisioning, CreatedAt: time.Now(), UpdatedAt: time.Now(), Metadata: domain.SessionMetadata{Model: op.TargetAgentConfig.Model, Effort: op.TargetAgentConfig.Effort, Permissions: op.TargetAgentConfig.Permissions}})
			if err != nil {
				t.Fatal(err)
			}
			if ok, err := f.db.UpdateBrowserCapabilityVerifier(ctx, seed.ID, seed.ControllerOwner(), "target-verifier"); err != nil || !ok {
				t.Fatal(ok, err)
			}
			seed, _, err = f.db.GetSession(ctx, seed.ID)
			if err != nil {
				t.Fatal(err)
			}
			seed.Metadata.ControllerGeneration, seed.Metadata.ProviderConversationID = "target-generation", "target-native"
			if err := f.db.UpdateSession(ctx, seed); err != nil {
				t.Fatal(err)
			}
			op, _, err = f.db.GetOrchestratorReplacement(ctx, op.ID)
			if err != nil {
				t.Fatal(err)
			}
			op.Phase = domain.ReplacementControllerReady
			if ok, err := f.db.SaveOrchestratorReplacement(ctx, op); err != nil || !ok {
				t.Fatal(ok, err)
			}
			op, _, err = f.db.GetOrchestratorReplacement(ctx, op.ID)
			if err != nil {
				t.Fatal(err)
			}
			snapshot, err := os.ReadFile(filepath.Join(op.Transcript.Receipt.Directory, "handoff.md"))
			if err != nil {
				t.Fatal(err)
			}
			if changed == "handoff.md" || changed == "transcript.md" || changed == "manifest.json" {
				if err := os.WriteFile(filepath.Join(op.Transcript.Receipt.Directory, changed), []byte("changed"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if changed == "living changed" {
				if err := os.WriteFile(filepath.Join(op.SharedDir, op.DocumentPath), []byte("a later living version"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if changed == "directory symlink" {
				dir := op.Transcript.Receipt.Directory
				if err := os.Rename(dir, dir+".old"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(dir+".old", dir); err != nil {
					t.Fatal(err)
				}
			}
			revision := op.Revision
			if changed == "stale revision" {
				revision--
			}
			before, err := f.db.GetProjectDocumentState(ctx, op.ProjectID)
			if err != nil {
				t.Fatal(err)
			}
			handoff, err := f.service.ReadReplacementHandoff(ctx, op.ID, revision)
			if changed == "current" || changed == "living changed" {
				if err != nil || handoff.Content != string(snapshot) || handoff.Hash != op.Preparation.SubmittedHash || handoff.Path != filepath.Join(op.Transcript.Receipt.Directory, "handoff.md") {
					t.Fatalf("bounded snapshot=%+v err=%v", handoff, err)
				}
			} else if err == nil {
				t.Fatal("unverified evidence read", changed)
			}
			after, err := f.db.GetProjectDocumentState(ctx, op.ProjectID)
			if err != nil || before.Revision != after.Revision || before.Lease.Epoch != after.Lease.Epoch || !after.Lease.Frozen {
				t.Fatal("read changed writer lease", err)
			}
		})
	}
}
