package domain

import (
	"encoding/json"
	"os"
	"strconv"
	"testing"
)

// Required paths come from every object member in the comprehensive TS-validated
// fixture, including array entries, styles/ancestors and false/zero values.
func diagnosticFieldPaths(value any, path []string) [][]string {
	var out [][]string
	switch value := value.(type) {
	case map[string]any:
		for key, item := range value {
			next := append(append([]string{}, path...), key)
			out = append(out, next)
			out = append(out, diagnosticFieldPaths(item, next)...)
		}
	case []any:
		for i, item := range value {
			out = append(out, diagnosticFieldPaths(item, append(append([]string{}, path...), strconv.Itoa(i)))...)
		}
	}
	return out
}
func diagnosticFixtureMember(t *testing.T, root any, path []string) (map[string]any, string) {
	t.Helper()
	value := root
	for _, part := range path[:len(path)-1] {
		switch current := value.(type) {
		case map[string]any:
			value = current[part]
		case []any:
			index, err := strconv.Atoi(part)
			if err != nil {
				t.Fatal(err)
			}
			value = current[index]
		}
	}
	return value.(map[string]any), path[len(path)-1]
}
func TestLiveSnapshotRequiresEveryJSONField(t *testing.T) {
	raw, err := os.ReadFile("../../../test/fixtures/app-diagnostics-complete-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		t.Fatal(err)
	}
	var good AppDiagnosticsSnapshot
	if err := json.Unmarshal(raw, &good); err != nil || !ValidAppDiagnosticsCaptureSnapshot(good) {
		t.Fatalf("valid complete fixture rejected: %v", err)
	}
	for _, path := range diagnosticFieldPaths(value, nil) {
		for _, mode := range []string{"missing", "null"} {
			name := mode
			for _, part := range path {
				name += "/" + part
			}
			t.Run(name, func(t *testing.T) {
				var mutated any
				if err := json.Unmarshal(raw, &mutated); err != nil {
					t.Fatal(err)
				}
				parent, key := diagnosticFixtureMember(t, mutated, path)
				if mode == "missing" {
					delete(parent, key)
				} else {
					parent[key] = nil
				}
				data, err := json.Marshal(mutated)
				if err != nil {
					t.Fatal(err)
				}
				var snapshot AppDiagnosticsSnapshot
				if err := json.Unmarshal(data, &snapshot); err == nil && ValidAppDiagnosticsCaptureSnapshot(snapshot) {
					t.Fatal("required field omission/null accepted as a live snapshot")
				}
			})
		}
	}
	t.Run("optional-style", func(t *testing.T) {
		node := value.(map[string]any)["collector"].(map[string]any)["geometry"].(map[string]any)["nodes"].([]any)[0].(map[string]any)
		node["availability"] = "not_mounted"
		delete(node, "style")
		data, _ := json.Marshal(value)
		var snapshot AppDiagnosticsSnapshot
		if err := json.Unmarshal(data, &snapshot); err != nil || !ValidAppDiagnosticsCaptureSnapshot(snapshot) {
			t.Fatalf("optional unavailable style rejected: %v", err)
		}
	})
}

func TestLiveSnapshotRejectsJSONFieldCaseAliases(t *testing.T) {
	raw, err := os.ReadFile("../../../test/fixtures/app-diagnostics-complete-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	var value map[string]any
	if err := json.Unmarshal(raw, &value); err != nil {
		t.Fatal(err)
	}
	node := value["collector"].(map[string]any)["geometry"].(map[string]any)["nodes"].([]any)[0].(map[string]any)
	node["availability"] = "not_mounted"
	node["Style"] = node["style"]
	delete(node, "style")
	raw, err = json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	var snapshot AppDiagnosticsSnapshot
	if err := json.Unmarshal(raw, &snapshot); err == nil && ValidAppDiagnosticsCaptureSnapshot(snapshot) {
		t.Fatal("case-insensitive optional field bypassed the exact wire contract")
	}
}
