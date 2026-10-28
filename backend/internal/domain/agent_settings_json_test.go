package domain

import (
	"encoding/json"
	"testing"
)

func TestAgentSettingsPatchJSONPreservesOmittedAndProviderReset(t *testing.T) {
	for _, tc := range []struct {
		name, raw, wantJSON string
		model, effort       *string
	}{
		{name: "omitted", raw: `{}`, wantJSON: `{}`},
		{name: "model reset", raw: `{"model":null}`, wantJSON: `{"model":null}`, model: new("")},
		{name: "effort reset", raw: `{"effort":null}`, wantJSON: `{"effort":null}`, effort: new("")},
		{name: "explicit choices", raw: `{"model":"model","effort":"high"}`, wantJSON: `{"effort":"high","model":"model"}`, model: new("model"), effort: new("high")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var patch AgentSettingsPatch
			if err := json.Unmarshal([]byte(tc.raw), &patch); err != nil {
				t.Fatal(err)
			}
			if (patch.Model == nil) != (tc.model == nil) || (patch.Effort == nil) != (tc.effort == nil) {
				t.Fatalf("lost field presence: %+v", patch)
			}
			if tc.model != nil && string(*patch.Model) != *tc.model || tc.effort != nil && string(*patch.Effort) != *tc.effort {
				t.Fatalf("wrong selection: %+v", patch)
			}
			raw, err := json.Marshal(patch)
			if err != nil || string(raw) != tc.wantJSON {
				t.Fatalf("wire=%s err=%v want=%s", raw, err, tc.wantJSON)
			}
		})
	}
}

func TestAgentSettingsPatchJSONRejectsInvalidSettings(t *testing.T) {
	for _, raw := range []string{`{"fastMode":null}`, `{"fastMode":"turbo"}`, `{"fastMode":true}`, `{"model":1}`, `{"effort":" high"}`, `{"unknown":"on"}`, `[]`} {
		t.Run(raw, func(t *testing.T) {
			var patch AgentSettingsPatch
			if err := json.Unmarshal([]byte(raw), &patch); err == nil {
				t.Fatalf("invalid patch accepted: %+v", patch)
			}
		})
	}
}
