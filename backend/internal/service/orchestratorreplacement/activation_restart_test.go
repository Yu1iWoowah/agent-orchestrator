package orchestratorreplacement_test

import (
	"context"
	"reflect"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/lifecycle"
	docs "github.com/aoagents/agent-orchestrator/backend/internal/service/projectdocuments"
)

func TestReplacementActivatedSuccessorSameNativeRestart(t *testing.T) {
	for _, released := range []bool{false, true} {
		for _, publication := range []bool{false, true} {
			name := "generic update stays fenced"
			if publication {
				name = "verified ready preserves intake"
			}
			if !released {
				name = "before release/" + name
			} else {
				name = "after drain/" + name
			}
			t.Run(name, func(t *testing.T) {
				x := activationCoordinatorFixture(t)
				ctx := context.Background()
				op, err := x.service().Advance(ctx, x.operation.ID)
				if err != nil || op.WriterActivation == nil {
					t.Fatal(op.Error, err)
				}
				if released {
					op, err = x.service().Advance(ctx, op.ID)
					if err != nil || op.IntakeDrainRequestedAt == nil {
						t.Fatal(op.Error, err)
					}
				}
				activation := *op.WriterActivation
				before, err := x.f.store.GetProjectDocumentState(ctx, op.ProjectID)
				if err != nil {
					t.Fatal(err)
				}
				target, _, err := x.f.store.GetSession(ctx, op.SuccessorSessionID)
				if err != nil {
					t.Fatal(err)
				}
				target.Metadata.ControllerGeneration = "version-refresh-generation"
				target.Metadata.BrowserCapabilityVerifier = "version-refresh-capability"
				if released {
					target.Metadata.Model = "updated-chat-model"
				}
				// Start first claims the generation/capability. This alone grants no writer.
				if err := x.f.store.UpdateSession(ctx, target); err != nil {
					t.Fatal(err)
				}
				if publication {
					lc := lifecycle.New(x.f.store.Store, nil)
					if err := lc.MarkSpawned(ctx, target.ID, target.Metadata); err != nil {
						t.Fatal(err)
					}
				}
				fenced, err := x.f.store.ReplacementWakeFenced(ctx, target.ID)
				if err != nil || fenced == (publication && released) {
					t.Fatal("unexpected intake fence", fenced, err)
				}
				after, err := x.f.store.GetProjectDocumentState(ctx, op.ProjectID)
				if err != nil {
					t.Fatal(err)
				}
				if publication {
					if after.Lease.Owner != target.ControllerOwner() || after.Lease.Epoch != before.Lease.Epoch || after.Lease.TokenHash != "" || after.Lease.ClaimVerifier != target.Metadata.BrowserCapabilityVerifier {
						t.Fatal("same native writer was not safely rebound", after.Lease)
					}
					x.f.runtime.successorObservation.Owner = target.ControllerOwner()
					op, err = x.service().Advance(ctx, op.ID)
					if err != nil || op.Phase != domain.ReplacementNotifying || op.Error != nil {
						t.Fatal("lawful restart stranded operation", op.Phase, op.Error, err)
					}
					if !reflect.DeepEqual(activation, *op.WriterActivation) {
						t.Fatal("initial activation receipt was rewritten")
					}
					op.Phase, op.ResumePhase = domain.ReplacementRecoveryRequired, domain.ReplacementNotifying
					op.Error = &domain.ReplacementFailure{Code: "TEST_NOTIFY_READ_FAILURE", Message: "transient read after lawful restart", Retryable: true}
					if saved, err := x.f.store.SaveOrchestratorReplacement(ctx, op); err != nil || !saved {
						t.Fatal(saved, err)
					}
					op, err = x.service().Retry(ctx, op.ID)
					if err != nil || op.Phase != domain.ReplacementNotifying {
						t.Fatal("lawful same-native writer cannot recover notifying", op.Phase, err)
					}
					if fenced, err := x.f.store.ReplacementWakeFenced(ctx, target.ID); err != nil || fenced {
						t.Fatal("retained retry re-held lawful successor", fenced, err)
					}
				} else if !reflect.DeepEqual(before.Lease, after.Lease) {
					t.Fatal("generic update transferred writer authority")
				}
			})
		}
	}
}

func TestReplacementReconnectDoesNotRewriteSessionFacts(t *testing.T) {
	x := activationCoordinatorFixture(t)
	ctx := context.Background()
	op, err := x.service().Advance(ctx, x.operation.ID)
	if err != nil {
		t.Fatal(err)
	}
	op, err = x.service().Advance(ctx, op.ID)
	if err != nil {
		t.Fatal(err)
	}
	rec, _, err := x.f.store.GetSession(ctx, op.SuccessorSessionID)
	if err != nil {
		t.Fatal(err)
	}
	rec.Metadata.ControllerGeneration, rec.Metadata.BrowserCapabilityVerifier = "reconnected-generation", "reconnected-verifier"
	rec.Activity.State = domain.ActivityActive
	if err := x.f.store.UpdateSession(ctx, rec); err != nil {
		t.Fatal(err)
	}
	before, _, err := x.f.store.GetSession(ctx, rec.ID)
	if err != nil {
		t.Fatal(err)
	}
	lc := lifecycle.New(x.f.store.Store, nil)
	if err := lc.MarkChatReconnected(ctx, rec.ID, rec.Metadata); err != nil {
		t.Fatal(err)
	}
	after, _, err := x.f.store.GetSession(ctx, rec.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatal("live reconnect rewrote session facts or revision")
	}
	if fenced, err := x.f.store.ReplacementWakeFenced(ctx, rec.ID); err != nil || fenced {
		t.Fatal("live reconnect did not publish its same-native writer", fenced, err)
	}
}

type restartCapability struct{ target domain.SessionID }

func (c restartCapability) Valid(id domain.SessionID, token, verifier string) bool {
	return id == c.target && (token == "successor-secret" && verifier == "after-verifier" || token == "next-secret" && verifier == "next-verifier")
}

func TestReplacementClaimedLeaseCannotReleaseRotatedCapabilityBeforeReady(t *testing.T) {
	for _, generation := range []bool{false, true} {
		name := "capability only"
		if generation {
			name = "generation and capability"
		}
		t.Run(name, func(t *testing.T) {
			x := activationCoordinatorFixture(t)
			ctx := context.Background()
			op, err := x.service().Advance(ctx, x.operation.ID)
			if err != nil {
				t.Fatal(err)
			}
			op, err = x.service().Advance(ctx, op.ID)
			if err != nil {
				t.Fatal(err)
			}
			service := docs.New(docs.Deps{Store: x.f.store.Store, DataDir: x.documentDataDir, Capabilities: restartCapability{target: op.SuccessorSessionID}})
			original := docs.Actor{SessionID: op.SuccessorSessionID, Capability: "successor-secret"}
			oldGrant, err := service.Acquire(ctx, op.ProjectID, original)
			if err != nil {
				t.Fatal(err)
			}
			target, _, err := x.f.store.GetSession(ctx, op.SuccessorSessionID)
			if err != nil {
				t.Fatal(err)
			}
			target.Metadata.BrowserCapabilityVerifier = "next-verifier"
			if generation {
				target.Metadata.ControllerGeneration = "next-generation"
			}
			if err := x.f.store.UpdateSession(ctx, target); err != nil {
				t.Fatal(err)
			}
			fenced, err := x.f.store.ReplacementWakeFenced(ctx, target.ID)
			if err != nil || !fenced {
				t.Fatal("unpublished capability released intake", fenced, err)
			}
			actor := docs.Actor{SessionID: target.ID, Capability: "next-secret"}
			if _, err := service.Acquire(ctx, op.ProjectID, actor); err == nil {
				t.Fatal("Acquire replaced an active operation's writer before ready")
			}
			lc := lifecycle.New(x.f.store.Store, nil)
			if err := lc.MarkSpawned(ctx, target.ID, target.Metadata); err != nil {
				t.Fatal(err)
			}
			grant, err := service.Acquire(ctx, op.ProjectID, actor)
			if err != nil || grant.Epoch != oldGrant.Epoch || grant.Token == oldGrant.Token {
				t.Fatal("managed continuation could not claim its new capability", grant.Epoch, err)
			}
			if _, err := service.Acquire(ctx, op.ProjectID, original); err == nil {
				t.Fatal("old capability survived ready publication")
			}
			if fenced, err := x.f.store.ReplacementWakeFenced(ctx, target.ID); err != nil || fenced {
				t.Fatal("claimed continuation remained fenced", fenced, err)
			}
		})
	}
}

func TestReplacementCompletedLeaseStillAllowsOrdinaryControllerRenewal(t *testing.T) {
	x := activationCoordinatorFixture(t)
	ctx := context.Background()
	worker, _, err := x.f.store.GetSession(ctx, x.worker.ID)
	if err != nil {
		t.Fatal(err)
	}
	worker.IsTerminated = true
	if err := x.f.store.UpdateSession(ctx, worker); err != nil {
		t.Fatal(err)
	}
	op, err := x.service().Advance(ctx, x.operation.ID)
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		op, err = x.service().Advance(ctx, op.ID)
		if err != nil {
			t.Fatal(err)
		}
	}
	if op.Phase != domain.ReplacementCompleted {
		t.Fatal(op.Phase)
	}
	target, _, err := x.f.store.GetSession(ctx, op.SuccessorSessionID)
	if err != nil {
		t.Fatal(err)
	}
	target.Metadata.ControllerGeneration, target.Metadata.BrowserCapabilityVerifier = "ordinary-generation", "next-verifier"
	if err := x.f.store.UpdateSession(ctx, target); err != nil {
		t.Fatal(err)
	}
	service := docs.New(docs.Deps{Store: x.f.store.Store, DataDir: x.documentDataDir, Capabilities: restartCapability{target: target.ID}})
	grant, err := service.Acquire(ctx, op.ProjectID, docs.Actor{SessionID: target.ID, Capability: "next-secret"})
	if err != nil || grant.Epoch != op.WriterActivation.AfterEpoch+1 {
		t.Fatal("completed operation blocked ordinary renewal", grant.Epoch, err)
	}
	state, err := x.f.store.GetProjectDocumentState(ctx, op.ProjectID)
	if err != nil || state.Lease.ReplacementID != "" {
		t.Fatal("completed lease was retained as an active reservation", err)
	}
}
