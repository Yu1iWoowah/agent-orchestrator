package projectdocuments_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/agent/claudecode"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	docs "github.com/aoagents/agent-orchestrator/backend/internal/service/projectdocuments"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/transcriptexport"
)

type nativeMutationStore struct {
	docs.Store
	mutate  func() error
	changed bool
}

func (s *nativeMutationStore) WithProjectDocumentPublication(ctx context.Context, id domain.ProjectID, expected domain.SessionRecord, revision int64, publish func(context.Context) error) (bool, error) {
	if err := s.mutate(); err != nil {
		return false, err
	}
	s.changed = true
	return s.Store.WithProjectDocumentPublication(ctx, id, expected, revision, publish)
}
func nativeEvidenceInput(t *testing.T, reply string) (fixture, docs.EvidenceInput, string, domain.TranscriptSourceSnapshot) {
	t.Helper()
	f := setup(t)
	ctx := context.Background()
	id := "11111111-1111-4111-8111-111111111111"
	owner := f.owner
	owner.Metadata.ProviderConversationID = id
	if err := f.db.UpdateSession(ctx, owner); err != nil {
		t.Fatal(err)
	}
	grant, err := f.service.Acquire(ctx, "demo", f.actor)
	if err != nil {
		t.Fatal(err)
	}
	doc, err := f.service.Write(ctx, "demo", docs.WriteInput{Actor: f.actor, Lease: grant, Path: "handoff/orchestrator-handoff.md", ExpectedHash: docs.MissingHash, Content: "# frozen audit"})
	if err != nil {
		t.Fatal(err)
	}
	if err = f.service.Freeze(ctx, "demo", f.actor, grant); err != nil {
		t.Fatal(err)
	}
	config := t.TempDir()
	dir := filepath.Join(config, "projects", "workspace")
	if err = os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	data := `{"uuid":"u","parentUuid":null,"type":"user","sessionId":"11111111-1111-4111-8111-111111111111","version":"2.1.0","isSidechain":false,"promptId":"p","message":{"content":"audit request"}}` + "\n" +
		`{"uuid":"a","parentUuid":"u","type":"assistant","sessionId":"11111111-1111-4111-8111-111111111111","version":"2.1.0","isSidechain":false,"promptId":"p","message":{"content":"` + reply + `","stop_reason":"end_turn"}}` + "\n"
	nativePath := filepath.Join(dir, id+".jsonl")
	if err = os.WriteFile(nativePath, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	binding, err := f.db.UpsertUsageBinding(ctx, domain.UsageBindingRecord{SessionID: owner.ID, Harness: domain.HarnessClaudeCode, NativeRootID: id})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.db.InsertUsageSource(ctx, domain.UsageSourceRecord{BindingID: binding.ID, Kind: domain.UsageSourceClaudeMain, NativeSessionID: id, ArtifactPath: nativePath, Generation: 1}); err != nil {
		t.Fatal(err)
	}
	snapshot, err := f.db.LoadTranscriptSourceSnapshot(ctx, owner.ID)
	if err != nil {
		t.Fatal(err)
	}
	out, err := transcriptexport.Build(ctx, snapshot, []transcriptexport.EpochCutoff{{NativeID: id, Bytes: int64(len(data)), LeafID: "a", Schema: ports.ClaudeTranscriptSchemaV1}}, claudecode.New())
	if err != nil {
		t.Fatal(err)
	}
	in := docs.EvidenceInput{Actor: f.actor, Lease: grant, OperationID: "11111111-1111-4111-8111-111111111111", AttemptID: "22222222-2222-4222-8222-222222222222", HandoffPath: doc.Path, HandoffHash: doc.Hash, Export: out, CreatedAt: time.Now()}
	return f, in, nativePath, snapshot
}

func TestHandoffEvidenceNativeChangeBetweenPreflightAndReservation(t *testing.T) {
	if nativePublicationUnsupported(t) {
		return
	}
	for _, name := range []string{"append pending hook", "same-size tail rewrite preserving mtime", "replace inode preserving bytes and mtime"} {
		t.Run(name, func(t *testing.T) {
			f, in, nativePath, snapshot := nativeEvidenceInput(t, "settled audit reply")
			ctx := context.Background()
			hooked := &nativeMutationStore{Store: f.db, mutate: func() error {
				data, err := os.ReadFile(nativePath)
				if err != nil {
					return err
				}
				info, err := os.Stat(nativePath)
				if err != nil {
					return err
				}
				if name == "append pending hook" {
					data = append(data, []byte(`{"uuid":"pending","parentUuid":"a","type":"system","subtype":"stop_hook_summary","preventedContinuation":true,"sessionId":"11111111-1111-4111-8111-111111111111","version":"2.1.0","isSidechain":false}`+"\n")...)
					return os.WriteFile(nativePath, data, 0600)
				}
				if name == "same-size tail rewrite preserving mtime" {
					data = []byte(strings.Replace(string(data), "settled audit reply", "changed audit reply", 1))
					if err = os.WriteFile(nativePath, data, 0600); err != nil {
						return err
					}
					return os.Chtimes(nativePath, info.ModTime(), info.ModTime())
				}
				replacement := nativePath + ".replacement"
				if err = os.WriteFile(replacement, data, 0600); err != nil {
					return err
				}
				if err = os.Chtimes(replacement, info.ModTime(), info.ModTime()); err != nil {
					return err
				}
				return os.Rename(replacement, nativePath)
			}}
			svc := docs.New(docs.Deps{Store: hooked, DataDir: f.root, Capabilities: capability{}})
			_, publicationErr := svc.PublishEvidence(ctx, "demo", in)
			if !hooked.changed {
				t.Fatal("fixture never crossed the native preflight boundary")
			}
			var failure *ports.NativeTranscriptError
			if !errors.As(publicationErr, &failure) || failure.Code != "TRANSCRIPT_SOURCE_CHANGED" {
				t.Fatalf("expected native fingerprint rejection, got %v", publicationErr)
			}
			after, err := f.db.LoadTranscriptSourceSnapshot(ctx, f.owner.ID)
			if err != nil {
				t.Fatal(err)
			}
			if after.Fingerprint != snapshot.Fingerprint {
				t.Fatal("fixture changed AO fingerprint")
			}
			if _, err = f.service.ReadEvidence(ctx, "demo", in.OperationID, in.AttemptID, "transcript.md"); err == nil {
				t.Fatal("rejected evidence remained readable")
			}
			attempt := filepath.Join(f.root, "projects/demo/orchestrator/handoffs", in.OperationID, in.AttemptID)
			if _, err = os.Stat(attempt); !os.IsNotExist(err) {
				t.Fatalf("rejected attempt was published: %v", err)
			}
		})
	}
}

func TestHandoffEvidenceStableLargeNativeFile(t *testing.T) {
	if nativePublicationUnsupported(t) {
		return
	}
	f, in, _, _ := nativeEvidenceInput(t, strings.Repeat("long verified reply ", 10000))
	ctx := context.Background()
	if _, err := f.service.PublishEvidence(ctx, "demo", in); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.PublishEvidence(ctx, "demo", in); err != nil {
		t.Fatalf("stable retry: %v", err)
	}
	if _, err := f.service.ReadEvidence(ctx, "demo", in.OperationID, in.AttemptID, "transcript.md"); err != nil {
		t.Fatal(err)
	}
}

func TestHandoffEvidenceNativeFingerprintMutationMatrix(t *testing.T) {
	if nativePublicationUnsupported(t) {
		return
	}
	cases := []string{"append", "truncate", "replace-inode", "prefix-new-mtime", "tail-preserve-mtime", "small-prefix-preserve-mtime", "large-prefix-preserve-mtime"}
	for _, name := range cases {
		t.Run(name, func(t *testing.T) {
			reply := strings.Repeat("long verified reply ", 10000) + "TAILMARK_A"
			if name == "small-prefix-preserve-mtime" {
				reply = "settled reply TAILMARK_A"
			}
			f, in, path, snapshot := nativeEvidenceInput(t, reply)
			ctx := context.Background()
			mutations := 0
			hooked := &nativeMutationStore{Store: f.db, mutate: func() error {
				mutations++
				data, err := os.ReadFile(path)
				if err != nil {
					return err
				}
				info, err := os.Stat(path)
				if err != nil {
					return err
				}
				original := string(data)
				switch name {
				case "append":
					data = append(data, []byte("{}\n")...)
				case "truncate":
					data = data[:len(data)-1]
				case "replace-inode":
					replacement := path + ".replacement"
					if err = os.WriteFile(replacement, data, 0600); err != nil {
						return err
					}
					if err = os.Chtimes(replacement, info.ModTime(), info.ModTime()); err != nil {
						return err
					}
					return os.Rename(replacement, path)
				case "tail-preserve-mtime":
					data = []byte(strings.Replace(original, "TAILMARK_A", "TAILMARK_B", 1))
				default:
					data = []byte(strings.Replace(original, "audit request", "other request", 1))
				}
				if strings.Contains(name, "prefix") || name == "tail-preserve-mtime" {
					if len(data) != len(original) || string(data) == original {
						t.Fatal("invalid equal-size rewrite fixture")
					}
				}
				if name == "large-prefix-preserve-mtime" {
					if len(data) <= 64<<10 || string(data[len(data)-(64<<10):]) != original[len(original)-(64<<10):] {
						t.Fatal("mutation was not outside the tail window")
					}
				}
				if err = os.WriteFile(path, data, 0600); err != nil {
					return err
				}
				if strings.Contains(name, "preserve-mtime") {
					return os.Chtimes(path, info.ModTime(), info.ModTime())
				}
				if name == "prefix-new-mtime" {
					return os.Chtimes(path, info.ModTime(), info.ModTime().Add(time.Second))
				}
				return nil
			}}
			svc := docs.New(docs.Deps{Store: hooked, DataDir: f.root, Capabilities: capability{}})
			_, publicationErr := svc.PublishEvidence(ctx, "demo", in)
			if !hooked.changed || mutations != 1 {
				t.Fatalf("unexpected mutation/retry count %d", mutations)
			}
			after, err := f.db.LoadTranscriptSourceSnapshot(ctx, f.owner.ID)
			if err != nil {
				t.Fatal(err)
			}
			if after.Fingerprint != snapshot.Fingerprint {
				t.Fatal("fixture changed AO fingerprint")
			}
			file, readErr := f.service.ReadEvidence(ctx, "demo", in.OperationID, in.AttemptID, "transcript.md")
			if publicationErr == nil {
				t.Fatalf("changed native file published: case=%s coverage=%s readable=%t originalPromptInEvidence=%t fullRevalidate=%v", name, in.Export.Manifest.Coverage, readErr == nil, strings.Contains(file.Content, "audit request"), in.Export.Revalidate(ctx))
			}
			var failure *ports.NativeTranscriptError
			if !errors.As(publicationErr, &failure) || failure.Code != "TRANSCRIPT_SOURCE_CHANGED" {
				t.Fatalf("wrong rejection: %v", publicationErr)
			}
			if readErr == nil {
				t.Fatal("rejected evidence readable")
			}
			attempt := filepath.Join(f.root, "projects/demo/orchestrator/handoffs", in.OperationID, in.AttemptID)
			if _, err = os.Stat(attempt); !os.IsNotExist(err) {
				t.Fatalf("failed attempt occupied final path: %v", err)
			}
			t.Logf("%s rejected; mutation/reservation calls=%d; code=%s", name, mutations, failure.Code)
		})
	}
}

type countingNativeReader struct {
	reader        ports.NativeTranscriptReader
	reads, checks int
}

func (r *countingNativeReader) ReadNativeTranscript(ctx context.Context, req ports.NativeTranscriptRequest) (ports.NativeTranscriptSegment, error) {
	r.reads++
	segment, err := r.reader.ReadNativeTranscript(ctx, req)
	check := segment.CheckStability
	if check != nil {
		segment.CheckStability = func(ctx context.Context) error { r.checks++; return check(ctx) }
	}
	return segment, err
}
func TestHandoffEvidenceFreshWitnessWithoutImplicitRetries(t *testing.T) {
	if nativePublicationUnsupported(t) {
		return
	}
	f, in, path, snapshot := nativeEvidenceInput(t, strings.Repeat("stable reply ", 10000))
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	reader := &countingNativeReader{reader: claudecode.New()}
	id := in.Export.Manifest.Segments[0].NativeID
	in.Export, err = transcriptexport.Build(context.Background(), snapshot, []transcriptexport.EpochCutoff{{NativeID: id, Bytes: info.Size(), LeafID: "a", Schema: ports.ClaudeTranscriptSchemaV1}}, reader)
	if err != nil {
		t.Fatal(err)
	}
	var receipt domain.HandoffEvidenceReceipt
	for i := 0; i < 2; i++ {
		got, err := f.service.PublishEvidence(context.Background(), "demo", in)
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			receipt = got
		} else if got != receipt {
			t.Fatal("stable retry receipt changed")
		}
	}
	if reader.reads != 3 || reader.checks != 2 {
		t.Fatalf("unexpected read/check/retry counts: reads=%d publicationChecks=%d", reader.reads, reader.checks)
	}
	t.Logf("one Build plus two publications: strictReads=%d publicationWitnessChecks=%d", reader.reads, reader.checks)
}

// On unsupported platforms these tests assert publication refusal explicitly;
// they do not claim that the native mutation matrix is supported there.
func nativePublicationUnsupported(t *testing.T) bool {
	t.Helper()
	if runtime.GOOS == "linux" || runtime.GOOS == "darwin" {
		return false
	}
	f, in, _, _ := nativeEvidenceInput(t, "unsupported platform fixture")
	ctx := context.Background()
	_, err := f.service.PublishEvidence(ctx, "demo", in)
	var failure *ports.NativeTranscriptError
	if !errors.As(err, &failure) || failure.Code != "TRANSCRIPT_STABILITY_UNSUPPORTED" {
		t.Fatalf("unsupported platform did not refuse publication: %v", err)
	}
	if _, err = f.service.ReadEvidence(ctx, "demo", in.OperationID, in.AttemptID, "transcript.md"); err == nil {
		t.Fatal("unsupported evidence became readable")
	}
	attempt := filepath.Join(f.root, "projects/demo/orchestrator/handoffs", in.OperationID, in.AttemptID)
	if _, err = os.Stat(attempt); !os.IsNotExist(err) {
		t.Fatalf("unsupported attempt occupied final path: %v", err)
	}
	t.Log("no verified native change time on this platform; publication refused, mutation support not claimed")
	return true
}
