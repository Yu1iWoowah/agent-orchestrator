// Package modelcapabilities resolves effort evidence without changing launch selections.
package modelcapabilities

import (
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

var claudeModelID = regexp.MustCompile(`^claude-(fable|opus|sonnet|haiku)-([0-9]{1,2})(?:-([0-9]{1,2}))?(?:-[0-9]{8})?(?:\[1m\])?$`)

// Efforts returns advertised effort levels and whether the evidence is known.
// Empty efforts on a concrete model means no supported effort. A Claude family
// alias with no own efforts borrows only from the newest canonical model ID in
// the same provider scope. Labels and regional provider IDs are not proof of
// alias resolution; absent or conflicting evidence stays unknown.
func Efforts(harness domain.AgentHarness, models []ports.AgentModelInfo, selected ports.AgentModelInfo) ([]string, bool) {
	if harness != domain.HarnessClaudeCode || len(selected.Efforts) > 0 {
		return selected.Efforts, true
	}
	family := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(selected.ID)), "[1m]")
	switch family {
	case "fable", "opus", "sonnet", "haiku":
	default:
		return selected.Efforts, true
	}
	var efforts []string
	best := -1
	known := false
	for _, model := range models {
		if model.Provider != selected.Provider {
			continue
		}
		match := claudeModelID.FindStringSubmatch(model.ID)
		if len(match) != 4 || match[1] != family {
			continue
		}
		major, _ := strconv.Atoi(match[2])
		minor, _ := strconv.Atoi(match[3])
		version := major*100 + minor
		if version > best {
			best, efforts, known = version, model.Efforts, true
		} else if version == best && !slices.Equal(efforts, model.Efforts) {
			known = false
		}
	}
	if !known {
		return nil, false
	}
	return efforts, true
}
