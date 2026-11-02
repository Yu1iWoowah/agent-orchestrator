package operatornative

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
)

const testToken = "test-owned-issuer-token-kept-in-memory-only"

func nativeFixture(t *testing.T) (*Broker, *sqlite.Store, string) {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("native issuer is fail-closed pending platform runtime receipts")
	}
	dir := t.TempDir()
	s, err := sqlite.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err := s.UpsertProject(context.Background(), domain.ProjectRecord{ID: "project", Path: dir, RegisteredAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	b := New(s.NativeOperatorIssuer(), testToken, nil)
	// Protocol fixtures inject peer qualification only here. The real Electron
	// integration uses New unchanged and verifies kernel identity + pinned code.
	b.verifyPeer = func(net.Conn) error { return nil }
	b.preflightPeer = func(net.Conn) error { return nil }
	b.pinStatus = func() error { return nil }
	return b, s, dir
}

func TestNativePlatformQualification(t *testing.T) {
	if New(nil, "", nil).platformVerified != (runtime.GOOS == "linux") {
		t.Fatal("only Linux may qualify before other platform runtime receipts")
	}
}

func TestNativeUnverifiedPlatformFailsClosed(t *testing.T) {
	for _, mode := range []string{"owned", "attach"} {
		t.Run(mode, func(t *testing.T) {
			b := New(nil, testToken, nil)
			// Exercise a closed release gate on every CI OS, even if all other
			// readiness facts have been populated. No store call is permissible.
			b.platformVerified = false
			b.ready, b.epoch, b.liveUntil = true, "issuer", time.Now().Add(time.Minute)
			b.verifyPeer = func(net.Conn) error {
				t.Error("unverified platform reached peer qualification")
				return nil
			}
			plan := planFor(domain.ConsentSettingsPermissions)
			readiness := b.GetAuthorityReadiness([]domain.OperatorPurpose{plan.Purpose}, plan.Scope)
			if readiness.AuthorityGo || readiness.NativeIssuerDelivered || readiness.Reason != "native_platform_unverified" || readiness.IssuerEpoch != "" {
				t.Fatalf("unverified platform qualified: %+v", readiness)
			}
			hello := frame{Type: "operator-hello", Version: 1}
			if mode == "owned" {
				hello.Token = testToken
			}
			c := dialNative(t, b, hello)
			_ = c.conn.SetReadDeadline(time.Now().Add(time.Second))
			var reply frame
			if err := c.decoder.Decode(&reply); err != nil || reply.Type != "operator-hello-rejected" || reply.Code != "OPERATOR_PLATFORM_UNVERIFIED" {
				t.Fatalf("unverified platform did not reject native channel: %+v %v", reply, err)
			}
		})
	}
}

func planFor(purpose domain.OperatorPurpose) domain.OperatorActionPlan {
	d, _ := purpose.Domain()
	return domain.OperatorActionPlan{Protocol: domain.OperatorConsentProtocol, Purpose: purpose, Domain: d, ActionKey: "action-" + string(purpose), SubjectRef: "subject", ParticipantRefs: []string{"session"}, Scope: domain.OperatorActionScope{ProjectRef: "project", OwnerRef: "owner", BranchRef: "branch", NativeRef: "native"}, ExpectedRevisions: map[string]int64{"owner": 1, "recovery_epoch": 2}, PayloadDigest: strings.Repeat("a", 64), EffectDigest: strings.Repeat("b", 64), ValidUntil: time.Now().UTC().Add(time.Minute).Truncate(time.Millisecond), RiskCodes: []string{"MANUAL_RECOVERY"}}
}

type nativeClient struct {
	conn    net.Conn
	decoder *json.Decoder
	done    chan struct{}
}

func dialNative(t *testing.T, b *Broker, hello frame) *nativeClient {
	t.Helper()
	server, client := net.Pipe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer server.Close()
		scan := bufio.NewScanner(server)
		if scan.Scan() {
			b.Handle(context.Background(), server, scan, append([]byte{}, scan.Bytes()...))
		}
	}()
	c := &nativeClient{conn: client, decoder: json.NewDecoder(client), done: done}
	t.Cleanup(func() {
		_ = client.Close()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("native handler leaked")
		}
	})
	c.send(t, hello)
	return c
}

func (c *nativeClient) send(t *testing.T, f frame) {
	t.Helper()
	_ = c.conn.SetWriteDeadline(time.Now().Add(3 * time.Second))
	if err := json.NewEncoder(c.conn).Encode(f); err != nil {
		t.Fatal(err)
	}
}
func (c *nativeClient) receive(t *testing.T) frame {
	t.Helper()
	_ = c.conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	var f frame
	if err := c.decoder.Decode(&f); err != nil {
		t.Fatal(err)
	}
	return f
}
func (c *nativeClient) call(t *testing.T, in frame) frame {
	t.Helper()
	c.send(t, in)
	return c.receive(t)
}

func readyNative(t *testing.T, b *Broker) *nativeClient {
	t.Helper()
	c := dialNative(t, b, frame{Type: "operator-hello", Version: 1, Token: testToken})
	if got := c.receive(t); got.Type != "operator-hello-ok" {
		t.Fatalf("hello: %+v", got)
	}
	if got := c.call(t, frame{Type: "ready"}); got.Type != "pong" {
		t.Fatalf("ready: %+v", got)
	}
	return c
}

func TestNativeOwnedAuthorityAndRestart(t *testing.T) {
	b, s, dir := nativeFixture(t)
	ctx := context.Background()
	plan := planFor(domain.ConsentHandoffRetryRestore)
	receipt, err := s.PrepareOperatorAction(ctx, plan)
	if err != nil {
		t.Fatal(err)
	}
	if b.GetAuthorityReadiness([]domain.OperatorPurpose{plan.Purpose}, plan.Scope).AuthorityGo {
		t.Fatal("ready without UI")
	}
	c := readyNative(t, b)
	initialReadiness := b.GetAuthorityReadiness([]domain.OperatorPurpose{plan.Purpose}, plan.Scope)
	if !initialReadiness.AuthorityGo {
		t.Fatal("qualified owned issuer unavailable")
	}
	if initialReadiness.QualificationRef == "" || initialReadiness.QualificationRef == initialReadiness.IssuerEpoch {
		t.Fatal("qualification reference aliases issuer epoch")
	}
	for _, purpose := range []domain.OperatorPurpose{domain.ConsentCoordPolicy, "wildcard.*"} {
		if b.GetAuthorityReadiness([]domain.OperatorPurpose{purpose}, plan.Scope).AuthorityGo {
			t.Fatal("unqualified purpose")
		}
	}
	if b.GetAuthorityReadiness(nil, plan.Scope).AuthorityGo || b.GetAuthorityReadiness([]domain.OperatorPurpose{plan.Purpose}, domain.OperatorActionScope{}).AuthorityGo {
		t.Fatal("missing purpose or scope")
	}
	review := c.call(t, frame{Type: "review", RequestRef: receipt.RequestRef, RequestID: "review-1"})
	if review.Type != "review-plan" || review.Plan == nil || review.RequestID != "review-1" || review.Challenge == "" {
		t.Fatal("missing fixed plan")
	}
	for _, in := range []frame{
		{Type: "confirm", RequestRef: receipt.RequestRef, Challenge: review.Challenge, Digest: strings.Repeat("c", 64)},
		{Type: "confirm", RequestRef: receipt.RequestRef, Challenge: "request-ref-is-not-a-challenge", Digest: review.Digest},
		{Type: "confirm", RequestRef: "another-request", Challenge: review.Challenge, Digest: review.Digest},
	} {
		if got := c.call(t, in); got.Type != "error" {
			t.Fatal("mismatched confirmation accepted")
		}
	}
	confirm := frame{Type: "confirm", RequestRef: receipt.RequestRef, Challenge: review.Challenge, Digest: review.Digest, RequestID: "confirm-1"}
	first := c.call(t, confirm)
	second := c.call(t, confirm)
	if first.Type != "confirmed" || first.ConsentRef == "" || first.ConsentRef != second.ConsentRef || first.RequestID != "confirm-1" {
		t.Fatal("confirm replay was not idempotent")
	}
	state, err := s.GetOperatorAction(ctx, plan.ActionKey)
	if err != nil || state.State != domain.OperatorActionConfirmed {
		t.Fatalf("state: %+v %v", state, err)
	}
	privateRef, err := s.GetOperatorActionConsent(ctx, plan)
	if err != nil || privateRef != first.ConsentRef {
		t.Fatal("domain cannot retrieve exact confirmed plan")
	}
	changed := plan
	changed.Scope.OwnerRef = "another-owner"
	if _, err := s.GetOperatorActionConsent(ctx, changed); err == nil {
		t.Fatal("domain received consent for a different plan")
	}
	_ = c.conn.Close()
	<-c.done
	if b.GetAuthorityReadiness([]domain.OperatorPurpose{plan.Purpose}, plan.Scope).AuthorityGo {
		t.Fatal("disconnected issuer ready")
	}
	state, err = s.GetOperatorAction(ctx, plan.ActionKey)
	if err != nil || state.State != domain.OperatorActionStale {
		t.Fatalf("old proof: %+v %v", state, err)
	}
	c = readyNative(t, b)
	if got := c.call(t, confirm); got.Type != "error" {
		t.Fatal("old challenge survived issuer restart")
	}
	_, err = s.PrepareOperatorAction(ctx, plan)
	if err != nil {
		t.Fatal(err)
	}
	review = c.call(t, frame{Type: "review", RequestRef: receipt.RequestRef})
	if got := c.call(t, frame{Type: "confirm", RequestRef: receipt.RequestRef, Challenge: review.Challenge, Digest: review.Digest}); got.Type != "confirmed" || got.ConsentRef == first.ConsentRef {
		t.Fatal("fresh epoch did not remint")
	}
	// Only harmless plan/reference/epoch facts are durable, never mint material.
	err = filepath.WalkDir(dir, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if bytes.Contains(data, []byte(testToken)) || bytes.Contains(data, []byte(review.Challenge)) {
			t.Errorf("mint material persisted in %s", filepath.Base(path))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

type blockedIssuerStore struct {
	nativeStore
	started chan struct{}
	resume  chan struct{}
}

func (s blockedIssuerStore) SetNativeOperatorIssuer(ctx context.Context, epoch string) error {
	if epoch != "" {
		close(s.started)
		<-s.resume
	}
	return s.nativeStore.SetNativeOperatorIssuer(ctx, epoch)
}

func TestNativeReadinessDoesNotWaitForStoreWriter(t *testing.T) {
	b, _, _ := nativeFixture(t)
	blocked := blockedIssuerStore{nativeStore: b.store, started: make(chan struct{}), resume: make(chan struct{})}
	b.store = blocked
	c := dialNative(t, b, frame{Type: "operator-hello", Version: 1, Token: testToken})
	<-blocked.started
	result := make(chan domain.OperatorAuthorityReadiness, 1)
	go func() { result <- b.GetAuthorityReadiness(nil, domain.OperatorActionScope{}) }()
	select {
	case got := <-result:
		if got.AuthorityGo {
			t.Error("issuer ready before installation")
		}
	case <-time.After(time.Second):
		t.Error("readiness held behind the store writer")
	}
	close(blocked.resume)
	if c.receive(t).Type != "operator-hello-ok" {
		t.Fatal("issuer did not resume")
	}
}

func TestNativeRejectsMissingPairingAndOtherCapabilities(t *testing.T) {
	b, _, _ := nativeFixture(t)
	for _, token := range []string{"", "browser-token", "diagnostic-token", "request-ref", testToken + "wrong"} {
		t.Run(token, func(t *testing.T) {
			if token == "" {
				b.verifyPeer = func(net.Conn) error { return errUnqualified }
			} else {
				b.verifyPeer = func(net.Conn) error { return nil }
			}
			c := dialNative(t, b, frame{Type: "operator-hello", Version: 1, Token: token})
			_ = c.conn.SetReadDeadline(time.Now().Add(time.Second))
			var out frame
			if err := c.decoder.Decode(&out); err != nil || out.Type != "operator-hello-rejected" {
				t.Fatal("unqualified caller got a native channel")
			}
		})
	}
}

func TestNativeCLIBootstrapTokenAloneIsNotAnIssuer(t *testing.T) {
	_, store, _ := nativeFixture(t)
	b := New(store.NativeOperatorIssuer(), testToken, nil)
	c := dialNative(t, b, frame{Type: "operator-hello", Version: 1, Token: testToken})
	_ = c.conn.SetReadDeadline(time.Now().Add(time.Second))
	var response frame
	if err := c.decoder.Decode(&response); err != nil || response.Type != "operator-hello-rejected" {
		t.Fatal("caller-chosen stdin token bypassed native peer qualification")
	}
}

func TestNativeThreePurposeFamiliesAndCancellation(t *testing.T) {
	b, s, _ := nativeFixture(t)
	c := readyNative(t, b)
	for _, purpose := range []domain.OperatorPurpose{domain.ConsentHandoffContext, domain.ConsentSettingsPermissions, domain.ConsentDeliveryDefer} {
		p := planFor(purpose)
		receipt, err := s.PrepareOperatorAction(context.Background(), p)
		if err != nil {
			t.Fatal(err)
		}
		review := c.call(t, frame{Type: "review-next"})
		if review.Type != "review-plan" {
			t.Fatal("no admitted plan")
		}
		if got := c.call(t, frame{Type: "cancel", RequestRef: receipt.RequestRef}); got.Type != "cancelled" {
			t.Fatal("cancel failed")
		}
		if got := c.call(t, frame{Type: "confirm", RequestRef: receipt.RequestRef, Challenge: review.Challenge, Digest: review.Digest}); got.Type != "error" {
			t.Fatal("cancelled challenge minted")
		}
		review = c.call(t, frame{Type: "review", RequestRef: receipt.RequestRef})
		if got := c.call(t, frame{Type: "confirm", RequestRef: receipt.RequestRef, Challenge: review.Challenge, Digest: review.Digest}); got.Type != "confirmed" {
			t.Fatal("family failed")
		}
		if err := s.RevokeUnused(context.Background(), receipt.RequestRef); err != nil {
			t.Fatal(err)
		}
	}
}

func TestNativeReviewNextAdvancesAfterCancelOrConfirm(t *testing.T) {
	for _, decision := range []string{"cancel", "confirm"} {
		t.Run(decision, func(t *testing.T) {
			b, s, _ := nativeFixture(t)
			c := readyNative(t, b)
			a := planFor(domain.ConsentSettingsPermissions)
			first, err := s.PrepareOperatorAction(context.Background(), a)
			if err != nil {
				t.Fatal(err)
			}
			time.Sleep(2 * time.Millisecond)
			secondPlan := a
			secondPlan.ActionKey = "second-action"
			second, err := s.PrepareOperatorAction(context.Background(), secondPlan)
			if err != nil {
				t.Fatal(err)
			}
			review := c.call(t, frame{Type: "review-next"})
			if review.RequestRef != first.RequestRef {
				t.Fatalf("first review: %+v", review)
			}
			reply := c.call(t, frame{Type: decision, RequestRef: review.RequestRef, Challenge: review.Challenge, Digest: review.Digest})
			if reply.Type != "cancelled" && reply.Type != "confirmed" {
				t.Fatalf("decision: %+v", reply)
			}
			next := c.call(t, frame{Type: "review-next"})
			if next.Type != "review-plan" || next.RequestRef != second.RequestRef {
				t.Fatalf("next plan blocked: %+v", next)
			}
			if decision == "confirm" {
				// Wraparound must still exclude A's live consent, without a domain consume.
				next = c.call(t, frame{Type: "review-next"})
				if next.RequestRef != second.RequestRef {
					t.Fatalf("live consent reviewed again: %+v", next)
				}
				_ = c.conn.Close()
				<-c.done
				c = readyNative(t, b)
				next = c.call(t, frame{Type: "review-next"})
				if next.RequestRef != first.RequestRef {
					t.Fatalf("stale issuer consent was hidden: %+v", next)
				}
			}
		})
	}
}

func TestNativeBusyConnectionDoesNotVerifyPeer(t *testing.T) {
	b, _, _ := nativeFixture(t)
	_ = readyNative(t, b)
	b.verifyPeer = func(net.Conn) error { t.Error("occupied issuer triggered peer IO"); return errUnqualified }
	extra := dialNative(t, b, frame{Type: "operator-hello", Version: 1, Token: testToken})
	if reply := extra.receive(t); reply.Type != "operator-hello-rejected" || reply.Code != "OPERATOR_ISSUER_BUSY" {
		t.Fatalf("busy rejection: %+v", reply)
	}
}

func TestNativePreflightRejectsBeforeReservingSlotOrHashing(t *testing.T) {
	b, _, _ := nativeFixture(t)
	b.preflightPeer = func(net.Conn) error {
		b.mu.Lock()
		defer b.mu.Unlock()
		if b.conn != nil {
			t.Error("unqualified kernel peer already reserved the slot")
		}
		return errUnqualified
	}
	b.verifyPeer = func(net.Conn) error { t.Error("unqualified peer reached bundle hashing"); return nil }
	c := dialNative(t, b, frame{Type: "operator-hello", Version: 1, Token: testToken})
	if reply := c.receive(t); reply.Type != "operator-hello-rejected" || reply.Code != "OPERATOR_PEER_UNQUALIFIED" {
		t.Fatalf("preflight rejection: %+v", reply)
	}
	b.preflightPeer = func(net.Conn) error { return nil }
	b.verifyPeer = func(net.Conn) error { return nil }
	_ = readyNative(t, b)
}

func TestNativePinPendingFailsClosedWithoutBlocking(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("platform gate independently rejects non-Linux")
	}
	release := make(chan struct{})
	verifier := startNativePeerVerifier(func() (desktopAnchor, error) { <-release; return desktopAnchor{}, nil })
	defer close(release)
	b := New(nil, testToken, nil)
	b.pinStatus, b.verifyPeer = verifier.status, verifier.verify
	b.ready, b.epoch, b.liveUntil = true, "issuer", time.Now().Add(time.Minute)
	plan := planFor(domain.ConsentSettingsPermissions)
	if b.GetAuthorityReadiness([]domain.OperatorPurpose{plan.Purpose}, plan.Scope).AuthorityGo {
		t.Fatal("ready before background pin completed")
	}
	c := dialNative(t, b, frame{Type: "operator-hello", Version: 1, Token: testToken})
	if reply := c.receive(t); reply.Type != "operator-hello-rejected" || reply.Code != "OPERATOR_PIN_PENDING" {
		t.Fatalf("pending pin did not fail closed: %+v", reply)
	}
}
