package integration

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"time"
)

// Snapshot is replaced as a whole. It contains no raw text, paths, tool args,
// tool-provided identifiers or inferred canonical event/request keys.
type Snapshot struct {
	Source             string       `json:"source"`
	Engine             string       `json:"engine"`
	EngineVersion      string       `json:"engineVersion"`
	ProjectID          string       `json:"projectId"`
	SessionID          string       `json:"sessionId"`
	NativeID           string       `json:"nativeId"`
	ObservedAt         time.Time    `json:"observedAt"`
	Status             string       `json:"status"`
	Reason             string       `json:"reason"`
	BucketAvailability string       `json:"bucketAvailability"`
	Events             []Event      `json:"events"`
	Tools              []ToolCounts `json:"tools"`
}

type Event struct {
	At                  time.Time `json:"at"`
	InputTokens         int64     `json:"inputTokens"`
	CacheReadTokens     int64     `json:"cacheReadTokens"`
	CacheWriteTokens    int64     `json:"cacheWriteTokens"`
	InputIncludingCache int64     `json:"inputIncludingCache"`
	OutputTokens        int64     `json:"outputTokens"`
	ReasoningTokens     int64     `json:"reasoningTokens"`
	EngineTotalTokens   int64     `json:"engineTotalTokens"`
}

type ToolCounts struct {
	Category string `json:"category"`
	Calls    int64  `json:"calls"`
	Errors   int64  `json:"errors"`
	Unknown  int64  `json:"unknown"`
}

func (r Runner) Report(ctx context.Context, m Manifest, sources []AuthorizedSource) Snapshot {
	out := Snapshot{Source: "external_analysis", Engine: EngineName, EngineVersion: EngineVersion,
		ProjectID: m.Identity.ProjectID, SessionID: m.Identity.SessionID, NativeID: m.Identity.NativeID,
		ObservedAt: time.Now().UTC(), Status: "unavailable", BucketAvailability: "engine_normalized_native_availability_unknown", Events: []Event{}, Tools: []ToolCounts{}}
	fail := func(reason string) Snapshot { out.Reason = reason; return out }
	if !ResolvePolicy(m.Identity.Role, m.Inputs).Enabled {
		out.Status = "disabled"
		return fail("integration_disabled")
	}
	if m.SchemaVersion != ManifestVersion || !m.Identity.valid() || m.Namespace != m.Identity.namespace() || !m.Active || m.Engine == nil || !m.Engine.valid() {
		return fail("manifest_or_engine_unavailable")
	}
	if len(sources) == 0 || m.Identity.NativeID == "" {
		out.Status = "unknown"
		return fail("native_source_unknown")
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	e, modules, err := engineFiles(ctx, m.Engine.Executable, m.Engine.NodeExecutable)
	if err != nil || e != *m.Engine {
		return fail(reportFailure(ctx, "engine_changed_or_unavailable"))
	}
	dir, err := r.jobDir()
	if err != nil {
		return fail("scratch_unavailable")
	}
	defer os.RemoveAll(dir)
	source := sources[0].Source
	if (source != "claude" && source != "codex") || (m.Identity.Harness != source && !(m.Identity.Harness == "claude-code" && source == "claude")) {
		return fail("untrusted_source")
	}
	seen := map[string]bool{}
	for i, s := range sources {
		key := s.Root + "\x00" + s.Path
		if i >= 64 || s.Source != source || s.NativeID != m.Identity.NativeID || seen[key] {
			return fail("untrusted_source")
		}
		seen[key] = true
		if err := copySource(ctx, dir, i, s); err != nil {
			return fail(reportFailure(ctx, "source_view_unavailable"))
		}
	}
	args := []string{"--source", source, "--source-dir", source + "=/view/" + source, "--all", "--no-sync", "--no-history"}
	raw, err := r.execute(ctx, dir, modules, e.NodeExecutable, append([]string{"events", "--pricing-offline", "--format", "jsonl"}, args...)...)
	if err != nil {
		return fail(reportFailure(ctx, "engine_execution_failed"))
	}
	events, err := projectEvents(raw, source, m.Identity.NativeID)
	if err != nil {
		return fail("event_schema_or_identity_mismatch")
	}
	raw, err = r.execute(ctx, dir, modules, e.NodeExecutable, append([]string{"tools", "--json", "--by-source", "--top", "0"}, args...)...)
	if err != nil {
		return fail(reportFailure(ctx, "engine_execution_failed"))
	}
	tools, err := projectTools(raw, source)
	if err != nil {
		return fail("tools_schema_mismatch")
	}
	if ctx.Err() != nil {
		return fail(reportFailure(ctx, "cancelled"))
	}
	out.Events, out.Tools, out.Status, out.Reason = events, tools, "ok", "external_snapshot_only"
	return out
}

func reportFailure(ctx context.Context, fallback string) string {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return "deadline_exceeded"
	}
	if ctx.Err() != nil {
		return "cancelled"
	}
	return fallback
}

// The public engine normalizes absent native buckets to zero. These are engine
// numbers only; BucketAvailability prevents treating zero as native evidence.
func projectEvents(raw []byte, source, native string) ([]Event, error) {
	out := []Event{}
	s := bufio.NewScanner(bytes.NewReader(raw))
	s.Buffer(make([]byte, 4096), 1<<20)
	for s.Scan() {
		var row struct {
			Source, SessionID, Timestamp, CostMode                                                     string
			InputTokens, OutputTokens, ReasoningTokens, CacheReadTokens, CacheWriteTokens, TotalTokens *int64
		}
		if json.Unmarshal(s.Bytes(), &row) != nil || row.Source != source || row.SessionID != native || (row.CostMode != "explicit" && row.CostMode != "estimated") {
			return nil, errors.New("event schema mismatch")
		}
		at, err := time.Parse(time.RFC3339Nano, row.Timestamp)
		if err != nil {
			return nil, errors.New("event timestamp mismatch")
		}
		for _, n := range []*int64{row.InputTokens, row.OutputTokens, row.ReasoningTokens, row.CacheReadTokens, row.CacheWriteTokens, row.TotalTokens} {
			if !safeNumber(n) {
				return nil, errors.New("event bucket mismatch")
			}
		}
		input := *row.InputTokens + *row.CacheReadTokens + *row.CacheWriteTokens
		if input > maxSafeInteger || *row.ReasoningTokens > *row.OutputTokens {
			return nil, errors.New("event vector mismatch")
		}
		out = append(out, Event{at.UTC(), *row.InputTokens, *row.CacheReadTokens, *row.CacheWriteTokens, input, *row.OutputTokens, *row.ReasoningTokens, *row.TotalTokens})
	}
	return out, s.Err()
}

const maxSafeInteger = 1<<53 - 1

func safeNumber(n *int64) bool { return n != nil && *n >= 0 && *n <= maxSafeInteger }

func projectTools(raw []byte, source string) ([]ToolCounts, error) {
	var report struct {
		SchemaVersion int
		Report        string
		Data          struct {
			BySource *bool
			ByRepo   *bool
			RowCount *int64
			Rows     []struct {
				Name, Source                     string
				Calls, Errors, Unknown, Sessions *int64
			}
			Totals struct{ Calls, Errors, Unknown, Sessions, Tools *int64 }
		}
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	if d.Decode(&report) != nil || d.Decode(new(any)) != io.EOF || report.SchemaVersion != 1 || report.Report != "tools" || report.Data.BySource == nil || !*report.Data.BySource || report.Data.ByRepo == nil || *report.Data.ByRepo || !safeNumber(report.Data.RowCount) || report.Data.Rows == nil || *report.Data.RowCount != int64(len(report.Data.Rows)) {
		return nil, errors.New("tools schema mismatch")
	}
	for _, n := range []*int64{report.Data.Totals.Calls, report.Data.Totals.Errors, report.Data.Totals.Unknown, report.Data.Totals.Sessions, report.Data.Totals.Tools} {
		if !safeNumber(n) {
			return nil, errors.New("tools totals mismatch")
		}
	}
	counts := map[string]ToolCounts{}
	var calls, failures, unknown int64
	for _, row := range report.Data.Rows {
		if row.Name == "" || row.Source != source || !safeNumber(row.Calls) || !safeNumber(row.Errors) || !safeNumber(row.Unknown) || !safeNumber(row.Sessions) || *row.Errors+*row.Unknown > *row.Calls {
			return nil, errors.New("tool row mismatch")
		}
		category := "other"
		switch row.Name {
		case "Read", "read_file", "Glob", "Grep", "list_dir":
			category = "read"
		case "Write", "Edit", "MultiEdit", "apply_patch":
			category = "write"
		case "Bash", "exec_command", "shell_command":
			category = "execute"
		}
		c := counts[category]
		c.Category, c.Calls, c.Errors, c.Unknown = category, c.Calls+*row.Calls, c.Errors+*row.Errors, c.Unknown+*row.Unknown
		counts[category] = c
		calls, failures, unknown = calls+*row.Calls, failures+*row.Errors, unknown+*row.Unknown
		if calls > maxSafeInteger {
			return nil, errors.New("tool count overflow")
		}
	}
	if calls != *report.Data.Totals.Calls || failures != *report.Data.Totals.Errors || unknown != *report.Data.Totals.Unknown {
		return nil, errors.New("tool totals mismatch")
	}
	out := []ToolCounts{}
	for _, k := range []string{"read", "write", "execute", "other"} {
		if c, ok := counts[k]; ok {
			out = append(out, c)
		}
	}
	return out, nil
}
