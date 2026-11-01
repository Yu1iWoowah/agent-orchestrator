package operatornative

import (
	"bufio"
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"io"
	"net"
	"runtime"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

type nativeStore interface {
	SetNativeOperatorIssuer(context.Context, string) error
	NativeOperatorPlan(context.Context, domain.OperatorRequestRef) (domain.OperatorActionPlan, error)
	NextNativeOperatorAction(context.Context) (domain.OperatorRequestRef, error)
	ConfirmNativeOperatorPlan(context.Context, domain.OperatorRequestRef, string, string) (domain.OperatorConsentRef, error)
}

type frame struct {
	RequestID  string                     `json:"requestId,omitempty"`
	Type       string                     `json:"type"`
	Version    int                        `json:"version,omitempty"`
	Token      string                     `json:"token,omitempty"`
	RequestRef domain.OperatorRequestRef  `json:"requestRef,omitempty"`
	Challenge  string                     `json:"challenge,omitempty"`
	Digest     string                     `json:"digest,omitempty"`
	Plan       *domain.OperatorActionPlan `json:"plan,omitempty"`
	ConsentRef domain.OperatorConsentRef  `json:"consentRef,omitempty"`
	Code       string                     `json:"code,omitempty"`
}

type challenge struct {
	value   string
	digest  string
	expires time.Time
}

// Broker serializes the one native issuer with epoch changes and mint. No
// private credential, challenge or dialog result is reachable through HTTP.
type Broker struct {
	mu         sync.Mutex
	store      nativeStore
	token      string
	conn       net.Conn
	epoch      string
	ready      bool
	liveUntil  time.Time
	challenges map[domain.OperatorRequestRef]challenge
	now        func() time.Time
	verifyPeer func(net.Conn) error
	// Set once at construction. Other platforms need real native receipts
	// before their issuer can be enabled; code pinning alone is insufficient.
	platformVerified bool
}

// New binds the native store to a private owned token and pinned attach code.
func New(store nativeStore, ownedToken string) *Broker {
	return &Broker{store: store, token: ownedToken, now: time.Now, challenges: map[domain.OperatorRequestRef]challenge{}, verifyPeer: newNativePeerVerifier(), platformVerified: runtime.GOOS == "linux"}
}

func readFrame(raw []byte) (frame, error) {
	var f frame
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(&f); err != nil {
		return f, err
	}
	if d.Decode(new(any)) != io.EOF {
		return f, errUnqualified
	}
	return f, nil
}

func send(conn net.Conn, out frame) error {
	if err := conn.SetWriteDeadline(time.Now().Add(time.Second)); err != nil {
		return err
	}
	return json.NewEncoder(conn).Encode(out)
}

// Handle is installed on the existing Browser IPC listener, before it starts.
// This connection never joins the Browser broker or its session command path.
func (b *Broker) Handle(ctx context.Context, conn net.Conn, scan *bufio.Scanner, raw []byte) {
	defer func() { _ = conn.Close() }()
	if !b.platformVerified {
		return
	}
	hello, err := readFrame(raw)
	if err != nil || hello.Type != "operator-hello" || hello.Version != 1 {
		return
	}
	// The stdin marker is a transport instruction, not native identity. Even an
	// owned token chosen by a CLI bootstrap cannot qualify a shell as issuer.
	if b.verifyPeer(conn) != nil {
		return
	}
	if hello.Token != "" {
		if len(b.token) < 32 || subtle.ConstantTimeCompare([]byte(hello.Token), []byte(b.token)) != 1 {
			return
		}
	}
	b.mu.Lock()
	if b.conn != nil {
		b.mu.Unlock()
		return
	}
	epoch := uuid.NewString()
	b.conn, b.epoch = conn, epoch
	b.ready = false
	b.challenges = map[domain.OperatorRequestRef]challenge{}
	b.mu.Unlock()
	defer func() {
		b.mu.Lock()
		b.ready = false
		b.epoch = ""
		b.challenges = map[domain.OperatorRequestRef]challenge{}
		b.mu.Unlock()
		_ = b.store.SetNativeOperatorIssuer(context.Background(), "")
		b.mu.Lock()
		b.conn = nil
		b.mu.Unlock()
	}()
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	if b.store.SetNativeOperatorIssuer(ctx, epoch) != nil {
		return
	}
	if send(conn, frame{Type: "operator-hello-ok", Version: 1}) != nil {
		return
	}
	for {
		_ = conn.SetReadDeadline(b.now().Add(15 * time.Second))
		if !scan.Scan() {
			return
		}
		// C0 plans and frames are substantially smaller than Browser snapshots.
		if len(scan.Bytes()) > 65536 {
			return
		}
		in, err := readFrame(scan.Bytes())
		if err != nil {
			return
		}
		out := b.handle(ctx, in)
		out.RequestID = in.RequestID
		if send(conn, out) != nil {
			return
		}
	}
}

func (b *Broker) handle(ctx context.Context, in frame) frame {
	b.mu.Lock()
	if in.Type == "ready" || in.Type == "ping" {
		if in.Type == "ready" {
			b.ready = true
		}
		b.liveUntil = b.now().Add(15 * time.Second)
		b.mu.Unlock()
		return frame{Type: "pong"}
	}
	if !b.ready || !b.now().Before(b.liveUntil) {
		b.mu.Unlock()
		return frame{Type: "error", Code: "OPERATOR_ISSUER_UNAVAILABLE"}
	}
	switch in.Type {
	case "review-next", "review":
		epoch := b.epoch
		b.mu.Unlock()
		ref := in.RequestRef
		if in.Type == "review-next" {
			var err error
			ref, err = b.store.NextNativeOperatorAction(ctx)
			if err != nil {
				return frame{Type: "error", Code: "OPERATOR_ACTION_UNAVAILABLE"}
			}
		}
		plan, err := b.store.NativeOperatorPlan(ctx, ref)
		if err != nil {
			return frame{Type: "error", Code: "OPERATOR_ACTION_UNAVAILABLE"}
		}
		_, digest, err := domain.CanonicalOperatorPlan(plan)
		if err != nil {
			return frame{Type: "error", Code: "OPERATOR_ACTION_UNAVAILABLE"}
		}
		b.mu.Lock()
		defer b.mu.Unlock()
		for key, c := range b.challenges {
			if !b.now().Before(c.expires) {
				delete(b.challenges, key)
			}
		}
		if epoch != b.epoch || !b.ready || !b.now().Before(b.liveUntil) || len(b.challenges) >= 128 {
			return frame{Type: "error", Code: "OPERATOR_ISSUER_UNAVAILABLE"}
		}
		c := challenge{value: uuid.NewString(), digest: digest, expires: minTime(plan.ValidUntil, b.now().Add(domain.OperatorConsentLifetime))}
		b.challenges[ref] = c
		return frame{Type: "review-plan", RequestRef: ref, Challenge: c.value, Digest: digest, Plan: &plan}
	case "confirm":
		c, ok := b.challenges[in.RequestRef]
		epoch := b.epoch
		b.mu.Unlock()
		if !ok || c.digest != in.Digest || !b.now().Before(c.expires) || subtle.ConstantTimeCompare([]byte(c.value), []byte(in.Challenge)) != 1 {
			return frame{Type: "error", Code: "OPERATOR_CONFIRMATION_INVALID"}
		}
		ref, err := b.store.ConfirmNativeOperatorPlan(ctx, in.RequestRef, c.digest, epoch)
		if err != nil {
			return frame{Type: "error", Code: "OPERATOR_CONFIRMATION_INVALID"}
		}
		return frame{Type: "confirmed", RequestRef: in.RequestRef, ConsentRef: ref}
	case "cancel":
		delete(b.challenges, in.RequestRef)
		b.mu.Unlock()
		return frame{Type: "cancelled"}
	default:
		b.mu.Unlock()
		return frame{Type: "error", Code: "OPERATOR_PROTOCOL_INVALID"}
	}
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

// GetAuthorityReadiness qualifies only an explicit closed purpose set and full
// scope while the native issuer is live. It never authorizes the future action.
func (b *Broker) GetAuthorityReadiness(required []domain.OperatorPurpose, scope domain.OperatorActionScope) domain.OperatorAuthorityReadiness {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := domain.OperatorAuthorityReadiness{Protocol: domain.OperatorConsentProtocol, RegistryVersion: domain.OperatorPurposeRegistryVersion, CoreDelivered: true, NativeIssuerDelivered: true, PurposeSet: []domain.OperatorPurpose{}, Reason: "native_issuer_unavailable"}
	if !b.platformVerified {
		out.NativeIssuerDelivered = false
		out.Reason = "native_platform_unverified"
		return out
	}
	if !b.ready || b.epoch == "" || !b.now().Before(b.liveUntil) {
		return out
	}
	out.IssuerEpoch = b.epoch
	expiry := b.liveUntil
	out.ExpiresAt = &expiry
	out.QualificationRef = b.epoch
	if len(required) == 0 || len(required) > 9 || !domain.OperatorReferenceValid(scope.ProjectRef) || !domain.OperatorReferenceValid(scope.OwnerRef) || !domain.OperatorReferenceValid(scope.BranchRef) || !domain.OperatorReferenceValid(scope.NativeRef) {
		out.Reason = "purpose_and_scope_required"
		return out
	}
	for _, purpose := range required {
		if _, active := purpose.Domain(); !active {
			out.Reason = "purpose_not_qualified"
			return out
		}
	}
	out.PurposeSet = append([]domain.OperatorPurpose{}, required...)
	out.AuthorityGo, out.Reason = true, "native_issuer_qualified"
	return out
}
