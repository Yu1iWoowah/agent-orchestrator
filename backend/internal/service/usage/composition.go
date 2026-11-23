package usage

import (
	"bufio"
	"context"
	"errors"
	"io"
	"os"
	"sort"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// Composition categories, in display order.
const (
	CategorySystem = iota
	CategoryTools
	CategoryMCPTools
	CategoryInjectedSystem
	CategoryHistory
	CategoryToolUse
	CategoryToolResult
	CategoryThinking
	CategoryThinkingEncrypted
	CategoryWorldState
	CategoryCount
)

var compositionCategoryNames = [CategoryCount]string{
	"system", "tools", "mcp_tools", "injected_system", "history",
	"tool_use", "tool_result", "thinking", "thinking_encrypted", "world_state",
}

// CompositionCounts holds bytes per category; -1 marks UNKNOWN.
type CompositionCounts [CategoryCount]int64

func (c *CompositionCounts) add(o CompositionCounts) {
	for i := range c {
		c[i] += o[i]
	}
}

// CompositionRequest is one model request's exact input and category bytes.
type CompositionRequest struct {
	Timestamp  string
	Input      *int64
	Unresolved int
	Counts     CompositionCounts
}

// CompositionRow allocates the exact input by byte share with the largest
// remainder method, so allocated tokens sum to the input. World state and
// UNKNOWN categories keep their bytes but get no tokens.
func CompositionRow(ordinal int, req CompositionRequest, unknownMCP bool) domain.UsageCompositionRequest {
	counts := req.Counts
	if unknownMCP {
		counts[CategoryMCPTools] = -1
	}
	allocated := func(i int) bool {
		return i != CategoryWorldState && counts[i] >= 0
	}
	var total int64
	for i := range counts {
		if allocated(i) {
			total += counts[i]
		}
	}
	row := domain.UsageCompositionRequest{Ordinal: ordinal, Timestamp: req.Timestamp, InputTokens: req.Input, Unresolved: req.Unresolved}
	for i, name := range compositionCategoryNames {
		cat := domain.UsageCompositionCategory{Category: name}
		switch {
		case counts[i] < 0:
			cat.Note = "unknown"
		case i == CategoryThinkingEncrypted:
			cat.Note = "encrypted_hidden"
		case i == CategoryWorldState:
			cat.Note = "delivery_unverified"
		}
		if counts[i] >= 0 {
			b := counts[i]
			cat.Bytes = &b
		}
		if allocated(i) && req.Input != nil && total > 0 {
			t := *req.Input * counts[i] / total
			cat.Tokens = &t
		}
		row.Categories = append(row.Categories, cat)
	}
	if req.Input != nil && total > 0 {
		var given int64
		var order []int
		for i := range counts {
			if allocated(i) {
				given += *row.Categories[i].Tokens
				order = append(order, i)
			}
		}
		rem := func(i int) int64 { return *req.Input * counts[i] % total }
		sort.SliceStable(order, func(a, b int) bool { return rem(order[a]) > rem(order[b]) })
		for _, i := range order[:*req.Input-given] {
			*row.Categories[i].Tokens++
		}
	}
	return row
}

// EachLine streams JSONL without retaining earlier lines.
func EachLine(r io.Reader, fn func([]byte)) error {
	br := bufio.NewReaderSize(r, 1<<20)
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			fn(line)
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

type compositionStore interface {
	ListUsageBindingsForSession(context.Context, domain.SessionID) ([]domain.UsageBindingRecord, error)
	ListUsageSourcesForBinding(context.Context, int64) ([]domain.UsageSourceRecord, error)
	GetReviewByID(context.Context, string) (domain.Review, bool, error)
}

// CompositionReader computes per-request composition at query time from the
// provider's local file. It reads read-only and stores nothing.
type CompositionReader struct {
	store     compositionStore
	locate    func(context.Context, domain.AgentHarness, string) (string, error)
	readCodex func(io.Reader) ([]CompositionRequest, error)
}

// NewCompositionReader returns a reader over registered usage sources;
// locate finds a reviewer's unregistered file by native id, and readCodex
// reads a rollout with the usage parser's request boundaries.
func NewCompositionReader(store compositionStore, locate func(context.Context, domain.AgentHarness, string) (string, error), readCodex func(io.Reader) ([]CompositionRequest, error)) *CompositionReader {
	return &CompositionReader{store: store, locate: locate, readCodex: readCodex}
}

// ErrCompositionReviewNotFound reports a review that is not this session's.
var ErrCompositionReviewNotFound = errors.New("review not found for session")

// Composition reads the session's root Claude transcripts or Codex rollouts,
// or with reviewID the session's reviewer file.
func (r *CompositionReader) Composition(ctx context.Context, id domain.SessionID, reviewID string) (domain.UsageComposition, error) {
	out := domain.UsageComposition{SessionID: id, Status: "unknown", Reason: "no_source", Requests: []domain.UsageCompositionRequest{}}
	var files []compositionFile
	if reviewID != "" {
		review, ok, err := r.store.GetReviewByID(ctx, reviewID)
		if err != nil {
			return out, err
		}
		if !ok || review.SessionID != id {
			return out, ErrCompositionReviewNotFound
		}
		out.ReviewID, out.NotCanonical = reviewID, true
		nativeID := review.AgentSessionID
		if review.InterfaceMode == domain.ReviewerInterfaceChat {
			nativeID = review.ProviderConversationID
		}
		kind := domain.UsageSourceCodexRollout
		if review.Harness == domain.ReviewerClaudeCode {
			kind = domain.UsageSourceClaudeMain
		} else if review.Harness != domain.ReviewerCodex {
			return out, nil
		}
		if r.locate != nil && nativeID != "" {
			if path, err := r.locate(ctx, domain.AgentHarness(review.Harness), nativeID); err == nil && path != "" {
				files = append(files, compositionFile{kind: kind, path: path})
			}
		}
	} else {
		bindings, err := r.store.ListUsageBindingsForSession(ctx, id)
		if err != nil {
			return out, err
		}
		for _, binding := range bindings {
			sources, err := r.store.ListUsageSourcesForBinding(ctx, binding.ID)
			if err != nil {
				return out, err
			}
			// A rebuilt or relocated file keeps its replaced generation; read only
			// the latest generation per kind and native session, as the collector
			// does for Codex.
			latest := map[string]domain.UsageSourceRecord{}
			var order []string
			for _, source := range sources {
				if source.SubagentID != "" || (source.Kind != domain.UsageSourceClaudeMain && source.Kind != domain.UsageSourceCodexRollout) {
					continue
				}
				key := string(source.Kind) + "\x00" + source.NativeSessionID
				current, seen := latest[key]
				if !seen {
					order = append(order, key)
				}
				if !seen || source.Generation > current.Generation || (source.Generation == current.Generation && source.ID > current.ID) {
					latest[key] = source
				}
			}
			for _, key := range order {
				files = append(files, compositionFile{kind: latest[key].Kind, path: latest[key].ArtifactPath})
			}
		}
	}
	var reqs []CompositionRequest
	for _, f := range files {
		out.Source = string(f.kind)
		got, ok := f.read(r.readCodex)
		if !ok {
			out.Reason = "source_read_failed"
			return out, nil
		}
		reqs = append(reqs, got...)
	}
	if out.Source != "" && len(reqs) == 0 {
		out.Reason = "no_request_records"
	}
	if len(reqs) == 0 {
		return out, nil
	}
	out.Status, out.Reason = "ok", ""
	for i, req := range reqs {
		out.Requests = append(out.Requests, CompositionRow(i+1, req, out.Source == string(domain.UsageSourceCodexRollout)))
	}
	return out, nil
}

type compositionFile struct {
	kind domain.UsageSourceKind
	path string
}

// read reports a missing or unreadable file as not ok; the response says
// source_read_failed instead of failing the request.
func (f compositionFile) read(readCodex func(io.Reader) ([]CompositionRequest, error)) ([]CompositionRequest, bool) {
	read := readCodex
	if f.kind == domain.UsageSourceClaudeMain {
		read = readClaudeComposition
	}
	if read == nil {
		return nil, false
	}
	file, err := os.Open(f.path)
	if err != nil {
		return nil, false
	}
	defer func() { _ = file.Close() }()
	reqs, err := read(file)
	return reqs, err == nil
}
