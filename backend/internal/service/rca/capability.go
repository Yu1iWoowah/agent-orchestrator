package rca

import (
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// This is an approved numeric-only PR1 attachment, never a source-body hash.
// Missing/changed evidence is UNKNOWN; no debug/native/provider fallback runs.
//
//go:embed testdata/pr1-baseline-v1.json
var approvedBaseline []byte

func baseline(data []byte) domain.RCABaseline {
	out := domain.RCABaseline{Status: "unknown", Method: "utf8_bytes_div4_v1"}
	digest := sha256.Sum256(data)
	if hex.EncodeToString(digest[:]) != "ba0455db56296343e87c94c214ed186dd60da6ca975359e178cd8fb02fbffc97" {
		return out
	}
	var numbers struct {
		Bytes  int64 `json:"bytes"`
		Tokens int64 `json:"estimatedTokens"`
	}
	if json.Unmarshal(data, &numbers) != nil {
		return out
	}
	out.Status, out.Bytes, out.EstimatedTokens = "baseline_non_request", &numbers.Bytes, &numbers.Tokens
	return out
}
