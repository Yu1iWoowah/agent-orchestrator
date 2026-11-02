package store

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/pressly/goose/v3"
	_ "modernc.org/sqlite"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/gen"
)

var operatorTestTime = time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)

// These fixtures own only a restricted journal, not H/S product services.
// The real C0 migration is applied by goose. Full-schema coexistence and
// up/down/up are additionally tested in package sqlite.
func operatorFixture(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "c.db")
	s := openOperatorFixture(t, path)
	_, err := s.writeDB.Exec(`CREATE TABLE projects(id TEXT PRIMARY KEY);
 INSERT INTO projects VALUES('project-1');
 CREATE TABLE change_log(seq INTEGER PRIMARY KEY AUTOINCREMENT,project_id TEXT NOT NULL REFERENCES projects(id),event_type TEXT NOT NULL CHECK(event_type='session_updated'),payload TEXT NOT NULL CHECK(json_valid(payload)),created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP);
 CREATE TABLE fixture_reservations(action_key TEXT PRIMARY KEY, reservation_ref TEXT NOT NULL, purpose TEXT NOT NULL, plan_digest TEXT NOT NULL);`)
	if err != nil {
		t.Fatal(err)
	}
	migration, err := os.ReadFile("../migrations/20261101165111979_operator_consent_core.sql")
	if err != nil {
		t.Fatal(err)
	}
	provider, err := goose.NewProvider(goose.DialectSQLite3, s.writeDB, fstest.MapFS{"20261101165111979_operator_consent_core.sql": &fstest.MapFile{Data: migration}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = provider.Up(context.Background()); err != nil {
		t.Fatal(err)
	}
	return s, path
}

func openOperatorFixture(t *testing.T, path string) *Store {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path)+"?_pragma=journal_mode(WAL)&_pragma=foreign_keys(ON)&_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	read, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path)+"?_pragma=foreign_keys(ON)&_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	s := NewStore(db, read)
	s.operatorEpoch = "daemon-fixture"
	s.operatorIssuerEpoch = "issuer-fixture"
	s.operatorNow = func() time.Time { return operatorTestTime }
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func operatorPlan(purpose domain.OperatorPurpose) domain.OperatorActionPlan {
	d, _ := purpose.Domain()
	return domain.OperatorActionPlan{Protocol: domain.OperatorConsentProtocol, Purpose: purpose, Domain: d, ActionKey: "action-1", SubjectRef: "subject-1", ParticipantRefs: []string{"session-1"}, Scope: domain.OperatorActionScope{ProjectRef: "project-1", OwnerRef: "owner-1", BranchRef: "branch-1", NativeRef: "native-1"}, ExpectedRevisions: map[string]int64{"owner": 4, "recovery_epoch": 2}, PayloadDigest: strings.Repeat("a", 64), EffectDigest: strings.Repeat("b", 64), ValidUntil: operatorTestTime.Add(10 * time.Minute), RiskCodes: []string{"MANUAL_RECOVERY"}}
}

// Core contract fixtures exercise the production mint transaction. They do not
// supply native gesture or attach qualification evidence.
func mintOperatorFixture(t *testing.T, s *Store, plan domain.OperatorActionPlan) domain.OperatorConsentRef {
	t.Helper()
	receipt, err := s.GetOperatorAction(context.Background(), plan.ActionKey)
	if err != nil {
		t.Fatal(err)
	}
	_, digest, err := domain.CanonicalOperatorPlan(plan)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := s.NativeOperatorIssuer().ConfirmNativeOperatorPlan(context.Background(), receipt.RequestRef, digest, s.operatorIssuer())
	if err != nil {
		t.Fatal(err)
	}
	return ref
}

func reserveOperatorFixture(s *Store, ref domain.OperatorConsentRef, p domain.OperatorActionPlan, failAt string) (domain.OperatorConsumption, error) {
	ctx := context.Background()
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	var result domain.OperatorConsumption
	err := s.inTxDB(ctx, "fixture action", func(q *gen.Queries, raw gen.DBTX) error {
		return s.withOperatorConsent(q, func(part ports.OperatorConsentTransaction) error {
			var err error
			result, err = part.ConsumeInActionTransaction(ctx, ref, p, p.ActionKey, "reservation-1")
			if err != nil {
				return err
			}
			if failAt == "consume" {
				return errors.New("fixture domain eligibility changed")
			}
			if !result.Replay {
				if _, err = raw.ExecContext(ctx, `INSERT INTO fixture_reservations VALUES(?,?,?,?)`, p.ActionKey, result.ReservationRef, string(p.Purpose), result.PlanDigest); err != nil {
					return err
				}
			}
			if failAt == "reservation" {
				return errors.New("fixture reservation failed")
			}
			return nil
		})
	})
	return result, err
}

func TestOperatorConsentRestrictedConsumers(t *testing.T) {
	for _, purpose := range []domain.OperatorPurpose{domain.ConsentHandoffContext, domain.ConsentHandoffRetryRestore, domain.ConsentSettingsPermissions} {
		t.Run(string(purpose), func(t *testing.T) {
			s, _ := operatorFixture(t)
			p := operatorPlan(purpose)
			first, err := s.PrepareOperatorAction(context.Background(), p)
			if err != nil {
				t.Fatal(err)
			}
			second, err := s.PrepareOperatorAction(context.Background(), p)
			if err != nil || first != second {
				t.Fatalf("prepare replay: %+v %v", second, err)
			}
			ref := mintOperatorFixture(t, s, p)
			if again := mintOperatorFixture(t, s, p); again != ref {
				t.Fatal("duplicate confirm minted another consent")
			}
			for _, stage := range []string{"consume", "reservation"} {
				if _, err = reserveOperatorFixture(s, ref, p, stage); err == nil {
					t.Fatal("expected domain rollback")
				}
				assertOperatorCounts(t, s, 0, 0)
			}
			got, err := reserveOperatorFixture(s, ref, p, "")
			if err != nil || got.Replay {
				t.Fatalf("consume: %+v %v", got, err)
			}
			assertOperatorCounts(t, s, 1, 1)
			// Simulate response loss, duplicate Confirm and query under the original key.
			if again := mintOperatorFixture(t, s, p); again != ref {
				t.Fatal("confirmed consumed action again")
			}
			got, err = reserveOperatorFixture(s, ref, p, "")
			if err != nil || !got.Replay {
				t.Fatalf("replay: %+v %v", got, err)
			}
			assertOperatorCounts(t, s, 1, 1)
			receipt, err := s.GetOperatorAction(context.Background(), p.ActionKey)
			if err != nil || receipt.State != domain.OperatorActionConsumed || receipt.ReservationRef != "reservation-1" {
				t.Fatalf("receipt %+v %v", receipt, err)
			}
			if err = s.RevokeUnused(context.Background(), first.RequestRef); !errors.Is(err, domain.ErrOperatorActionConflict) {
				t.Fatalf("revoked accepted action: %v", err)
			}
		})
	}
}

func assertOperatorCounts(t *testing.T, s *Store, consumed, reservations int) {
	t.Helper()
	for query, want := range map[string]int{`SELECT count(*) FROM operator_consents WHERE consumed_at_ms IS NOT NULL`: consumed, `SELECT count(*) FROM fixture_reservations`: reservations} {
		var got int
		if err := s.readDB.QueryRow(query).Scan(&got); err != nil || got != want {
			t.Fatalf("%s = %d want %d: %v", query, got, want, err)
		}
	}
}

func TestOperatorConsentPlanIsolation(t *testing.T) {
	s, _ := operatorFixture(t)
	p := operatorPlan(domain.ConsentHandoffRetryRestore)
	if _, err := s.PrepareOperatorAction(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	ref := mintOperatorFixture(t, s, p)
	cases := map[string]func(*domain.OperatorActionPlan){
		"purpose": func(p *domain.OperatorActionPlan) { p.Purpose = domain.ConsentHandoffContext },
		"permission_restore": func(p *domain.OperatorActionPlan) {
			p.Purpose = domain.ConsentSettingsPermissions
			p.Domain = "settings"
		},
		"subject": func(p *domain.OperatorActionPlan) { p.SubjectRef = "other" },
		"project": func(p *domain.OperatorActionPlan) { p.Scope.ProjectRef = "other" },
		"owner":   func(p *domain.OperatorActionPlan) { p.Scope.OwnerRef = "other" },
		"native":  func(p *domain.OperatorActionPlan) { p.Scope.NativeRef = "other" },
		"branch":  func(p *domain.OperatorActionPlan) { p.Scope.BranchRef = "other" },
		"payload": func(p *domain.OperatorActionPlan) { p.PayloadDigest = strings.Repeat("c", 64) },
		"effect":  func(p *domain.OperatorActionPlan) { p.EffectDigest = strings.Repeat("c", 64) },
		"revision": func(p *domain.OperatorActionPlan) {
			p.ExpectedRevisions = map[string]int64{"owner": 5, "recovery_epoch": 2}
		},
		"expiry":         func(p *domain.OperatorActionPlan) { p.ValidUntil = p.ValidUntil.Add(time.Second) },
		"participant":    func(p *domain.OperatorActionPlan) { p.ParticipantRefs = []string{"session-2"} },
		"batch_addition": func(p *domain.OperatorActionPlan) { p.ParticipantRefs = []string{"session-1", "session-2"} },
		"risk":           func(p *domain.OperatorActionPlan) { p.RiskCodes = []string{"DIFFERENT_RISK"} },
		"key":            func(p *domain.OperatorActionPlan) { p.ActionKey = "other-key" },
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			changed := p
			change(&changed)
			if _, err := reserveOperatorFixture(s, ref, changed, ""); !errors.Is(err, domain.ErrOperatorActionConflict) {
				t.Fatalf("scope accepted: %v", err)
			}
			if _, err := s.PrepareOperatorAction(context.Background(), changed); name != "key" && !errors.Is(err, domain.ErrOperatorActionConflict) {
				t.Fatalf("same key changed plan: %v", err)
			}
			assertOperatorCounts(t, s, 0, 0)
		})
	}
}

func TestOperatorConsentExpiryRevokeAndRestart(t *testing.T) {
	for _, kind := range []string{"daemon", "issuer", "consent_expiry", "domain_expiry", "daemon_domain_expiry", "revoke"} {
		t.Run(kind, func(t *testing.T) {
			s, path := operatorFixture(t)
			p := operatorPlan(domain.ConsentHandoffRetryRestore)
			prepared, err := s.PrepareOperatorAction(context.Background(), p)
			if err != nil {
				t.Fatal(err)
			}
			ref := mintOperatorFixture(t, s, p)
			switch kind {
			case "daemon", "daemon_domain_expiry":
				if err = s.Close(); err != nil {
					t.Fatal(err)
				}
				s = openOperatorFixture(t, path)
				s.operatorEpoch = "new-daemon"
				if kind == "daemon_domain_expiry" {
					s.operatorNow = func() time.Time { return p.ValidUntil }
				}
			case "issuer":
				s.operatorIssuerEpoch = "new-issuer"
			case "consent_expiry":
				s.operatorNow = func() time.Time { return operatorTestTime.Add(5 * time.Minute) }
			case "domain_expiry":
				s.operatorNow = func() time.Time { return p.ValidUntil }
			case "revoke":
				if err = s.RevokeUnused(context.Background(), prepared.RequestRef); err != nil {
					t.Fatal(err)
				}
			}
			if _, err = reserveOperatorFixture(s, ref, p, ""); !errors.Is(err, domain.ErrOperatorConsentUnavailable) {
				t.Fatalf("old consent accepted %v", err)
			}
			assertOperatorCounts(t, s, 0, 0)
			got, err := s.GetOperatorAction(context.Background(), p.ActionKey)
			if err != nil {
				t.Fatal(err)
			}
			want := domain.OperatorActionStale
			if kind == "consent_expiry" || kind == "domain_expiry" || kind == "daemon_domain_expiry" {
				want = domain.OperatorActionExpired
			}
			if kind == "revoke" {
				want = domain.OperatorActionRevoked
			}
			if got.State != want {
				t.Fatalf("state %s want %s", got.State, want)
			}
			if kind == "daemon_domain_expiry" || kind == "domain_expiry" {
				retried, err := s.PrepareOperatorAction(context.Background(), p)
				if err != nil || retried.State != domain.OperatorActionExpired || retried.RequestRef != prepared.RequestRef {
					t.Fatalf("expired prepare: %+v %v", retried, err)
				}
			}
			if kind == "daemon" || kind == "issuer" || kind == "consent_expiry" {
				if _, err = s.PrepareOperatorAction(context.Background(), p); err != nil {
					t.Fatal(err)
				}
				fresh := mintOperatorFixture(t, s, p)
				if fresh == ref {
					t.Fatal("reused old epoch/expired consent")
				}
				if _, err = reserveOperatorFixture(s, fresh, p, ""); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestOperatorConsentAcceptedSurvivesRestartAndExpiry(t *testing.T) {
	s, path := operatorFixture(t)
	p := operatorPlan(domain.ConsentHandoffRetryRestore)
	if _, err := s.PrepareOperatorAction(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	ref := mintOperatorFixture(t, s, p)
	if _, err := reserveOperatorFixture(s, ref, p, ""); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = openOperatorFixture(t, path)
	s.operatorEpoch = "new-daemon"
	s.operatorIssuerEpoch = "new-issuer"
	s.operatorNow = func() time.Time { return operatorTestTime.Add(time.Hour) }
	receipt, err := s.PrepareOperatorAction(context.Background(), p)
	if err != nil || receipt.State != domain.OperatorActionConsumed {
		t.Fatalf("accepted key lost: %+v %v", receipt, err)
	}
	got, err := reserveOperatorFixture(s, ref, p, "")
	if err != nil || !got.Replay {
		t.Fatalf("accepted replay %+v %v", got, err)
	}
	changed := p
	changed.ExpectedRevisions = map[string]int64{"recovery_epoch": 3}
	if _, err = reserveOperatorFixture(s, ref, changed, ""); !errors.Is(err, domain.ErrOperatorActionConflict) {
		t.Fatalf("old proof unlocked a fresh recovery epoch: %v", err)
	}
	assertOperatorCounts(t, s, 1, 1)
}

func TestOperatorConsentConcurrentRetryAndPoisonedScope(t *testing.T) {
	s, _ := operatorFixture(t)
	p := operatorPlan(domain.ConsentSettingsBatch)
	p.ParticipantRefs = []string{"session-1", "session-2"}
	if _, err := s.PrepareOperatorAction(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	ref := mintOperatorFixture(t, s, p)
	s.writeMu.Lock()
	var escaped ports.OperatorConsentTransaction
	err := s.inTxDB(context.Background(), "poisoned fixture", func(q *gen.Queries, raw gen.DBTX) error {
		return s.withOperatorConsent(q, func(part ports.OperatorConsentTransaction) error {
			escaped = part
			_, _ = part.ConsumeInActionTransaction(context.Background(), "missing", p, p.ActionKey, "reservation-1")
			_, err := raw.ExecContext(context.Background(), `INSERT INTO fixture_reservations VALUES('action-1','reservation-1','settings.rebind_idle_batch','unused')`)
			return err
		})
	})
	s.writeMu.Unlock()
	if !errors.Is(err, domain.ErrOperatorConsentUnavailable) {
		t.Fatalf("swallowed error committed %v", err)
	}
	if _, err = escaped.ConsumeInActionTransaction(context.Background(), ref, p, p.ActionKey, "reservation-1"); !errors.Is(err, domain.ErrOperatorConsentUnavailable) {
		t.Fatal("escaped participant remained active")
	}
	assertOperatorCounts(t, s, 0, 0)
	var wg sync.WaitGroup
	errs := make(chan error, 12)
	fresh := make(chan bool, 12)
	for range 12 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := reserveOperatorFixture(s, ref, p, "")
			errs <- err
			fresh <- !got.Replay
		}()
	}
	wg.Wait()
	close(errs)
	close(fresh)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	count := 0
	for b := range fresh {
		if b {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("fresh executions %d", count)
	}
	assertOperatorCounts(t, s, 1, 1)
}

func TestOperatorConsentCrashBoundaries(t *testing.T) {
	for _, stage := range []string{"before_consume", "after_consume", "after_reservation", "before_commit", "after_commit"} {
		t.Run(stage, func(t *testing.T) {
			s, path := operatorFixture(t)
			p := operatorPlan(domain.ConsentSettingsPermissions)
			if _, err := s.PrepareOperatorAction(context.Background(), p); err != nil {
				t.Fatal(err)
			}
			ref := mintOperatorFixture(t, s, p)
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			binary, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(binary, "-test.run=^TestOperatorConsentCrashProcess$")
			cmd.Env = append(os.Environ(), "C0_CRASH_DB="+path, "C0_CRASH_STAGE="+stage)
			output, err := cmd.CombinedOutput()
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ExitCode() != 73 {
				t.Fatalf("crash child: %v %s", err, output)
			}
			s = openOperatorFixture(t, path)
			want := 0
			if stage == "after_commit" {
				want = 1
			}
			assertOperatorCounts(t, s, want, want)
			got, err := reserveOperatorFixture(s, ref, p, "")
			if err != nil || got.Replay != (want == 1) {
				t.Fatalf("retry %+v %v", got, err)
			}
			assertOperatorCounts(t, s, 1, 1)
		})
	}
}

func TestOperatorConsentCrashProcess(t *testing.T) {
	path, stage := os.Getenv("C0_CRASH_DB"), os.Getenv("C0_CRASH_STAGE")
	if path == "" {
		t.Skip("crash subprocess only")
	}
	s := openOperatorFixture(t, path)
	p := operatorPlan(domain.ConsentSettingsPermissions)
	row, err := s.qr.GetOperatorConfirmationByKey(context.Background(), p.ActionKey)
	if err != nil {
		t.Fatal(err)
	}
	consent, err := s.qr.GetLiveOperatorConsent(context.Background(), row.RequestRef)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := s.writeDB.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	crash := func(at string) {
		if stage == at {
			os.Exit(73)
		}
	}
	crash("before_consume")
	err = s.withOperatorConsent(s.qw.WithTx(tx), func(part ports.OperatorConsentTransaction) error {
		got, err := part.ConsumeInActionTransaction(context.Background(), domain.OperatorConsentRef(consent.ConsentRef), p, p.ActionKey, "reservation-1")
		if err != nil {
			return err
		}
		crash("after_consume")
		_, err = tx.Exec(`INSERT INTO fixture_reservations VALUES(?,?,?,?)`, p.ActionKey, got.ReservationRef, string(p.Purpose), got.PlanDigest)
		if err != nil {
			return err
		}
		crash("after_reservation")
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	crash("before_commit")
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	crash("after_commit")
	t.Fatal("unknown crash stage")
}

func TestOperatorConsentCDCHasOnlySafeReference(t *testing.T) {
	s, _ := operatorFixture(t)
	p := operatorPlan(domain.ConsentHandoffContext)
	prepared, err := s.PrepareOperatorAction(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	ref := mintOperatorFixture(t, s, p)
	if _, err = reserveOperatorFixture(s, ref, p, ""); err != nil {
		t.Fatal(err)
	}
	rows, err := s.readDB.Query(`SELECT payload FROM change_log`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	count := 0
	for rows.Next() {
		var payload string
		if err = rows.Scan(&payload); err != nil {
			t.Fatal(err)
		}
		if payload != `{"operatorRequestRef":"`+string(prepared.RequestRef)+`"}` {
			t.Fatalf("unsafe CDC %s", payload)
		}
		count++
	}
	if err = rows.Err(); err != nil {
		t.Fatal(err)
	}
	if count != 3 {
		t.Fatalf("events %d", count)
	}
}

func TestOperatorConsentReferencesAreNotAuthorityAndLifetimeIsCapped(t *testing.T) {
	for _, validFor := range []time.Duration{2 * time.Minute, 10 * time.Minute} {
		t.Run(validFor.String(), func(t *testing.T) {
			s, _ := operatorFixture(t)
			p := operatorPlan(domain.ConsentHandoffContext)
			p.ValidUntil = operatorTestTime.Add(validFor)
			receipt, err := s.PrepareOperatorAction(context.Background(), p)
			if err != nil {
				t.Fatal(err)
			}
			// A request reference alone is not a consent reference, even internally.
			if _, err = reserveOperatorFixture(s, domain.OperatorConsentRef(receipt.RequestRef), p, ""); !errors.Is(err, domain.ErrOperatorConsentUnavailable) {
				t.Fatalf("request ref granted authority: %v", err)
			}
			ref := mintOperatorFixture(t, s, p)
			row, err := s.qr.GetOperatorConsentByRef(context.Background(), string(ref))
			if err != nil {
				t.Fatal(err)
			}
			want := operatorTestTime.Add(min(validFor, domain.OperatorConsentLifetime)).UnixMilli()
			if row.ExpiresAtMs != want {
				t.Fatalf("expiry %d want %d", row.ExpiresAtMs, want)
			}
			s.operatorNow = func() time.Time { return time.UnixMilli(want) }
			if _, err = reserveOperatorFixture(s, ref, p, ""); !errors.Is(err, domain.ErrOperatorConsentUnavailable) {
				t.Fatalf("exact expiry accepted: %v", err)
			}
			assertOperatorCounts(t, s, 0, 0)
		})
	}
}

func TestOperatorConsentInvalidPlansAndCancellation(t *testing.T) {
	s, _ := operatorFixture(t)
	p := operatorPlan(domain.ConsentHandoffContext)
	for name, change := range map[string]func(*domain.OperatorActionPlan){
		"protocol":              func(p *domain.OperatorActionPlan) { p.Protocol = "operator-consent/2" },
		"domain":                func(p *domain.OperatorActionPlan) { p.Domain = "settings" },
		"optional":              func(p *domain.OperatorActionPlan) { p.Purpose = domain.ConsentCoordPolicy; p.Domain = "coord" },
		"wildcard":              func(p *domain.OperatorActionPlan) { p.Purpose = "handoff.*" },
		"body":                  func(p *domain.OperatorActionPlan) { p.SubjectRef = "raw body\nnot a ref" },
		"digest":                func(p *domain.OperatorActionPlan) { p.EffectDigest = "not a digest" },
		"revision":              func(p *domain.OperatorActionPlan) { p.ExpectedRevisions = map[string]int64{"owner": -1} },
		"unsafe_revision":       func(p *domain.OperatorActionPlan) { p.ExpectedRevisions = map[string]int64{"owner": 1 << 53} },
		"empty_revisions":       func(p *domain.OperatorActionPlan) { p.ExpectedRevisions = nil },
		"duplicate_participant": func(p *domain.OperatorActionPlan) { p.ParticipantRefs = []string{"one", "one"} },
		"risk_body":             func(p *domain.OperatorActionPlan) { p.RiskCodes = []string{"raw message body"} },
	} {
		t.Run(name, func(t *testing.T) {
			changed := p
			change(&changed)
			if _, err := s.PrepareOperatorAction(context.Background(), changed); !errors.Is(err, domain.ErrOperatorPlanInvalid) {
				t.Fatalf("invalid plan accepted: %v", err)
			}
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.PrepareOperatorAction(ctx, p); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled prepare: %v", err)
	}
	if _, err := s.GetOperatorAction(context.Background(), p.ActionKey); !errors.Is(err, domain.ErrOperatorActionNotFound) {
		t.Fatalf("cancelled prepare persisted: %v", err)
	}
}

func TestOperatorConsentSafeIntegerRevisionBoundary(t *testing.T) {
	s, _ := operatorFixture(t)
	plan := operatorPlan(domain.ConsentHandoffContext)
	plan.ExpectedRevisions = map[string]int64{"owner": 1<<53 - 1}
	prepared, err := s.PrepareOperatorAction(context.Background(), plan)
	if err != nil {
		t.Fatalf("maximum exactly displayable revision rejected: %v", err)
	}
	got, err := s.NativeOperatorIssuer().NativeOperatorPlan(context.Background(), prepared.RequestRef)
	if err != nil || got.ExpectedRevisions["owner"] != 1<<53-1 {
		t.Fatalf("maximum revision changed: %+v %v", got, err)
	}
}

func TestOperatorConsentNanosecondExpiryReplay(t *testing.T) {
	s, path := operatorFixture(t)
	plan := operatorPlan(domain.ConsentHandoffContext)
	plan.ValidUntil = plan.ValidUntil.Add(123456789 * time.Nanosecond)
	prepared, err := s.PrepareOperatorAction(context.Background(), plan)
	if err != nil {
		t.Fatal(err)
	}
	rebuilt := plan
	rebuilt.ValidUntil = prepared.ValidUntil
	t.Run("prepare_from_receipt", func(t *testing.T) {
		got, err := s.PrepareOperatorAction(context.Background(), rebuilt)
		if err != nil || got != prepared {
			t.Fatalf("millisecond prepare replay: %+v %v", got, err)
		}
	})
	ref := mintOperatorFixture(t, s, plan)
	accepted, err := reserveOperatorFixture(s, ref, plan, "")
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := s.GetOperatorAction(context.Background(), plan.ActionKey)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s = openOperatorFixture(t, path)
	s.operatorEpoch = "new-daemon"
	s.operatorIssuerEpoch = "new-issuer"
	s.operatorNow = func() time.Time { return operatorTestTime.Add(time.Hour) }
	t.Run("prepare_after_restart", func(t *testing.T) {
		got, err := s.PrepareOperatorAction(context.Background(), rebuilt)
		if err != nil || got != receipt {
			t.Fatalf("millisecond accepted replay: %+v %v", got, err)
		}
	})
	t.Run("consume_after_restart", func(t *testing.T) {
		got, err := reserveOperatorFixture(s, ref, rebuilt, "")
		if err != nil || !got.Replay || got.PlanDigest != accepted.PlanDigest || got.ReservationRef != accepted.ReservationRef {
			t.Fatalf("millisecond consume replay: %+v %v", got, err)
		}
	})
	assertOperatorCounts(t, s, 1, 1)
}

func TestOrdinaryStoreDoesNotExposeNativeMint(t *testing.T) {
	type nativeMinter interface {
		ConfirmNativeOperatorPlan(context.Context, domain.OperatorRequestRef, string, string) (domain.OperatorConsentRef, error)
	}
	s := &Store{}
	if _, ok := any(s).(nativeMinter); ok {
		t.Fatal("ordinary domain store exposes native mint")
	}
	if _, ok := any(s.NativeOperatorIssuer()).(nativeMinter); !ok {
		t.Fatal("bootstrap native adapter missing mint")
	}
}

func TestNativeReviewNextIncludesExpiredConsent(t *testing.T) {
	s, _ := operatorFixture(t)
	plan := operatorPlan(domain.ConsentSettingsPermissions)
	receipt, err := s.PrepareOperatorAction(context.Background(), plan)
	if err != nil {
		t.Fatal(err)
	}
	_ = mintOperatorFixture(t, s, plan)
	issuer := s.NativeOperatorIssuer()
	if _, err := issuer.NextNativeOperatorAction(context.Background(), ""); !errors.Is(err, domain.ErrOperatorActionNotFound) {
		t.Fatalf("live consent selected: %v", err)
	}
	s.operatorNow = func() time.Time { return operatorTestTime.Add(6 * time.Minute) }
	if got, err := issuer.NextNativeOperatorAction(context.Background(), ""); err != nil || got != receipt.RequestRef {
		t.Fatalf("expired consent hidden: %s %v", got, err)
	}
}
