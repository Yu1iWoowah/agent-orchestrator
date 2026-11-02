package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// OperatorConsentProtocol identifies the fixed C0 foundation contract.
const OperatorConsentProtocol = "operator-consent/1"

// OperatorPurposeRegistryVersion versions the closed purpose registry.
const OperatorPurposeRegistryVersion = 1

// OperatorConsentLifetime is the maximum lifetime after native mint.
const OperatorConsentLifetime = 5 * time.Minute

// OperatorPurpose identifies one separately reviewed human decision.
type OperatorPurpose string

// Registered purposes. Coord policy remains disabled until its consumer ships.
const (
	ConsentHandoffContext      OperatorPurpose = "handoff.required_context"
	ConsentHandoffRetryRestore OperatorPurpose = "handoff.retry_source_restore"
	ConsentHandoffPromote      OperatorPurpose = "handoff.recover_promote_verified_candidate"
	ConsentHandoffAbandon      OperatorPurpose = "handoff.abandon_source_stopped"
	ConsentSettingsRebind      OperatorPurpose = "settings.rebind"
	ConsentSettingsBatch       OperatorPurpose = "settings.rebind_idle_batch"
	ConsentSettingsPermissions OperatorPurpose = "settings.restore_permissions"
	ConsentDeliveryDefer       OperatorPurpose = "delivery.unknown.defer"
	ConsentDeliveryResend      OperatorPurpose = "delivery.linked_resend"
	ConsentCoordPolicy         OperatorPurpose = "coord.policy.update"
)

// Domain returns the closed registry's domain. Optional consumers stay disabled.
func (p OperatorPurpose) Domain() (string, bool) {
	switch p {
	case ConsentHandoffContext, ConsentHandoffRetryRestore, ConsentHandoffPromote, ConsentHandoffAbandon:
		return "handoff", true
	case ConsentSettingsRebind, ConsentSettingsBatch, ConsentSettingsPermissions:
		return "settings", true
	case ConsentDeliveryDefer, ConsentDeliveryResend:
		return "delivery", true
	default:
		return "", false
	}
}

// Operator confirmation boundary errors contain no input or authority material.
var (
	ErrOperatorPlanInvalid        = errors.New("invalid operator action plan")
	ErrOperatorActionConflict     = errors.New("operator action conflicts with fixed plan")
	ErrOperatorConsentUnavailable = errors.New("operator consent unavailable")
	ErrOperatorActionNotFound     = errors.New("operator action not found")
)

// OperatorRequestRef is a safe lookup reference, never a mint credential.
type OperatorRequestRef string

// OperatorConsentRef identifies a purpose-bound record on the native-only path.
type OperatorConsentRef string

// OperatorActionScope binds domain-owned qualifiers; these are references, never
// permission rules, message bodies, credentials or provider responses.
type OperatorActionScope struct {
	ProjectRef string `json:"projectRef"`
	OwnerRef   string `json:"ownerRef"`
	BranchRef  string `json:"branchRef"`
	NativeRef  string `json:"nativeRef"`
}

// OperatorActionPlan is constructed after the domain's side-effect-free admission.
// Participant order is significant, including the fixed batch list. RiskCodes
// are bounded machine codes for the native UI, not arbitrary explanatory text.
type OperatorActionPlan struct {
	Protocol          string              `json:"protocol"`
	Purpose           OperatorPurpose     `json:"purpose"`
	Domain            string              `json:"domain"`
	ActionKey         string              `json:"actionKey"`
	SubjectRef        string              `json:"subjectRef"`
	ParticipantRefs   []string            `json:"participantRefs"`
	Scope             OperatorActionScope `json:"scope"`
	ExpectedRevisions map[string]int64    `json:"expectedRevisions"`
	PayloadDigest     string              `json:"payloadDigest"`
	EffectDigest      string              `json:"effectDigest"`
	ValidUntil        time.Time           `json:"validUntil"`
	RiskCodes         []string            `json:"riskCodes"`
}

// OperatorReferenceValid accepts only bounded identifiers. Public APIs never
// accept plans; admission and the meaning of each reference belong to the domain.
func OperatorReferenceValid(s string) bool {
	if s == "" || len(s) > 160 {
		return false
	}
	for _, c := range s {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', strings.ContainsRune("._:/@+-", c):
		default:
			return false
		}
	}
	return true
}

// CanonicalOperatorPlan binds every field, including purpose, expiry and scope.
// encoding/json sorts map keys; UTC and millisecond precision match the durable
// expiry representation so a reconstructed plan retains its digest.
func CanonicalOperatorPlan(p OperatorActionPlan) (string, string, error) {
	d, active := p.Purpose.Domain()
	if !active || d != p.Domain || p.Protocol != OperatorConsentProtocol ||
		!OperatorReferenceValid(p.ActionKey) || !OperatorReferenceValid(p.SubjectRef) ||
		!OperatorReferenceValid(p.Scope.ProjectRef) || !OperatorReferenceValid(p.Scope.OwnerRef) ||
		!OperatorReferenceValid(p.Scope.BranchRef) || !OperatorReferenceValid(p.Scope.NativeRef) ||
		p.ValidUntil.IsZero() || len(p.ParticipantRefs) == 0 || len(p.ParticipantRefs) > 64 ||
		len(p.ExpectedRevisions) == 0 || len(p.ExpectedRevisions) > 64 || len(p.RiskCodes) == 0 || len(p.RiskCodes) > 16 {
		return "", "", ErrOperatorPlanInvalid
	}
	for _, digest := range []string{p.PayloadDigest, p.EffectDigest} {
		decoded, err := hex.DecodeString(digest)
		if err != nil || len(decoded) != sha256.Size || digest != strings.ToLower(digest) {
			return "", "", ErrOperatorPlanInvalid
		}
	}
	seen := map[string]bool{}
	for _, ref := range p.ParticipantRefs {
		if !OperatorReferenceValid(ref) || seen[ref] {
			return "", "", ErrOperatorPlanInvalid
		}
		seen[ref] = true
	}
	for ref, rev := range p.ExpectedRevisions {
		// The native Electron client must display the exact JSON revision.
		if !OperatorReferenceValid(ref) || rev < 0 || rev >= 1<<53 {
			return "", "", ErrOperatorPlanInvalid
		}
	}
	for _, risk := range p.RiskCodes {
		if risk == "" || len(risk) > 64 {
			return "", "", ErrOperatorPlanInvalid
		}
		for _, c := range risk {
			if c != '_' && (c < 'A' || c > 'Z') {
				return "", "", ErrOperatorPlanInvalid
			}
		}
	}
	p.ValidUntil = p.ValidUntil.UTC().Truncate(time.Millisecond)
	b, err := json.Marshal(p)
	if err != nil {
		return "", "", ErrOperatorPlanInvalid
	}
	hash := sha256.Sum256(b)
	return string(b), hex.EncodeToString(hash[:]), nil
}

// OperatorActionState is derived from durable request/consent facts and epochs.
type OperatorActionState string

// Operator action states do not describe provider execution outcomes.
const (
	OperatorActionPrepared  OperatorActionState = "prepared"
	OperatorActionConfirmed OperatorActionState = "confirmed"
	OperatorActionConsumed  OperatorActionState = "consumed"
	OperatorActionRevoked   OperatorActionState = "revoked"
	OperatorActionExpired   OperatorActionState = "expired"
	OperatorActionStale     OperatorActionState = "stale"
)

// OperatorActionReceipt is safe to display. Consent references and challenges
// are deliberately absent. The reservation remains owned by the domain journal.
type OperatorActionReceipt struct {
	RequestRef     OperatorRequestRef
	ActionKey      string
	Purpose        OperatorPurpose
	State          OperatorActionState
	ValidUntil     time.Time
	ReservationRef string
}

// OperatorConsumption links the consumed plan to its domain-owned reservation.
type OperatorConsumption struct {
	RequestRef     OperatorRequestRef
	ActionKey      string
	PlanDigest     string
	ReservationRef string
	Replay         bool
}

// OperatorAuthorityReadiness separates authorization from actual delivery.
// PR1 never has a qualified native issuer or a usable mint entry point.
type OperatorAuthorityReadiness struct {
	Protocol              string            `json:"protocol"`
	RegistryVersion       int               `json:"registryVersion"`
	CoreDelivered         bool              `json:"coreDelivered"`
	NativeIssuerDelivered bool              `json:"nativeIssuerDelivered"`
	AuthorityGo           bool              `json:"authorityGo"`
	PurposeSet            []OperatorPurpose `json:"purposeSet"`
	IssuerEpoch           string            `json:"issuerEpoch"`
	ExpiresAt             *time.Time        `json:"expiresAt"`
	QualificationRef      string            `json:"qualificationRef"`
	Reason                string            `json:"reason"`
}

// GetAuthorityReadiness fails closed independently of optional domain consumers.
func GetAuthorityReadiness(_ []OperatorPurpose, _ OperatorActionScope) OperatorAuthorityReadiness {
	return OperatorAuthorityReadiness{Protocol: OperatorConsentProtocol, RegistryVersion: OperatorPurposeRegistryVersion,
		CoreDelivered: true, AuthorityGo: false, PurposeSet: []OperatorPurpose{}, Reason: "native_issuer_not_delivered"}
}
