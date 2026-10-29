package domain

import (
	"bytes"
	"encoding/json"
	"os"
	"testing"
)

func TestAppDiagnosticsCollectorAllowlist(t *testing.T) {
	raw, err := os.ReadFile("../../../test/fixtures/app-diagnostics-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	var good AppDiagnosticsSnapshot
	if err := json.Unmarshal(raw, &good); err != nil {
		t.Fatal(err)
	}
	if !ValidAppDiagnosticsSnapshot(good) {
		t.Fatal("safe v1 collector rejected")
	}
	for _, change := range []string{"content", "source", "version", "oversize", "nan-name", "missing-identity"} {
		t.Run(change, func(t *testing.T) {
			var snapshot AppDiagnosticsSnapshot
			if err := json.Unmarshal(raw, &snapshot); err != nil {
				t.Fatal(err)
			}
			switch change {
			case "missing-identity":
				snapshot.Identity = nil
			case "content":
				snapshot.Collector.Errors.Records = []AppDiagnosticsErrorSummary{{At: snapshot.CapturedAt, Source: "console_error", Name: "Error", Message: "canary-secret /private/path", Count: 1}}
			case "source":
				snapshot.Collector.Route.Source = "https://private.example/token"
			case "version":
				snapshot.Collector.RedactionSchemaVersion = 2
			case "oversize":
				snapshot.Collector.Components.Entries[0].ListedIDs = make([]string, 201)
			case "nan-name":
				snapshot.Identity.BuildCommit = "/private/canary-secret"
			}
			if ValidAppDiagnosticsSnapshot(snapshot) {
				t.Fatal("unsafe collector accepted")
			}
		})
	}
	mutated := bytes.Replace(raw, []byte(`"source": "local"`), []byte(`"source": "local", "payload": "canary-secret"`), 1)
	decoder := json.NewDecoder(bytes.NewReader(mutated))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&good); err == nil {
		t.Fatal("unknown nested field accepted")
	}
}
