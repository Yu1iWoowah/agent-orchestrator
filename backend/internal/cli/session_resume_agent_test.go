package cli

import (
	"encoding/json"
	"io"
	"net/http"
	"reflect"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// resumeAgentServer accepts the resume and answers each status read with the
// next entry of statuses, repeating the last one.
func resumeAgentServer(t *testing.T, statuses ...func(http.ResponseWriter)) *sessionRequestLog {
	t.Helper()
	cfg := setConfigEnv(t)
	log := &sessionRequestLog{}
	var reads atomic.Int32
	srv := newFixtureServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log.append(r)
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/sessions/demo-1/resume-agent":
			w.WriteHeader(http.StatusAccepted)
			_, _ = io.WriteString(w, `{"ok":true,"sessionId":"demo-1","session":`+sessionJSON("demo-1", "demo", "worker", "exited", false)+`}`)
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/sessions/demo-1/resume-agent":
			i := min(int(reads.Add(1))-1, len(statuses)-1)
			statuses[i](w)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	writeRunFileFor(t, cfg, srv)
	interval, limit := resumeAgentPollInterval, resumeAgentWaitLimit
	resumeAgentPollInterval = time.Millisecond
	t.Cleanup(func() { resumeAgentPollInterval, resumeAgentWaitLimit = interval, limit })
	return log
}

func resumeStatus(state, extra string) func(http.ResponseWriter) {
	return func(w http.ResponseWriter) {
		_, _ = io.WriteString(w, `{"sessionId":"demo-1","state":"`+state+`"`+extra+`,"session":`+sessionJSON("demo-1", "demo", "worker", "idle", false)+`}`)
	}
}

func TestSessionResumeAgentPrintsTheSameJSONOnceTheResumeSucceeds(t *testing.T) {
	log := resumeAgentServer(t, resumeStatus("running", ""), resumeStatus("running", ""), resumeStatus("succeeded", `,"resumeMode":"saved_prompt"`))

	out, errOut, err := executeCLI(t, Deps{ProcessAlive: func(int) bool { return true }}, "session", "resume-agent", "demo-1", "--json")
	if err != nil {
		t.Fatalf("resume-agent: %v\nstderr=%s", err, errOut)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, out)
	}
	keys := make([]string, 0, len(got))
	for key := range got {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	if !reflect.DeepEqual(keys, []string{"ok", "resumeMode", "session", "sessionId"}) ||
		got["ok"] != true || got["sessionId"] != "demo-1" || got["resumeMode"] != "saved_prompt" {
		t.Fatalf("stdout = %s", out)
	}
	if session, _ := got["session"].(map[string]any); session["id"] != "demo-1" || session["status"] != "idle" {
		t.Fatalf("session = %#v, want the resumed session", got["session"])
	}
	if reads := strings.Count(strings.Join(log.all(), "\n"), "GET /api/v1/sessions/demo-1/resume-agent"); reads != 3 {
		t.Fatalf("status reads = %d, want 3", reads)
	}
}

func TestSessionResumeAgentFailsWithTheExistingErrorFormat(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status func(http.ResponseWriter)
		limit  time.Duration
		want   string
	}{
		{
			name:   "failed",
			status: resumeStatus("failed", `,"error":{"status":409,"code":"AGENT_RESUME_FAILED","message":"AO could not restart the agent","details":{"step":"runtime"}}`),
			want:   "AO could not restart the agent (AGENT_RESUME_FAILED)",
		},
		{
			name: "unknown resume",
			status: func(w http.ResponseWriter) {
				w.WriteHeader(http.StatusNotFound)
				_, _ = io.WriteString(w, `{"error":"not_found","code":"AGENT_RESUME_NOT_FOUND","message":"No resume of this agent is recorded since the daemon started","requestId":"req-1"}`)
			},
			want: "No resume of this agent is recorded since the daemon started (AGENT_RESUME_NOT_FOUND) [request req-1]",
		},
		{
			name:   "daemon unavailable",
			status: func(http.ResponseWriter) { panic(http.ErrAbortHandler) },
			want:   "call daemon:",
		},
		{
			name:   "wait limit",
			status: resumeStatus("running", ""),
			limit:  30 * time.Millisecond,
			want:   "agent resume for session demo-1 did not finish within 30ms; AO keeps resuming it in the background",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resumeAgentServer(t, tc.status)
			if tc.limit > 0 {
				resumeAgentWaitLimit = tc.limit
			}
			out, _, err := executeCLI(t, Deps{ProcessAlive: func(int) bool { return true }}, "session", "resume-agent", "demo-1", "--json")
			if err == nil || ExitCode(err) != 1 || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v (exit %d), want exit 1 with %q", err, ExitCode(err), tc.want)
			}
			if out != "" {
				t.Fatalf("stdout = %q, want nothing on failure", out)
			}
		})
	}
}
