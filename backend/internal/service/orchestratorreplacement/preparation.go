package orchestratorreplacement

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// MaxHandoffBytes bounds the only document embedded in successor bootstrap.
const MaxHandoffBytes = domain.MaxReplacementHandoffBytes

// PreparationMetadata binds an agent-authored document to one daemon prompt.
type PreparationMetadata struct {
	Version         int              `json:"version"`
	OperationID     string           `json:"operationId"`
	AttemptID       string           `json:"attemptId"`
	Nonce           string           `json:"nonce"`
	SourceSessionID domain.SessionID `json:"sourceSessionId"`
	LeaseEpoch      int64            `json:"leaseEpoch"`
}

// ValidatePreparation checks format and request binding, not the agent's
// semantic understanding of pending questions, directives or authorization.
func ValidatePreparation(data []byte, expected PreparationMetadata, baseline string, sessions []domain.SessionID) error {
	if len(data) == 0 || len(data) > MaxHandoffBytes || !utf8.Valid(data) || bytes.IndexByte(data, 0) >= 0 {
		return fmt.Errorf("handoff requires bounded UTF-8 text")
	}
	sum := sha256.Sum256(data)
	if hex.EncodeToString(sum[:]) == baseline {
		return fmt.Errorf("handoff is unchanged from baseline")
	}
	lines := strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n")
	const prefix = "<!-- ao-handoff "
	first := lines[0]
	if !strings.HasPrefix(first, prefix) || !strings.HasSuffix(first, " -->") {
		return fmt.Errorf("handoff metadata must be the first line")
	}
	d := json.NewDecoder(strings.NewReader(strings.TrimSuffix(strings.TrimPrefix(first, prefix), " -->")))
	d.DisallowUnknownFields()
	var actual PreparationMetadata
	if err := d.Decode(&actual); err != nil {
		return fmt.Errorf("invalid handoff metadata")
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return fmt.Errorf("handoff metadata has trailing values")
	}
	if actual != expected || actual.Version != 1 || actual.OperationID == "" || actual.AttemptID == "" || actual.Nonce == "" || actual.SourceSessionID == "" || actual.LeaseEpoch <= 0 {
		return fmt.Errorf("handoff metadata does not match this preparation")
	}
	var current, sessionMap *strings.Builder
	sectionCount := 0
	for _, line := range lines[1:] {
		if strings.HasPrefix(line, "## ") {
			heading := strings.TrimPrefix(line, "## ")
			if len(heading) < 2 || sectionCount >= 6 || heading[0] != 'A'+byte(sectionCount) || heading[1] != ' ' {
				return fmt.Errorf("handoff needs exactly A–F in order")
			}
			if current != nil && strings.TrimSpace(current.String()) == "" {
				return fmt.Errorf("handoff has an empty section")
			}
			current = &strings.Builder{}
			if sectionCount == 1 {
				sessionMap = current
			}
			sectionCount++
			continue
		}
		if current != nil {
			current.WriteString(line)
			current.WriteByte('\n')
		}
	}
	if sectionCount != 6 || sessionMap == nil {
		return fmt.Errorf("handoff needs all A–F sections")
	}
	if current == nil || strings.TrimSpace(current.String()) == "" {
		return fmt.Errorf("handoff has an empty section")
	}
	return validateSessionMap(sessionMap.String(), sessions)
}

func tableCells(line string) []string {
	line = strings.TrimSpace(line)
	if !strings.HasPrefix(line, "|") || !strings.HasSuffix(line, "|") {
		return nil
	}
	parts := strings.Split(strings.TrimSuffix(strings.TrimPrefix(line, "|"), "|"), "|")
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}
	return parts
}

func validateSessionMap(section string, expected []domain.SessionID) error {
	var rows [][]string
	for _, line := range strings.Split(section, "\n") {
		if cells := tableCells(line); cells != nil {
			rows = append(rows, cells)
		}
	}
	header := []string{"sessionId", "task", "state", "nextD", "lastACK", "unansweredQ"}
	if len(rows) < 2 || len(rows[0]) != len(header) || len(rows[1]) != len(header) {
		return fmt.Errorf("section B requires the canonical six-column session map")
	}
	for i, name := range header {
		if rows[0][i] != name || len(strings.Trim(rows[1][i], ":")) < 3 || strings.Trim(rows[1][i], ":-") != "" {
			return fmt.Errorf("section B has invalid columns or separator")
		}
	}
	seen := map[domain.SessionID]bool{}
	for _, row := range rows[2:] {
		if len(row) != len(header) {
			return fmt.Errorf("section B row has missing columns")
		}
		for _, cell := range row {
			if cell == "" {
				return fmt.Errorf("section B requires explicit none or unknown values, not blanks")
			}
		}
		id := domain.SessionID(row[0])
		if seen[id] {
			return fmt.Errorf("section B repeats a session")
		}
		seen[id] = true
	}
	for _, id := range expected {
		if !seen[id] {
			return fmt.Errorf("section B is missing a session from the cutoff")
		}
	}
	return nil
}
