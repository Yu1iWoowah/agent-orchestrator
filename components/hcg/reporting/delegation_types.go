package reporting

import "time"

// Fixed delegation DTOs keep the backend edge independent of internal types.

type DelegationProfileRef struct {
	ID       string `json:"id"`
	Revision string `json:"revision"`
}

type DelegationProjectPolicy struct {
	Enabled         *bool                  `json:"enabled,omitempty"`
	AllowedProfiles []DelegationProfileRef `json:"allowedProfiles,omitempty"`
	SelectedProfile *DelegationProfileRef  `json:"selectedProfile,omitempty"`
}

type DelegationPolicySnapshot struct {
	Project              DelegationProjectPolicy
	Worker, Orchestrator *bool
}

type DelegationCredentialReference struct {
	Provider string `json:"provider"`
	Name     string `json:"name"`
	Version  string `json:"version"`
}

type DelegationInputManifest struct {
	ProjectID string                   `json:"projectId"`
	Name      string                   `json:"name"`
	Revision  string                   `json:"revision"`
	Files     []DelegationApprovedFile `json:"files"`
}

type DelegationInstallation struct {
	Source          string `json:"source"`
	Venv            string `json:"venv"`
	Commit          string `json:"commit"`
	SourceDigest    string `json:"sourceDigest"`
	RuntimeDigest   string `json:"runtimeDigest"`
	PythonDigest    string `json:"pythonDigest"`
	ConfigDigest    string `json:"configDigest"`
	LockDigest      string `json:"lockDigest"`
	PythonVersion   string `json:"pythonVersion"`
	AdapterVersion  string `json:"adapterVersion"`
	TemplateVersion string `json:"templateVersion"`
}

type DelegationDeployment struct {
	Profile       DelegationProfile             `json:"profile"`
	CredentialRef DelegationCredentialReference `json:"credentialRef"`
	Workspace     string                        `json:"workspace"`
	Installation  DelegationInstallation        `json:"installation"`
	Inputs        []DelegationInputManifest     `json:"inputs"`
}

type DelegationRegistration struct {
	Grant         *DelegationLabGrant                `json:"grant"`
	Deployment    DelegationDeployment               `json:"deployment"`
	ExternalSpend map[string]DelegationExternalSpend `json:"externalSpend"`
}

type DelegationResolvedProfile struct {
	RunID           string `json:"runId"`
	OwnerRoot       string `json:"ownerRoot"`
	ID              string `json:"id"`
	Revision        string `json:"revision"`
	BillingProvider string `json:"billing_provider"`
	Model           string `json:"model"`
	Digest          string `json:"digest"`
	Available       bool   `json:"available"`
	Current         bool   `json:"current"`
	PriceVersion    string `json:"priceVersion"`
	Reason          string `json:"reason"`
}

type DelegationView struct {
	RunID                      string                     `json:"runId"`
	OwnerRoot                  string                     `json:"ownerRoot"`
	MountConfigured            bool                       `json:"mountConfigured"`
	ProjectID                  string                     `json:"projectId"`
	SessionID                  string                     `json:"sessionId"`
	Inputs                     Inputs                     `json:"inputs"`
	Source                     string                     `json:"source"`
	SessionOverride            string                     `json:"sessionOverride"`
	ProjectOptIn               bool                       `json:"projectOptIn"`
	EffectiveEnabled           bool                       `json:"effectiveEnabled"`
	EmergencyStop              bool                       `json:"emergencyStop"`
	Requested                  bool                       `json:"requested"`
	Active                     bool                       `json:"active"`
	Available                  bool                       `json:"available"`
	Reason                     string                     `json:"reason"`
	AllowedProfiles            []DelegationProfileRef     `json:"allowedProfiles"`
	SelectedProfile            *DelegationProfileRef      `json:"selectedProfile"`
	ResolvedProfile            *DelegationResolvedProfile `json:"resolvedProfile"`
	SelectionSource            string                     `json:"selectionSource"`
	ProjectPolicyRevision      string                     `json:"projectPolicyRevision"`
	HostProfileCurrentRevision string                     `json:"hostProfileCurrentRevision"`
	ProfileEligibility         string                     `json:"profileEligibility"`
	MountKnown                 bool                       `json:"mountKnown"`
	MountEnabled               bool                       `json:"mountEnabled"`
	MountProfile               *DelegationProfileRef      `json:"mountProfile"`
	ManifestRevision           uint64                     `json:"manifestRevision"`
}

type DelegationPrices struct {
	Version               string `json:"version"`
	Period                string `json:"period"`
	InputPerMillion       int64  `json:"inputNanoUSDPerMillion"`
	CachedInputPerMillion int64  `json:"cachedInputNanoUSDPerMillion"`
	OutputPerMillion      int64  `json:"outputNanoUSDPerMillion"`
	LimitsCertified       bool   `json:"limitsCertified"`
	BillableUsageCovered  bool   `json:"billableUsageCovered"`
}

type DelegationExternalSpend struct {
	Known           bool  `json:"known"`
	OutsideGateOnly bool  `json:"outsideGateOnly"`
	Nanos           int64 `json:"nanos"`
}

type DelegationProfile struct {
	Prices          DelegationPrices `json:"prices"`
	ID              string           `json:"id"`
	Revision        string           `json:"revision"`
	BillingProvider string           `json:"billingProvider"`
	Model           string           `json:"model"`
	ContextTokens   int64            `json:"contextTokens"`
	MaxOutputTokens int64            `json:"maxOutputTokens"`
	OutputTokens    int64            `json:"outputTokens"`
}

type DelegationSourceRange struct {
	ID       string `json:"id"`
	Path     string `json:"path"`
	Revision string `json:"revision"`
	SHA256   string `json:"sha256"`
	Start    int    `json:"startLine"`
	End      int    `json:"endLine"`
}

type DelegationOmission struct {
	SourceID string `json:"sourceId,omitempty"`
	Reason   string `json:"reason"`
}

type DelegationFinding struct {
	ID          string   `json:"id"`
	Kind        string   `json:"kind"`
	Text        string   `json:"text"`
	EvidenceIDs []string `json:"evidence_ids"`
}

type DelegationUsageReference struct {
	Status        string `json:"status"`
	AttemptID     string `json:"attempt_id"`
	RequestID     string `json:"request_id,omitempty"`
	ProfileDigest string `json:"profile_digest"`
}

type DelegationCoverage struct {
	RequestedScopeDigest string                  `json:"requested_scope_digest"`
	InjectedScopeDigest  string                  `json:"injected_scope_digest"`
	Status               string                  `json:"coverage_status"`
	Approved             []DelegationSourceRange `json:"approved_ranges"`
	Injected             []DelegationSourceRange `json:"injected_ranges"`
}

type DelegationReply struct {
	Schema          string                   `json:"schema"`
	DelegationID    string                   `json:"delegation_id"`
	Status          string                   `json:"status"`
	Answer          string                   `json:"answer"`
	Findings        []DelegationFinding      `json:"findings"`
	Evidence        []DelegationEvidence     `json:"evidence"`
	Omissions       []DelegationOmission     `json:"omissions"`
	Reason          string                   `json:"reason"`
	Backend         string                   `json:"backend"`
	Model           string                   `json:"model"`
	BillingProvider string                   `json:"billing_provider"`
	ProfileID       string                   `json:"profile_id"`
	ProfileRevision string                   `json:"profile_revision"`
	ProjectID       string                   `json:"project_id"`
	SessionID       string                   `json:"session_id"`
	UsageRef        DelegationUsageReference `json:"usage_ref"`
	Coverage        DelegationCoverage       `json:"coverage"`
}

type DelegationEvidence struct {
	ID            string    `json:"id"`
	SourceID      string    `json:"source_id"`
	Revision      string    `json:"revision"`
	ContentSHA256 string    `json:"content_sha256"`
	LineStart     int       `json:"line_start"`
	LineEnd       int       `json:"line_end"`
	Quote         string    `json:"quote"`
	QuoteSHA256   string    `json:"quote_sha256"`
	Path          string    `json:"path"`
	ObservedAt    time.Time `json:"observed_at"`
	Verification  string    `json:"verification"`
}

type DelegationApprovedFile struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}
type DelegationRequest struct {
	Question    string   `json:"question"`
	Paths       []string `json:"paths"`
	Focus       string   `json:"focus,omitempty"`
	OutputLimit int      `json:"output_limit,omitempty"`
}

type DelegationLabSession struct {
	ProjectID string `json:"projectId"`
	SessionID string `json:"sessionId"`
}

// DelegationLabGrant is operator-only registration data, never a model tool argument.
// One immutable run owns this root; only its explicit session list may grow.
type DelegationLabGrant struct {
	RunID               string                          `json:"runId"`
	OwnerRoot           string                          `json:"ownerRoot"`
	CanonicalOwner      DelegationLabSession            `json:"canonicalOwner"`
	StartsAt            time.Time                       `json:"startsAt"`
	ExpiresAt           time.Time                       `json:"expiresAt"`
	Profiles            map[string]DelegationProfileRef `json:"profiles"`
	Bindings            []DelegationLabSession          `json:"bindings"`
	ProviderCapNanos    map[string]int64                `json:"providerCapNanos"`
	TotalCapNanos       int64                           `json:"totalCapNanos"`
	AttemptsPerProvider int                             `json:"attemptsPerProvider"`
}
