package claudecode

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

var _ ports.NativeTranscriptReader = (*Plugin)(nil)

const transcriptReaderVersion = "claude-jsonl-reader/2"

func transcriptFailure(code, reason string) error {
	return &ports.NativeTranscriptError{Code: code, Reason: reason}
}

// ReadNativeTranscript reads one fixed provider boundary without starting a
// controller, reconciling authentication, or modifying provider-owned files.
func (p *Plugin) ReadNativeTranscript(ctx context.Context, request ports.NativeTranscriptRequest) (ports.NativeTranscriptSegment, error) {
	var segment ports.NativeTranscriptSegment
	if err := ctx.Err(); err != nil {
		return segment, err
	}
	if request.Schema != ports.ClaudeTranscriptSchemaV1 {
		return segment, transcriptFailure("TRANSCRIPT_SCHEMA_UNSUPPORTED", "unverified reader schema")
	}
	native, err := canonicalClaudeNativeSessionID(request.Source.NativeSessionID)
	if err != nil || !filepath.IsAbs(request.Source.ConfigDir) {
		return segment, transcriptFailure("TRANSCRIPT_SOURCE_INVALID", "missing canonical native source")
	}
	if request.ByteCutoff <= 0 || request.LeafID == "" {
		return segment, transcriptFailure("TRANSCRIPT_CUTOFF_INVALID", "a nonempty settled boundary is required")
	}
	maxBytes := request.MaxSourceBytes
	if maxBytes == 0 {
		maxBytes = ports.MaxTranscriptSourceBytes
	}
	maxRecord := request.MaxRecordBytes
	if maxRecord == 0 {
		maxRecord = ports.MaxTranscriptRecordBytes
	}
	maxRecords := request.MaxRecords
	if maxRecords == 0 {
		maxRecords = ports.MaxTranscriptRecords
	}
	if maxBytes < 1 || maxBytes > ports.MaxTranscriptSourceBytes || maxRecord < 1 || maxRecord > ports.MaxTranscriptRecordBytes || maxRecords < 1 || maxRecords > ports.MaxTranscriptRecords || request.ByteCutoff > maxBytes {
		return segment, transcriptFailure("TRANSCRIPT_LIMIT_EXCEEDED", "source exceeds export capacity")
	}
	rootPath := filepath.Join(request.Source.ConfigDir, "projects")
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		return segment, transcriptFailure("TRANSCRIPT_SOURCE_MISSING", "native root is unavailable")
	}
	defer func() { _ = root.Close() }()
	rootInfo, err := root.Stat(".")
	if err != nil {
		return segment, err
	}
	dir, err := root.Open(".")
	if err != nil {
		return segment, err
	}
	projects, err := dir.ReadDir(4097)
	_ = dir.Close()
	if err != nil && !errors.Is(err, io.EOF) {
		return segment, err
	}
	if len(projects) > 4096 {
		return segment, transcriptFailure("TRANSCRIPT_LIMIT_EXCEEDED", "native project directory count exceeds capacity")
	}
	relative := ""
	var before os.FileInfo
	for _, project := range projects {
		if err := ctx.Err(); err != nil {
			return segment, err
		}
		if !project.IsDir() {
			continue
		}
		candidate := project.Name() + "/" + native + ".jsonl"
		info, e := root.Lstat(candidate)
		if os.IsNotExist(e) {
			continue
		}
		if e != nil {
			return segment, e
		}
		if !info.Mode().IsRegular() {
			return segment, transcriptFailure("TRANSCRIPT_SOURCE_INVALID", "native source is not a regular file")
		}
		if relative != "" {
			return segment, transcriptFailure("TRANSCRIPT_SOURCE_AMBIGUOUS", "multiple files claim the native identity")
		}
		relative = candidate
		before = info
	}
	if relative == "" {
		return segment, transcriptFailure("TRANSCRIPT_SOURCE_MISSING", "native transcript is unavailable")
	}
	file, err := root.Open(relative)
	if err != nil {
		return segment, err
	}
	defer func() { _ = file.Close() }()
	opened, err := file.Stat()
	if err != nil {
		return segment, err
	}
	if !os.SameFile(before, opened) || opened.Size() != request.ByteCutoff {
		return segment, transcriptFailure("TRANSCRIPT_CUTOFF_INVALID", "native file differs from the fixed byte boundary")
	}
	var last [1]byte
	if _, err := file.ReadAt(last[:], request.ByteCutoff-1); err != nil || last[0] != '\n' {
		return segment, transcriptFailure("TRANSCRIPT_CUTOFF_INVALID", "native tail is not a complete JSONL record")
	}
	segment = ports.NativeTranscriptSegment{NativeID: native, SourcePath: filepath.Join(rootPath, filepath.FromSlash(relative)), ByteCutoff: request.ByteCutoff, Schema: request.Schema, ReaderVersion: transcriptReaderVersion, LeafID: request.LeafID}
	digest := sha256.New()
	scanner := bufio.NewScanner(io.TeeReader(io.NewSectionReader(file, 0, request.ByteCutoff), digest))
	scanner.Buffer(make([]byte, 4096), maxRecord)
	nodes := map[string]ports.NativeTranscriptRecord{}
	identities := map[string][]byte{}
	versions := map[string]bool{}
	for _, ancestor := range request.VerifiedAncestors {
		if ancestor.ID == "" || ancestor.RecordHash == "" || ancestor.Provenance != domain.NativeThreadMain {
			return ports.NativeTranscriptSegment{}, transcriptFailure("TRANSCRIPT_PROVENANCE_UNKNOWN", "invalid parent proof")
		}
		ancestor.Excluded = "ancestor"
		if old, ok := nodes[ancestor.ID]; ok && old.RecordHash != ancestor.RecordHash {
			return ports.NativeTranscriptSegment{}, transcriptFailure("TRANSCRIPT_IDENTITY_CONFLICT", "parent UUID ownership conflicts")
		}
		nodes[ancestor.ID] = ancestor
	}

	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return ports.NativeTranscriptSegment{}, err
		}
		if segment.Counts.Records >= maxRecords {
			return ports.NativeTranscriptSegment{}, transcriptFailure("TRANSCRIPT_LIMIT_EXCEEDED", "record count exceeds capacity")
		}
		segment.Counts.Records++
		raw := scanner.Bytes()
		if len(raw) > maxRecord {
			return ports.NativeTranscriptSegment{}, transcriptFailure("TRANSCRIPT_LIMIT_EXCEEDED", "native record exceeds configured capacity")
		}
		if !utf8.Valid(raw) {
			return ports.NativeTranscriptSegment{}, transcriptFailure("TRANSCRIPT_RECORD_INVALID", "native source is not UTF-8")
		}
		record, version, e := decodeTranscriptRecord(raw, native, segment.Counts.Records, request)
		if e != nil {
			return ports.NativeTranscriptSegment{}, fmt.Errorf("native line %d: %w", segment.Counts.Records, e)
		}
		if record.RecordHash == "" {
			record.RecordHash, e = canonicalTranscriptHash(raw)
			if e != nil {
				return ports.NativeTranscriptSegment{}, e
			}
		}
		if previous, ok := nodes[record.ID]; ok && previous.Excluded == "ancestor" && record.Excluded != "ancestor" && record.Provenance == domain.NativeThreadMain {
			return ports.NativeTranscriptSegment{}, transcriptFailure("TRANSCRIPT_IDENTITY_CONFLICT", "new native record reuses an ancestor UUID")
		}
		if version != "" {
			versions[version] = true
		}
		if record.Provenance == domain.NativeThreadChild {
			segment.Counts.ChildRecords++
			segment.Records = append(segment.Records, record)
			continue
		}
		if record.ID != "" {
			var value any
			decoder := json.NewDecoder(bytes.NewReader(raw))
			decoder.UseNumber()
			if err := decoder.Decode(&value); err != nil {
				return ports.NativeTranscriptSegment{}, err
			}
			canonical, e := json.Marshal(value)
			if e != nil {
				return ports.NativeTranscriptSegment{}, e
			}
			if previous, found := identities[record.ID]; found {
				if !bytes.Equal(previous, canonical) {
					return ports.NativeTranscriptSegment{}, transcriptFailure("TRANSCRIPT_IDENTITY_CONFLICT", "native UUID has conflicting records")
				}
				record.Excluded = "duplicate"
				segment.Counts.Duplicates++
				segment.Records = append(segment.Records, record)
				continue
			}
			identities[record.ID] = canonical
			nodes[record.ID] = record
		}
		if record.Excluded == "ancestor" {
			segment.Counts.AncestorRecords++
		} else if record.Excluded != "" {
			segment.Counts.MetadataRecords++
		} else {
			segment.Counts.MainRecords++
			if record.Type == "user" {
				segment.Counts.UserRecords++
			}
			if record.Type == "assistant" {
				segment.Counts.AssistantRecords++
			}
			for _, block := range record.Blocks {
				switch block.Kind {
				case ports.TranscriptToolCall:
					segment.Counts.ToolCalls++
				case ports.TranscriptToolResult:
					segment.Counts.ToolResults++
					segment.Counts.OmittedToolResults++
				case ports.TranscriptAttachment:
					segment.Counts.Attachments++
				}
			}
		}
		if record.Timestamp == "" {
			segment.Counts.UnknownTimestamps++
		}
		segment.Records = append(segment.Records, record)
	}
	if err := scanner.Err(); err != nil {
		return ports.NativeTranscriptSegment{}, transcriptFailure("TRANSCRIPT_LIMIT_EXCEEDED", "native record exceeds readable capacity")
	}
	lineage := []string{}
	visited := map[string]bool{}
	leaf := request.LeafID
	for leaf != "" {
		record, found := nodes[leaf]
		if !found && len(request.KnownNativeIDs) > 1 && len(request.VerifiedAncestors) == 0 {
			return ports.NativeTranscriptSegment{}, transcriptFailure("TRANSCRIPT_ANCESTOR_REQUIRED", "parent epoch must be verified first")
		}
		if !found || visited[leaf] {
			return ports.NativeTranscriptSegment{}, transcriptFailure("TRANSCRIPT_ANCESTRY_INCOMPLETE", "native ancestry is missing or cyclic")
		}
		visited[leaf] = true
		lineage = append(lineage, leaf)
		if record.Excluded == "" {
			segment.MainRecordIDs = append(segment.MainRecordIDs, leaf)
		}
		leaf = record.ParentID
	}
	slices.Reverse(segment.MainRecordIDs)
	slices.Reverse(lineage)
	for id, record := range nodes {
		if record.Excluded == "" && !visited[id] {
			return ports.NativeTranscriptSegment{}, transcriptFailure("TRANSCRIPT_ANCESTRY_INCOMPLETE", "unclassified main-line fork is outside the selected ancestry")
		}
	}
	if segment.Counts.MainRecords == 0 && segment.Counts.AncestorRecords == 0 {
		return ports.NativeTranscriptSegment{}, transcriptFailure("TRANSCRIPT_ANCESTRY_INCOMPLETE", "no readable main-line records")
	}
	if err := verifyTranscriptTail(&segment, nodes, lineage); err != nil {
		return ports.NativeTranscriptSegment{}, err
	}
	after, err := file.Stat()
	if err != nil {
		return ports.NativeTranscriptSegment{}, err
	}
	named, err := root.Lstat(relative)
	if err != nil {
		return ports.NativeTranscriptSegment{}, err
	}
	namedRoot, err := os.Stat(rootPath)
	if err != nil {
		return ports.NativeTranscriptSegment{}, err
	}
	if !os.SameFile(opened, named) || !os.SameFile(rootInfo, namedRoot) || after.Size() != opened.Size() || !after.ModTime().Equal(opened.ModTime()) {
		return ports.NativeTranscriptSegment{}, transcriptFailure("TRANSCRIPT_SOURCE_CHANGED", "native source changed during export")
	}
	segment.SHA256 = hex.EncodeToString(digest.Sum(nil))
	for version := range versions {
		segment.ProviderVersions = append(segment.ProviderVersions, version)
	}
	slices.Sort(segment.ProviderVersions)
	// Re-read the same descriptor's prefix: an in-place rewrite must not pass
	// merely because an attacker restored size and mtime.
	check := sha256.New()
	if _, err := io.Copy(check, io.NewSectionReader(file, 0, request.ByteCutoff)); err != nil {
		return ports.NativeTranscriptSegment{}, err
	}
	if !bytes.Equal(check.Sum(nil), digest.Sum(nil)) {
		return ports.NativeTranscriptSegment{}, transcriptFailure("TRANSCRIPT_SOURCE_CHANGED", "native bytes changed during export")
	}
	if err := ctx.Err(); err != nil {
		return ports.NativeTranscriptSegment{}, err
	}
	return segment, nil
}

func supportedTranscriptVersion(version string) bool {
	// This reader profile validates the 2.1 JSONL record schema. A new schema
	// family must acquire fixtures before it can claim complete coverage.
	if !strings.HasPrefix(version, "2.1.") {
		return false
	}
	value := strings.TrimPrefix(version, "2.1.")
	if value == "" || len(value) > 1 && value[0] == '0' {
		return false
	}
	for _, c := range value {
		if c < '0' || c > '9' {
			return false
		}
	}
	patch, err := strconv.Atoi(value)
	return err == nil && patch >= 0
}
