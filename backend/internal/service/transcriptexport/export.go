// Package transcriptexport verifies a fixed source inventory without controlling
// providers. Replacement intake fencing and retirement belong to its caller.
package transcriptexport

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// Scope includes all required epochs of the source AO session main line.
const Scope = "source-ao-session-main-line/all-required-claude-epochs"

// EpochCutoff fixes a native byte boundary and a settled main-line leaf.
type EpochCutoff = domain.TranscriptEpochCutoff

// Manifest records coverage facts without repeating the transcript payload.
type Manifest struct {
	EpochOrder            string                          `json:"epochOrder"`
	ConversationCutoffs   map[string]int64                `json:"conversationCutoffs"`
	Version               string                          `json:"version"`
	ProjectID             domain.ProjectID                `json:"projectId"`
	SourceID              domain.SessionID                `json:"sourceSessionId"`
	Mode                  domain.SessionMode              `json:"mode"`
	Harness               domain.AgentHarness             `json:"harness"`
	Owner                 domain.SessionControllerOwner   `json:"owner"`
	SnapshotFingerprint   string                          `json:"snapshotFingerprint"`
	CutoffSequence        int64                           `json:"aoCutoffSequence"`
	Scope                 string                          `json:"scope"`
	Coverage              string                          `json:"coverage"`
	ExcludedNativeSources []SourceExclusion               `json:"excludedNativeSources"`
	ChildPolicy           string                          `json:"childPolicy"`
	ExcludedChildTurns    int                             `json:"excludedChildTurns"`
	AOOnlyMessages        int                             `json:"aoOnlyMessages"`
	AOOnlyAttachments     int                             `json:"aoOnlyAttachments"`
	AOSupplements         []AOSupplement                  `json:"aoSupplements"`
	Steers                []SteerCoverage                 `json:"steers"`
	CoverageLinks         []CoverageLink                  `json:"coverageLinks"`
	CoveredTurns          []string                        `json:"coveredTurns"`
	UnknownRecords        int                             `json:"unknownRecords"`
	ParseFailures         int                             `json:"parseFailures"`
	MissingEpochs         int                             `json:"missingEpochs"`
	TimeStart             string                          `json:"timeStart"`
	TimeEnd               string                          `json:"timeEnd"`
	Segments              []ports.NativeTranscriptSegment `json:"segments"`
	Omissions             []string                        `json:"omissions"`
}

// Output seals verified transcript bytes until frozen evidence publication.
type Output struct {
	seal       [32]byte
	revalidate func(context.Context) (NativeStability, error)
	Manifest   Manifest
	Transcript []byte
}

func incomplete(code, reason string) error {
	return &ports.NativeTranscriptError{Code: code, Reason: reason}
}

// Build accepts boundaries from the coordinator, but derives source paths and
// the required native inventory exclusively from durable, session-owned facts.
func Build(ctx context.Context, s domain.TranscriptSourceSnapshot, cuts []EpochCutoff, reader ports.NativeTranscriptReader) (Output, error) {
	var out Output
	if s.Session.Harness != domain.HarnessClaudeCode {
		return out, incomplete("ORCHESTRATOR_HANDOFF_SOURCE_UNSUPPORTED", "only Claude Code source is supported")
	}
	if s.Session.Kind != domain.KindOrchestrator || s.Session.IsTerminated || s.Fingerprint == "" || reader == nil {
		return out, incomplete("TRANSCRIPT_SOURCE_INVALID", "a live owned source snapshot is required")
	}
	epochs, err := inventory(s)
	if err != nil {
		return out, err
	}
	if len(epochs) == 0 || len(epochs) != len(cuts) || len(epochs) > 64 {
		return out, incomplete("TRANSCRIPT_EPOCHS_INCOMPLETE", "required native epoch boundaries are missing or exceed capacity")
	}
	byCut := map[string]EpochCutoff{}
	var total int64
	for _, c := range cuts {
		if _, ok := epochs[c.NativeID]; !ok || byCut[c.NativeID].NativeID != "" || c.Bytes <= 0 || c.LeafID == "" {
			return out, incomplete("TRANSCRIPT_CUTOFF_INVALID", "unknown, duplicate, or empty native boundary")
		}
		byCut[c.NativeID] = c
		total += c.Bytes
		if total > ports.MaxTranscriptSourceBytes {
			return out, incomplete("TRANSCRIPT_LIMIT_EXCEEDED", "combined source exceeds export capacity")
		}
	}
	m := Manifest{EpochOrder: "verified dependency order; independent epochs sorted by identity, not inferred time", ConversationCutoffs: s.ConversationCutoffs, Version: "ao-handoff-transcript/1", ProjectID: s.Session.ProjectID, SourceID: s.Session.ID, Mode: domain.NormalizeSessionMode(s.Session.Mode), Harness: s.Session.Harness, Owner: s.Session.ControllerOwner(), SnapshotFingerprint: s.Fingerprint, CutoffSequence: s.CutoffSequence, Scope: Scope, Coverage: "complete_for_scope", ChildPolicy: "exclude-and-count/unknown-fails-closed", TimeStart: "unknown", TimeEnd: "unknown", CoveredTurns: []string{}, Omissions: []string{"full tool-result output omitted; counted per result block", "thinking and binary attachments represented by type and source reference"}}
	segments, verifiedRequests, err := readEpochs(ctx, epochs, byCut, reader)
	if err != nil {
		return out, err
	}
	if err := verifyObservations(s, segments); err != nil {
		return out, err
	}
	var firstTime, lastTime time.Time
	var body strings.Builder
	fmt.Fprintf(&body, "# AO source conversation transcript\n\nProject: %s\nSource session: %s\nHarness: %s\nMode: %s\nScope: %s\nCoverage: %s\nAO cutoff: %d\nSnapshot: %s\n\n", inlineMetadata(string(m.ProjectID)), inlineMetadata(string(m.SourceID)), m.Harness, m.Mode, m.Scope, m.Coverage, m.CutoffSequence, inlineMetadata(m.SnapshotFingerprint))
	childIdentities := map[string]map[string]bool{}
	nativePrompts := map[string][]nativePrompt{}
	nativeIdentities := map[string]map[string]bool{}
	for _, seg := range segments {
		id := seg.NativeID
		c := byCut[id]
		if c.SHA256 != "" && c.SHA256 != seg.SHA256 {
			return out, incomplete("TRANSCRIPT_SOURCE_CHANGED", "captured native content changed before export")
		}
		if seg.ByteCutoff != c.Bytes || seg.LeafID != c.LeafID || seg.SHA256 == "" || len(seg.MainRecordIDs) == 0 && seg.Counts.AncestorRecords == 0 {
			return out, incomplete("TRANSCRIPT_COVERAGE_INCOMPLETE", "reader did not verify the fixed native boundary")
		}
		fmt.Fprintf(&body, "## Native epoch %s\n\nSource SHA-256: %s\nByte cutoff: %d\nLeaf: %s\nCounts: main=%d child=%d metadata=%d duplicate=%d tools=%d results=%d omitted-results=%d attachments=%d\n\n", inlineMetadata(id), inlineMetadata(seg.SHA256), seg.ByteCutoff, inlineMetadata(seg.LeafID), seg.Counts.MainRecords, seg.Counts.ChildRecords, seg.Counts.MetadataRecords, seg.Counts.Duplicates, seg.Counts.ToolCalls, seg.Counts.ToolResults, seg.Counts.OmittedToolResults, seg.Counts.Attachments)
		nativePrompts[id] = promptInventory(seg)
		records := map[string]ports.NativeTranscriptRecord{}
		nativeIdentities[id] = map[string]bool{}
		childIdentities[id] = map[string]bool{}
		for _, r := range seg.Records {
			if r.Provenance == domain.NativeThreadChild {
				if r.ID != "" {
					childIdentities[id][r.ID] = true
				}
				if r.PromptID != "" {
					childIdentities[id][r.PromptID] = true
				}
			}
			if r.Excluded != "" {
				continue
			}
			if r.Provenance != domain.NativeThreadMain || r.NativeID != id {
				return out, incomplete("TRANSCRIPT_PROVENANCE_UNKNOWN", "reader returned an unclassified main-line record")
			}
			records[r.ID] = r
			nativeIdentities[id][r.ID] = true
			if r.PromptID != "" {
				nativeIdentities[id][r.PromptID] = true
			}
		}
		for _, rid := range seg.MainRecordIDs {
			r, ok := records[rid]
			if !ok {
				return out, incomplete("TRANSCRIPT_COVERAGE_INCOMPLETE", "reader ancestry has an absent record")
			}
			fmt.Fprintf(&body, "### %s / %s\n\nNative record: %s; parent: %s; prompt: %s; time: %s\n\n", inlineMetadata(r.Type), inlineMetadata(id), inlineMetadata(r.ID), inlineMetadata(r.ParentID), inlineMetadata(r.PromptID), inlineMetadata(displayTime(r.Timestamp)))
			if stamp, e := time.Parse(time.RFC3339Nano, r.Timestamp); e == nil {
				if firstTime.IsZero() || stamp.Before(firstTime) {
					firstTime = stamp
					m.TimeStart = stamp.UTC().Format(time.RFC3339Nano)
				}
				if lastTime.IsZero() || stamp.After(lastTime) {
					lastTime = stamp
					m.TimeEnd = stamp.UTC().Format(time.RFC3339Nano)
				}
			}

			for _, b := range r.Blocks {
				fmt.Fprintf(&body, "%s", renderBlock(b))
			}
		}
		// The manifest is an identity/provenance ledger; message payload occurs once
		// in transcript.md, not again in a potentially much larger JSON receipt.
		seg.Records = append([]ports.NativeTranscriptRecord(nil), seg.Records...)
		seg.CheckStability = nil
		for i := range seg.Records {
			seg.Records[i].Blocks = nil
		}
		m.Segments = append(m.Segments, seg)
	}
	branches := map[string]domain.ConversationBranch{}
	for _, b := range s.Branches {
		branches[b.ID] = b
	}
	inputs, members, e := batchInputs(s)
	if e != nil {
		return out, e
	}
	details, e := steerDetails(s)
	if e != nil {
		return out, e
	}
	childDispatch := map[string]bool{}
	initialCaptured := map[string]bool{}
	index := domain.IndexNativeThreadEvidence(s.NativeThreads)
	aoOnly := map[string]domain.TurnState{}
	for _, turn := range s.Turns {
		if members[turn.ID] != "" {
			continue
		}
		if input, ok := inputs[turn.ID]; ok {
			turn.ProviderInputText = input.ProviderInputText
		}
		// Legacy first-batch steers were stored on the active turn. Their receipt
		// identifies that this field is guidance, not the original prompt.
		for _, activity := range s.Activities {
			detail, ok := details[activity.ID]
			if ok && activity.TurnID == turn.ID && detail.StoredOnTurn && detail.ProviderInputText == turn.ProviderInputText {
				turn.ProviderInputText = ""
			}
		}

		if turn.ProviderTurnID == "" && (turn.State == domain.TurnStateQueued || turn.State.Terminal()) {
			aoOnly[turn.ID] = turn.State
			continue
		}
		if !turn.State.Terminal() {
			return out, incomplete("TRANSCRIPT_TAIL_UNSETTLED", "AO source still has an active turn")
		}
		root := branches[turn.BranchID].ProviderConversationID
		provenance := index.Classify(turn.BranchID, turn.ProviderTurnID, root)
		owner := index[domain.NativeThreadTurnKey{BranchID: turn.BranchID, ProviderTurnID: turn.ProviderTurnID}]
		if provenance == domain.NativeThreadChild {
			if _, ownedEpoch := epochs[owner]; ownedEpoch {
				root = owner
				provenance = domain.NativeThreadMain
			} else if !confirmedChild(s, root, owner) {
				provenance = domain.NativeThreadUnknown
			}
		}
		if provenance == domain.NativeThreadMain && childIdentities[root][turn.ProviderTurnID] {
			return out, incomplete("TRANSCRIPT_PROVENANCE_UNKNOWN", "durable main ownership conflicts with native child identity")
		}
		switch provenance {
		case domain.NativeThreadChild:
			m.ExcludedChildTurns++
			childDispatch[turn.ID] = true
			continue
		case domain.NativeThreadUnknown:
			return out, incomplete("TRANSCRIPT_PROVENANCE_UNKNOWN", "AO turn lacks unambiguous native-thread ownership")
		}

		initialCaptured[turn.ID] = turn.ProviderInputText != ""
		link, e := correlateTurn(turn, s.Messages, nativeIdentities[root], nativePrompts[root])
		if e != nil {
			return out, e
		}
		link.NativeID = root
		for _, old := range m.CoverageLinks {
			if old.NativeID == root && old.NativePromptID == link.NativePromptID {
				return out, incomplete("TRANSCRIPT_COVERAGE_INCOMPLETE", "two AO turns claim one native prompt")
			}
		}
		m.CoverageLinks = append(m.CoverageLinks, link)
		m.CoveredTurns = append(m.CoveredTurns, turn.ID)
	}
	steers, steeredMembers, e := verifySteerInputs(s, segments, m.CoverageLinks, members, details)
	if e != nil {
		return out, e
	}
	m.Steers = steers
	leads := map[string]CoverageLink{}
	for _, link := range m.CoverageLinks {
		leads[link.AOTurnID] = link
	}
	for _, turn := range s.Turns {
		leadID := members[turn.ID]
		if leadID == "" {
			continue
		}
		if childDispatch[leadID] {
			m.ExcludedChildTurns++
			continue
		}
		link, ok := leads[leadID]
		if !ok {
			return out, incomplete("TRANSCRIPT_COVERAGE_INCOMPLETE", "batch dispatch is not covered")
		}
		if steeredMembers[turn.ID] == "" && !initialCaptured[leadID] {
			return out, incomplete("TRANSCRIPT_COVERAGE_INCOMPLETE", "member lacks captured initial input or steer receipt")
		}
		link.AOTurnID = turn.ID
		link.DispatchTurnID = leadID
		link.Method = "batch-member/" + link.Method
		if receipt := steeredMembers[turn.ID]; receipt != "" {
			for _, steer := range steers {
				if steer.ActivityID == receipt {
					link.NativePromptID = steer.NativeRecordID
					link.Method = "steered-batch-member"
				}
			}
		}
		m.CoverageLinks = append(m.CoverageLinks, link)
		m.CoveredTurns = append(m.CoveredTurns, turn.ID)
	}

	for _, t := range s.Terminal {
		found := ""
		for _, p := range nativePrompts[t.NativeID] {
			if domain.NativeCheckpointTextMatches(t.Request, p.User) && (t.Response == "" || domain.NativeCheckpointTextMatches(t.Response, p.Assistant)) {
				if found != "" {
					return out, incomplete("TRANSCRIPT_COVERAGE_INCOMPLETE", "retained TUI boundary is ambiguous")
				}
				found = p.ID
			}
		}
		if found == "" {
			return out, incomplete("TRANSCRIPT_COVERAGE_INCOMPLETE", "retained TUI boundary is absent from native main line")
		}
	}

	for _, message := range s.Messages {
		state, only := aoOnly[message.TurnID]
		if !only && (message.TurnID != "" || message.Origin != domain.MessageOriginDaemon) {
			continue
		}
		entry, text, err := aoSupplement(message, state)
		if err != nil {
			return out, err
		}
		m.AOOnlyMessages++
		m.AOOnlyAttachments += len(entry.Attachments)
		m.AOSupplements = append(m.AOSupplements, entry)
		body.WriteString(text)
	}
	for _, source := range s.Sources {
		if source.Kind == domain.UsageSourceClaudeSubagent {
			m.ExcludedNativeSources = append(m.ExcludedNativeSources, SourceExclusion{NativeID: source.NativeSessionID, SubagentID: source.SubagentID, Path: source.ArtifactPath, Reason: "owned child file outside main-line scope; record count not read", RecordCountKnown: false})
		}
	}
	out.Manifest = m
	out.Transcript = []byte(body.String())
	out.seal = out.digest()
	expected := append([]ports.NativeTranscriptSegment(nil), m.Segments...)
	out.revalidate = func(check context.Context) (NativeStability, error) {
		var stable NativeStability
		for _, segment := range expected {
			again, e := reader.ReadNativeTranscript(check, verifiedRequests[segment.NativeID])
			if e != nil {
				return NativeStability{}, e
			}
			if again.SHA256 != segment.SHA256 || again.NativeID != segment.NativeID || again.LeafID != segment.LeafID || again.ByteCutoff != segment.ByteCutoff || again.SourcePath != segment.SourcePath {
				return NativeStability{}, incomplete("TRANSCRIPT_SOURCE_CHANGED", "native boundary changed after verification")
			}
			if again.CheckStability == nil {
				return NativeStability{}, incomplete("TRANSCRIPT_STABILITY_UNVERIFIED", "native reader did not supply a stability witness")
			}
			stable.checks = append(stable.checks, again.CheckStability)
		}
		return stable, nil
	}
	return out, nil
}

func inventory(s domain.TranscriptSourceSnapshot) (map[string]string, error) {
	required := map[string]string{}
	known := map[string]string{}
	add := func(h domain.AgentHarness, id, config string) error {
		if h != domain.HarnessClaudeCode {
			return incomplete("ORCHESTRATOR_HANDOFF_HISTORY_UNSUPPORTED", "required history includes a non-Claude epoch")
		}
		if id == "" {
			return incomplete("TRANSCRIPT_EPOCHS_INCOMPLETE", "native history has no identity")
		}
		required[id] = ""
		if config != "" {
			if !filepath.IsAbs(config) {
				return incomplete("TRANSCRIPT_SOURCE_INVALID", "native config root is not absolute")
			}
			if old := known[id]; old != "" && old != config {
				return incomplete("TRANSCRIPT_SOURCE_AMBIGUOUS", "native identity has conflicting config roots")
			}
			known[id] = config
		}
		return nil
	}
	for _, transition := range s.Transitions {
		if transition.Active() || transition.Phase == domain.SessionInterfaceTransitionRecovery {
			return nil, incomplete("TRANSCRIPT_SOURCE_UNSETTLED", "interface/account transition is unsettled")
		}
		if transition.NativeConversationID != "" {
			if e := add(domain.HarnessClaudeCode, transition.NativeConversationID, ""); e != nil {
				return nil, e
			}
		}
	}

	for _, n := range s.NativeSessions {
		dir := n.ConfigDir
		if n.TranscriptPath != "" {
			found, e := configFromPath(n.TranscriptPath, n.NativeID)
			if e != nil {
				return nil, e
			}
			if dir != "" && dir != found {
				return nil, incomplete("TRANSCRIPT_SOURCE_AMBIGUOUS", "retained native path and config root conflict")
			}
			dir = found
		}
		if e := add(n.Harness, n.NativeID, dir); e != nil {
			return nil, e
		}
	}
	for _, b := range s.Bindings {
		if e := add(b.Harness, b.NativeRootID, ""); e != nil {
			return nil, e
		}
	}
	for _, b := range s.Branches {
		if b.SessionID == s.Session.ID && b.ProviderConversationID != "" {
			if e := add(domain.HarnessClaudeCode, b.ProviderConversationID, ""); e != nil {
				return nil, e
			}
		}
	}
	for _, t := range s.Terminal {
		if e := add(t.Harness, t.NativeID, ""); e != nil {
			return nil, e
		}
	}
	for _, id := range []string{s.Session.Metadata.AgentSessionID, s.Session.Metadata.ProviderConversationID} {
		if id != "" {
			if e := add(s.Session.Harness, id, ""); e != nil {
				return nil, e
			}
		}
	}
	bind := map[int64]domain.UsageBindingRecord{}
	for _, b := range s.Bindings {
		bind[b.ID] = b
	}
	for _, source := range s.Sources {
		b, ok := bind[source.BindingID]
		if !ok {
			return nil, incomplete("TRANSCRIPT_SOURCE_INVALID", "usage source has no owned binding")
		}
		if source.Kind == domain.UsageSourceClaudeSubagent {
			if source.SubagentID == "" || source.NativeSessionID != b.NativeRootID {
				return nil, incomplete("TRANSCRIPT_PROVENANCE_UNKNOWN", "child source lacks its parent native identity")
			}
			continue
		}
		if source.Generation > 1 {
			return nil, incomplete("TRANSCRIPT_EPOCHS_INCOMPLETE", "native source rotation requires retained generation evidence")
		}
		if source.Kind != domain.UsageSourceClaudeMain || source.NativeSessionID != b.NativeRootID || source.SubagentID != "" {
			return nil, incomplete("TRANSCRIPT_PROVENANCE_UNKNOWN", "usage source is not an owned main transcript")
		}
		dir, e := configFromPath(source.ArtifactPath, source.NativeSessionID)
		if e != nil {
			return nil, e
		}
		if e := add(b.Harness, source.NativeSessionID, dir); e != nil {
			return nil, e
		}
	}
	if p := s.Session.Metadata.NativeTranscriptPath; p != "" {
		id := s.Session.Metadata.AgentSessionID
		if id == "" {
			id = s.Session.Metadata.ProviderConversationID
		}
		dir, e := configFromPath(p, id)
		if e != nil {
			return nil, e
		}
		if e := add(s.Session.Harness, id, dir); e != nil {
			return nil, e
		}
	}
	for id := range required {
		if known[id] == "" {
			return nil, incomplete("TRANSCRIPT_EPOCHS_INCOMPLETE", "native epoch has no retained config-root ownership")
		}
		required[id] = known[id]
	}
	return required, nil
}
func configFromPath(p, id string) (string, error) {
	if id == "" || !filepath.IsAbs(p) || filepath.Clean(p) != p || filepath.Base(p) != id+".jsonl" {
		return "", incomplete("TRANSCRIPT_SOURCE_INVALID", "owned native path does not match its identity")
	}
	project := filepath.Dir(p)
	projects := filepath.Dir(project)
	if filepath.Base(projects) != "projects" {
		return "", incomplete("TRANSCRIPT_SOURCE_INVALID", "unknown Claude transcript layout")
	}
	return filepath.Dir(projects), nil
}
func displayTime(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}
func literal(s string) string {
	fence := "```"
	for strings.Contains(s, fence) {
		fence += "`"
	}
	return fence + "text\n" + s + "\n" + fence + "\n\n"
}

// Inline code keeps untrusted metadata inert; Quote makes line breaks visible.
func inlineMetadata(s string) string {
	quoted := strconv.Quote(s)
	s = quoted[1 : len(quoted)-1]
	fence := "`"
	for strings.Contains(s, fence) {
		fence += "`"
	}
	return fence + " " + s + " " + fence
}
func renderBlock(b ports.NativeTranscriptBlock) string {
	switch b.Kind {
	case ports.TranscriptText:
		return literal(b.Text)
	case ports.TranscriptToolCall:
		return fmt.Sprintf("Tool %s / %s / %s\n\n%s", inlineMetadata(b.ToolName), inlineMetadata(b.CallID), inlineMetadata(b.Status), literal(b.Text))
	case ports.TranscriptToolResult:
		return fmt.Sprintf("Tool result %s / %s; full output omitted (one result block).\n\n", inlineMetadata(b.CallID), inlineMetadata(b.Status))
	default:
		return fmt.Sprintf("Attachment/reference: %s\n\n", inlineMetadata(b.Reference))
	}
}

func (o Output) digest() [32]byte {
	h := sha256.New()
	_ = json.NewEncoder(h).Encode(o.Manifest)
	_, _ = h.Write(o.Transcript)
	var sum [32]byte
	copy(sum[:], h.Sum(nil))
	return sum
}

// Valid detects accidental mutation between verification and publication.
func (o Output) Valid() bool { return o.seal != [32]byte{} && o.seal == o.digest() }

// Revalidate re-reads the fixed native boundaries; the caller still owns intake
// fencing and must compare the AO source snapshot under its publication guard.
func (o Output) Revalidate(ctx context.Context) error {
	_, err := o.RevalidateForPublication(ctx)
	return err
}

// NativeStability carries bounded checks from fresh reads outside the DB guard.
type NativeStability struct{ checks []func(context.Context) error }

// Check compares native file fingerprints without parsing under the DB guard.
func (s NativeStability) Check(ctx context.Context) error {
	if len(s.checks) == 0 {
		return incomplete("TRANSCRIPT_STABILITY_UNVERIFIED", "native stability witness is missing")
	}
	for _, check := range s.checks {
		if err := check(ctx); err != nil {
			return err
		}
	}
	return ctx.Err()
}

// RevalidateForPublication strictly rereads sources before reservation. Check the
// returned witness immediately before publishing under that guard.
func (o Output) RevalidateForPublication(ctx context.Context) (NativeStability, error) {
	if !o.Valid() || o.revalidate == nil {
		return NativeStability{}, incomplete("TRANSCRIPT_SOURCE_INVALID", "verified export was modified")
	}
	return o.revalidate(ctx)
}
