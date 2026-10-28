package cli

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

const testMergePRURL = "https://github.com/owner/repo/pull/42"
const testMergeHead = "0123456789abcdef0123456789abcdef01234567"

func TestPRMergePostsToDaemon(t *testing.T) {
	for _, tt := range []struct {
		name     string
		number   string
		prURL    string
		head     string
		response string
		want     string
	}{
		{"squash", "#42", testMergePRURL, testMergeHead, `{"ok":true,"prNumber":42,"method":"squash"}`, "merged PR #42 using squash\n"},
		{"no method", "42", testMergePRURL, testMergeHead, `{"ok":true,"prNumber":42}`, "merged PR #42\n"},
		{"sha256 and gitlab", "42", "https://gitlab.example.com/group/subgroup/repo/-/merge_requests/42", strings.Repeat("aB", 32), `{"ok":true,"prNumber":42}`, "merged PR #42\n"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cfg := setConfigEnv(t)
			srv, capture := reviewServer(t, http.StatusOK, tt.response)
			writeRunFileFor(t, cfg, srv)

			out, errOut, err := executeCLI(t, aliveDeps(), "pr", "merge", tt.number, "--pr-url", tt.prURL, "--expected-head", tt.head)
			if err != nil {
				t.Fatalf("unexpected error: %v\nstderr=%s", err, errOut)
			}
			if capture.method != http.MethodPost || capture.path != "/api/v1/prs/42/merge" {
				t.Fatalf("request = %s %s", capture.method, capture.path)
			}
			var body map[string]string
			if err := json.Unmarshal([]byte(capture.body), &body); err != nil {
				t.Fatalf("decode body: %v", err)
			}
			if len(body) != 2 || body["prUrl"] != tt.prURL || body["expectedHeadSha"] != tt.head {
				t.Fatalf("body = %s, want prUrl=%q expectedHeadSha=%q", capture.body, tt.prURL, tt.head)
			}
			if out != tt.want {
				t.Fatalf("stdout = %q, want %q", out, tt.want)
			}
		})
	}
}

func TestPRMergeRejectsInvalidInput(t *testing.T) {
	validFlags := []string{"--pr-url", testMergePRURL, "--expected-head", testMergeHead}
	for _, tt := range []struct {
		name string
		args []string
		want string
	}{
		{"no number", validFlags, "accepts 1 arg"},
		{"extra number", append([]string{"42", "43"}, validFlags...), "accepts 1 arg"},
		{"zero", append([]string{"0"}, validFlags...), "positive integer"},
		{"negative", append(append([]string{}, validFlags...), "--", "-1"), "positive integer"},
		{"not a number", append([]string{"abc"}, validFlags...), "positive integer"},
		{"bare hash", append([]string{"#"}, validFlags...), "positive integer"},
		{"missing flags", []string{"42"}, "--pr-url"},
		{"missing url", []string{"42", "--expected-head", testMergeHead}, "--pr-url"},
		{"missing head", []string{"42", "--pr-url", testMergePRURL}, "--expected-head"},
		{"blank url", []string{"42", "--pr-url", " ", "--expected-head", testMergeHead}, "--pr-url"},
		{"relative url", []string{"42", "--pr-url", "owner/repo/pull/42", "--expected-head", testMergeHead}, "--pr-url"},
		{"missing host", []string{"42", "--pr-url", "https:///pull/42", "--expected-head", testMergeHead}, "--pr-url"},
		{"invalid url", []string{"42", "--pr-url", "https://%zz/pull/42", "--expected-head", testMergeHead}, "--pr-url"},
		{"number mismatch", []string{"42", "--pr-url", "https://github.com/owner/repo/pull/43", "--expected-head", testMergeHead}, "--pr-url"},
		{"not a PR url", []string{"42", "--pr-url", "https://github.com/owner/repo", "--expected-head", testMergeHead}, "--pr-url"},
		{"invalid scheme", []string{"42", "--pr-url", "file:///pull/42", "--expected-head", testMergeHead}, "--pr-url"},
		{"blank head", []string{"42", "--pr-url", testMergePRURL, "--expected-head", " "}, "--expected-head"},
		{"short head", []string{"42", "--pr-url", testMergePRURL, "--expected-head", "abcdef1"}, "--expected-head"},
		{"non hex head", []string{"42", "--pr-url", testMergePRURL, "--expected-head", strings.Repeat("z", 40)}, "--expected-head"},
		{"wrong head length", []string{"42", "--pr-url", testMergePRURL, "--expected-head", strings.Repeat("a", 41)}, "--expected-head"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cfg := setConfigEnv(t)
			srv, capture := reviewServer(t, http.StatusOK, `{"ok":true,"prNumber":42}`)
			writeRunFileFor(t, cfg, srv)
			_, _, err := executeCLI(t, aliveDeps(), append([]string{"pr", "merge"}, tt.args...)...)
			if got := ExitCode(err); got != 2 {
				t.Fatalf("exit code = %d, want 2; err=%v", got, err)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %q, want %q", err, tt.want)
			}
			if strings.HasSuffix(capture.path, "/merge") {
				t.Fatalf("invalid input sent %s %s", capture.method, capture.path)
			}
		})
	}
}

func TestPRMergeSurfacesDaemonError(t *testing.T) {
	for _, tt := range []struct {
		name    string
		status  int
		message string
		code    string
	}{
		{"head changed", http.StatusConflict, "PR head changed; refresh before merging", "PR_HEAD_CHANGED"},
		{"not mergeable", http.StatusConflict, "PR is not mergeable", "PR_NOT_MERGEABLE"},
		{"preconditions", http.StatusUnprocessableEntity, "PR merge preconditions are not met", "PR_PRECONDITIONS_UNMET"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cfg := setConfigEnv(t)
			body, err := json.Marshal(map[string]string{"message": tt.message, "code": tt.code, "requestId": "req-1"})
			if err != nil {
				t.Fatal(err)
			}
			srv, _ := reviewServer(t, tt.status, string(body))
			writeRunFileFor(t, cfg, srv)
			out, _, err := executeCLI(t, aliveDeps(), "pr", "merge", "42", "--pr-url", testMergePRURL, "--expected-head", testMergeHead)
			if got := ExitCode(err); got != 1 {
				t.Fatalf("exit code = %d, want 1; err=%v", got, err)
			}
			for _, want := range []string{tt.message, tt.code, "req-1"} {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("err = %q, want %q", err, want)
				}
			}
			if out != "" {
				t.Fatalf("stdout = %q on failed merge", out)
			}
		})
	}
}

func TestPRResolveCommentsPostsIDs(t *testing.T) {
	cfg := setConfigEnv(t)
	srv, capture := reviewServer(t, http.StatusOK, `{"ok":true,"resolved":2}`)
	writeRunFileFor(t, cfg, srv)

	out, errOut, err := executeCLI(t, aliveDeps(), "pr", "resolve-comments", "42", "thread-1", "thread-2")
	if err != nil {
		t.Fatalf("unexpected error: %v\nstderr=%s", err, errOut)
	}
	if capture.method != http.MethodPost || capture.path != "/api/v1/prs/42/resolve-comments" {
		t.Fatalf("request = %s %s", capture.method, capture.path)
	}
	var req resolveCommentsRequest
	if err := json.Unmarshal([]byte(capture.body), &req); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if len(req.CommentIDs) != 2 || req.CommentIDs[0] != "thread-1" || req.CommentIDs[1] != "thread-2" {
		t.Fatalf("comment ids = %v", req.CommentIDs)
	}
	if !strings.Contains(out, "resolved 2 review thread(s) on PR #42") {
		t.Fatalf("stdout = %q", out)
	}
}

func TestPRResolveCommentsAllowsNoIDs(t *testing.T) {
	cfg := setConfigEnv(t)
	srv, capture := reviewServer(t, http.StatusOK, `{"ok":true,"resolved":3}`)
	writeRunFileFor(t, cfg, srv)

	if _, errOut, err := executeCLI(t, aliveDeps(), "pr", "resolve-comments", "42"); err != nil {
		t.Fatalf("unexpected error: %v\nstderr=%s", err, errOut)
	}
	if strings.TrimSpace(capture.body) != "{}" {
		t.Fatalf("body = %q, want {}", capture.body)
	}
}

func TestPRResolveCommentsRequiresPRNumber(t *testing.T) {
	setConfigEnv(t)

	_, _, err := executeCLI(t, aliveDeps(), "pr", "resolve-comments")
	if got := ExitCode(err); got != 2 {
		t.Fatalf("exit code = %d, want 2; err=%v", got, err)
	}
}
