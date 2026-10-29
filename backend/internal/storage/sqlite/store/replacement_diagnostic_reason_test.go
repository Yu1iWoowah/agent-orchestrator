package store_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestReplacementDiagnosticExplainsUnknownFieldWithoutChangingFacts(t *testing.T) {
	st, api, op, raw := repairFixture(t)
	if _, err := raw.Exec(`UPDATE orchestrator_replacements SET state_json=json_set(state_json,'$.futureNativeStop',json('{"stopped":true}')) WHERE id=?`, op.ID); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	before, found, err := api.DiagnoseOrchestratorReplacement(ctx, op.ID)
	if err != nil || !found || !before.Corrupt {
		t.Fatal(found, err)
	}
	data, err := json.Marshal(before)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatal(err)
	}
	reason, _ := fields["corruptionReason"].(string)
	if !strings.Contains(reason, "unknown field") || !strings.Contains(reason, "futureNativeStop") {
		t.Errorf("diagnostic hides the incompatible field: reason=%q", reason)
	}
	after, _, err := api.DiagnoseOrchestratorReplacement(ctx, op.ID)
	if err != nil || after.RawDigest != before.RawDigest || after.Revision != before.Revision {
		t.Fatal("diagnosis changed raw facts", err)
	}
	if fenced, err := st.ReplacementWakeFenced(ctx, op.SourceSessionID); err != nil || !fenced {
		t.Fatal("diagnosis released an unverified source", fenced, err)
	}
}
