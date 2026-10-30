package cli

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const replacementTestID = "11111111-1111-4111-8111-111111111111"

func TestReplacementRepairCLIRequiresExplicitCAS(t *testing.T) {
	cfg := setConfigEnv(t)
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/internal/") {
			return
		}
		calls++
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/orchestrator/replacements/"+replacementTestID+"/repair" {
			t.Error("wrong repair route")
		}
		var body map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || string(body["expectedRevision"]) != "8" || string(body["snapshotRevision"]) != "7" || len(body) != 4 {
			t.Error("explicit repair CAS lost", body, err)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"operation":{"id":%q,"phase":"recovery_required"}}`, replacementTestID)
	}))
	defer srv.Close()
	writeRunFileFor(t, cfg, srv)
	args := []string{"orchestrator", "replacement", "repair", replacementTestID, "--expected-revision", "8", "--expected-digest", strings.Repeat("a", 64), "--snapshot-revision", "7", "--snapshot-digest", strings.Repeat("b", 64), "--json"}
	out, stderr, err := executeCLI(t, Deps{ProcessAlive: func(int) bool { return true }}, args...)
	if err != nil || calls != 1 || !strings.Contains(out, "recovery_required") {
		t.Fatal("repair request unavailable", err, stderr, calls)
	}
	_, _, err = executeCLI(t, Deps{}, "orchestrator", "replacement", "repair", replacementTestID)
	if ExitCode(err) != 2 || calls != 1 {
		t.Fatal("repair missing CAS reached daemon", err, calls)
	}
}

func TestOrchestratorReplacementHTTPCommands(t *testing.T) {
	for _, action := range []string{"replace", "get", "retry", "cancel"} {
		t.Run(action, func(t *testing.T) {
			cfg := setConfigEnv(t)
			calls := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasPrefix(r.URL.Path, "/internal/") {
					return
				}
				calls++
				path := "/api/v1/orchestrator/replacements/" + replacementTestID
				method := http.MethodGet
				switch action {
				case "replace":
					path, method = "/api/v1/projects/demo/orchestrator/replacements", http.MethodPost
					var in map[string]string
					if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
						t.Error(err)
					}
					if in["sourceSessionId"] != "source" || in["operationKey"] != replacementTestID || len(in) != 2 {
						t.Errorf("replacement request = %#v", in)
					}
				case "retry", "cancel":
					path, method = path+"/"+action, http.MethodPost
				}
				if r.Method != method || r.URL.Path != path {
					t.Errorf("request = %s %s, want %s %s", r.Method, r.URL.Path, method, path)
				}
				w.Header().Set("Content-Type", "application/json")
				if action != "get" {
					w.WriteHeader(http.StatusAccepted)
				}
				fmt.Fprintf(w, `{"operation":{"id":%q,"phase":"accepted","sourceSessionId":"source","resumeEvidence":{"verified":false},"notificationReceipts":[],"error":{"code":"WAITING_SOURCE_IDLE","message":"waiting"}}}`, replacementTestID)
			}))
			defer srv.Close()
			writeRunFileFor(t, cfg, srv)
			args := []string{"orchestrator", "replacement", action, replacementTestID, "--json"}
			if action == "replace" {
				args = []string{"orchestrator", "replace", "--project", "demo", "--from", "source", "--operation-key", replacementTestID, "--json"}
			}
			out, stderr, err := executeCLI(t, Deps{ProcessAlive: func(int) bool { return true }}, args...)
			if err != nil {
				t.Fatalf("%s failed: %v; stderr=%s", action, err, stderr)
			}
			var got struct {
				Operation map[string]json.RawMessage `json:"operation"`
			}
			if err := json.Unmarshal([]byte(out), &got); err != nil {
				t.Fatalf("not operation JSON: %v / %s", err, out)
			}
			if string(got.Operation["id"]) != `"`+replacementTestID+`"` || got.Operation["resumeEvidence"] == nil || got.Operation["notificationReceipts"] == nil || got.Operation["error"] == nil || calls != 1 {
				t.Fatalf("durable facts lost or extra requests: %s; calls=%d", out, calls)
			}
		})
	}
}

func TestOrchestratorReplacementKeepsDaemonError(t *testing.T) {
	cfg := setConfigEnv(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/internal/") {
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		fmt.Fprint(w, `{"code":"ORCHESTRATOR_HANDOFF_SOURCE_UNSUPPORTED","message":"source must use Claude Code","requestId":"replacement-request"}`)
	}))
	defer srv.Close()
	writeRunFileFor(t, cfg, srv)
	_, stderr, err := executeCLI(t, Deps{ProcessAlive: func(int) bool { return true }}, "orchestrator", "replace", "--project", "demo", "--from", "source")
	if err == nil || !strings.Contains(err.Error(), "ORCHESTRATOR_HANDOFF_SOURCE_UNSUPPORTED") || !strings.Contains(err.Error(), "replacement-request") {
		t.Fatalf("daemon error envelope lost: %v; stderr=%s", err, stderr)
	}
	if ExitCode(err) != 1 {
		t.Fatalf("daemon refusal exit code=%d", ExitCode(err))
	}
}

func TestOrchestratorReplacementFailurePreservesRecoveryIdentities(t *testing.T) {
	for _, code := range []string{"ORCHESTRATOR_REPLACEMENT_IN_PROGRESS", "ORCHESTRATOR_REPLACEMENT_KEY_CONFLICT", "ORCHESTRATOR_HANDOFF_SOURCE_UNSUPPORTED"} {
		t.Run(code, func(t *testing.T) {
			cfg := setConfigEnv(t)
			var key string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasPrefix(r.URL.Path, "/internal/") {
					return
				}
				var in map[string]string
				if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
					t.Error(err)
				}
				key = in["operationKey"]
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusConflict)
				fmt.Fprintf(w, `{"code":%q,"message":"replacement refused","requestId":"request-trace","details":{"operationId":%q,"capability":"must-not-leak"}}`, code, replacementTestID)
			}))
			defer srv.Close()
			writeRunFileFor(t, cfg, srv)
			out, stderr, err := executeCLI(t, Deps{ProcessAlive: func(int) bool { return true }}, "orchestrator", "replace", "--project", "demo", "--from", "source", "--json")
			if err == nil || !strings.Contains(err.Error(), replacementTestID) || !strings.Contains(err.Error(), code) || !strings.Contains(err.Error(), "request-trace") {
				t.Fatal("operation identity missing from refusal", err, stderr)
			}
			if key == "" || !strings.Contains(stderr, key) || out != "" || strings.Contains(err.Error(), "must-not-leak") {
				t.Fatal("replay key unavailable or private details leaked", key, out, stderr, err)
			}
		})
	}
}

func TestOrchestratorReplacementUsage(t *testing.T) {
	for _, args := range [][]string{
		{"replace"}, {"replace", "--project", "demo"}, {"replace", "--from", "source"},
		{"replace", "--project", "demo", "--from", "source", "--operation-key", "not-a-uuid"},
		{"replacement", "get"}, {"replacement", "get", "../source"},
		{"replacement", "retry", "00000000-0000-0000-0000-000000000000"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			setConfigEnv(t)
			_, _, err := executeCLI(t, Deps{}, append([]string{"orchestrator"}, args...)...)
			if ExitCode(err) != 2 {
				t.Fatalf("misuse should exit 2 before looking up daemon: %v", err)
			}
		})
	}
}

func TestOrchestratorReplacementAcceptanceReceipt(t *testing.T) {
	cfg := setConfigEnv(t)
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/internal/") {
			return
		}
		calls++
		var in map[string]string
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			t.Error(err)
		}
		if in["operationKey"] == "" {
			t.Error("CLI omitted idempotency key")
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		fmt.Fprintf(w, `{"operation":{"id":%q,"phase":"accepted"}}`, replacementTestID)
	}))
	defer srv.Close()
	writeRunFileFor(t, cfg, srv)
	out, _, err := executeCLI(t, Deps{ProcessAlive: func(int) bool { return true }}, "orchestrator", "replace", "--project", "demo", "--from", "source")
	if err != nil || calls != 1 || !strings.Contains(out, "accepted") || !strings.Contains(out, "replacement get "+replacementTestID+" --json") || strings.Contains(out, "completed") {
		t.Fatalf("acceptance should return immediately with a read command: %q err=%v calls=%d", out, err, calls)
	}
}

func TestOrchestratorPreparedSubmitsOnlyAuthenticatedReceipt(t *testing.T) {
	cfg := setConfigEnv(t)
	t.Setenv("AO_SESSION_ID", "source")
	t.Setenv("AO_BROWSER_CAPABILITY", "private-source-capability")
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/internal/") {
			return
		}
		calls++
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/orchestrator/replacements/"+replacementTestID+"/prepared" {
			t.Error("unexpected write/lease call", r.Method, r.URL.Path)
		}
		var in struct {
			Actor          projectDocumentActor `json:"actor"`
			AttemptID      string               `json:"attemptId"`
			Nonce          string               `json:"nonce"`
			Path           string               `json:"path"`
			Hash           string               `json:"hash"`
			WriteReceiptID string               `json:"writeReceiptId"`
		}
		decoder := json.NewDecoder(r.Body)
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&in); err != nil {
			t.Error(err)
		}
		if in.Actor.SessionID != "source" || in.Actor.Capability != "private-source-capability" || in.AttemptID != replacementTestID || in.Nonce != replacementTestID || in.Path != "handoff/HANDOFF.md" || in.Hash != strings.Repeat("a", 64) || in.WriteReceiptID != "managed-write-receipt" {
			t.Error("receipt changed", in.AttemptID, in.Nonce, in.Path, in.Hash, in.WriteReceiptID)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		fmt.Fprintf(w, `{"operation":{"id":%q,"phase":"preparing_handoff","preparation":{"submittedHash":"handoff-sha"}}}`, replacementTestID)
	}))
	defer srv.Close()
	writeRunFileFor(t, cfg, srv)
	out, stderr, err := executeCLI(t, Deps{ProcessAlive: func(int) bool { return true }}, "orchestrator", "replacement", "prepared", replacementTestID, "--attempt", replacementTestID, "--nonce", replacementTestID, "--path", "handoff/HANDOFF.md", "--hash", strings.Repeat("a", 64), "--write-receipt-id", "managed-write-receipt", "--json")
	if err != nil || calls != 1 || !strings.Contains(out, "submittedHash") || strings.Contains(out+stderr, "private-source-capability") {
		t.Fatal("authenticated receipt failed", calls, err, stderr)
	}
}

func TestOrchestratorPreparedRejectsInvalidReceiptLocally(t *testing.T) {
	for _, change := range []string{"missing capability", "bad attempt", "bad nonce", "bad hash", "missing receipt"} {
		t.Run(change, func(t *testing.T) {
			cfg := setConfigEnv(t)
			t.Setenv("AO_SESSION_ID", "source")
			t.Setenv("AO_BROWSER_CAPABILITY", "private-source-capability")
			calls := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if !strings.HasPrefix(r.URL.Path, "/internal/") {
					calls++
				}
			}))
			defer srv.Close()
			writeRunFileFor(t, cfg, srv)
			attempt, nonce, hash, receipt := replacementTestID, replacementTestID, strings.Repeat("a", 64), "receipt"
			switch change {
			case "missing capability":
				t.Setenv("AO_BROWSER_CAPABILITY", "")
			case "bad attempt":
				attempt = "invalid"
			case "bad nonce":
				nonce = "invalid"
			case "bad hash":
				hash = "invalid"
			case "missing receipt":
				receipt = ""
			}
			_, _, err := executeCLI(t, Deps{ProcessAlive: func(int) bool { return true }}, "orchestrator", "replacement", "prepared", replacementTestID, "--attempt", attempt, "--nonce", nonce, "--path", "handoff/HANDOFF.md", "--hash", hash, "--write-receipt-id", receipt)
			if err == nil || ExitCode(err) != 2 || calls != 0 {
				t.Fatal("invalid receipt sent", ExitCode(err), calls, err)
			}
		})
	}
}

func TestOrchestratorReplacementIntakeReadsOnceWithoutSending(t *testing.T) {
	cfg := setConfigEnv(t)
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/internal/") {
			return
		}
		calls++
		if r.Method != http.MethodGet || r.URL.Path != "/api/v1/orchestrator/replacements/"+replacementTestID+"/intake" {
			t.Errorf("unexpected mutation/read: %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"intake":{"operationId":%q,"messages":[{"id":"original-message","deliveryContentJson":"[{\"type\":\"image\",\"reference\":\"original.png\"}]","nativeDelivery":"unconfirmed"}],"reports":[{"id":"worker-Q23","note":"Q23: boundary?","deliveryState":"pending"}]}}`, replacementTestID)
	}))
	defer srv.Close()
	writeRunFileFor(t, cfg, srv)
	out, stderr, err := executeCLI(t, Deps{ProcessAlive: func(int) bool { return true }}, "orchestrator", "replacement", "intake", replacementTestID, "--json")
	if err != nil {
		t.Fatal("pending intake CLI unavailable", err, stderr)
	}
	if calls != 1 || !strings.Contains(out, "original.png") || !strings.Contains(out, "worker-Q23") || !strings.Contains(out, "unconfirmed") {
		t.Fatal("pending receipt/payload lost or mutation sent", calls, out)
	}
}
