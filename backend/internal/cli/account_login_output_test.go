package cli

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func assertPrivateLoginStream(t *testing.T, line string) {
	t.Helper()
	const prompt = "\r\n\r\nPaste authorization code: "
	check := func(out *bytes.Buffer) {
		t.Helper()
		if strings.Contains(out.String(), "~") {
			t.Fatal("credential first byte escaped")
		}
	}
	var bytesOut bytes.Buffer
	var bytesOutput accountLoginOutput
	for i := range len(line) {
		bytesOutput.write(&bytesOut, []byte(line[i:i+1]))
		check(&bytesOut)
	}
	bytesOutput.write(&bytesOut, []byte(prompt))
	check(&bytesOut)
	if !strings.Contains(bytesOut.String(), "Paste authorization code: ") {
		t.Fatal("interactive prompt remained hidden after a definite boundary")
	}
	for split := 0; split <= len(line); split++ {
		var out bytes.Buffer
		var output accountLoginOutput
		output.write(&out, []byte(line[:split]))
		check(&out)
		output.write(&out, []byte(line[split:]))
		check(&out)
		output.write(&out, []byte(prompt))
		check(&out)
		if !strings.Contains(out.String(), "Paste authorization code: ") {
			t.Fatalf("prompt lost at split %d", split)
		}
	}
}

func TestLoginOutputAuditD3OriginalCounterexamples(t *testing.T) {
	const value = "~NEW_D3_AUDIT_CANARY_846291"
	for name, line := range map[string]string{
		"kelvin-token":         "headers[ToKen]=" + value + "\r\n",
		"kelvin-api-key":       "request.headers.APIKey=" + value + "\r\n",
		"dotted-capital-i":     "headers[Authorİzation]=" + value + "\r\n",
		"fold-basic":           "Authorization: Basic\r\n " + value + "\r\n",
		"fold-empty":           "Authorization:\r\n\t" + value + "\r\n",
		"json-value-next-line": "{\"headers\":{\"Authorization\":\n \"" + value + "\"}}\r\n",
	} {
		t.Run(name, func(t *testing.T) {
			assertPrivateLoginStream(t, line)
			t.Run("pty-onlcr", func(t *testing.T) {
				assertPrivateLoginStream(t, strings.ReplaceAll(line, "\n", "\r\n"))
			})
		})
	}
}

func TestLoginOutputUnicodeAndMultilineMatrix(t *testing.T) {
	const value = "~UNICODE_MULTILINE_PRIVATE_CANARY"
	names := []string{"token", "ToKen", "ToKen", "APIKey", "Authorİzation", "Ａｕｔｈ", "Ｃｏｏｋｉｅ", "to\u200bken", "sｅssion", "未知欄位", `\u0074oken`}
	forms := []struct {
		name string
		line func(string) string
	}{
		{"assignment", func(k string) string { return "headers[" + k + "]=" + value + "\r\n" }},
		{"header-crlf", func(k string) string { return k + ": Basic\r\n " + value + "\r\n" }},
		{"header-empty", func(k string) string { return k + ":\r\n\t" + value + "\r\n" }},
		{"header-lf", func(k string) string { return k + ": Basic\n\t" + value + "\n" }},
		{"header-cr", func(k string) string { return k + ": Basic\r " + value + "\r" }},
		{"json-key-newline", func(k string) string { return fmt.Sprintf("{\"%s\"\n: \"%s\"}\r\n", k, value) }},
		{"json-newline", func(k string) string { return fmt.Sprintf("{\"headers\":{\"%s\":\n \"%s\"}}\r\n", k, value) }},
		{"json-whitespace", func(k string) string { return fmt.Sprintf("{\"%s\":\r\n\r\n \"%s\"}\r\n", k, value) }},
		{"json-multiline-string", func(k string) string { return fmt.Sprintf("{\"%s\":\"prefix\n%s\"}\r\n", k, value) }},
		{"json-cr-string", func(k string) string { return fmt.Sprintf("{\"%s\":\"prefix\r%s\"}\r", k, value) }},
		{"json-escaped-quote", func(k string) string { return fmt.Sprintf("{\"%s\":\"prefix\\\"%s\"}\r\n", k, value) }},
		{"json-object", func(k string) string { return fmt.Sprintf("{\"%s\":\n{\"value\":\"%s\"}}\r\n", k, value) }},
		{"json-array", func(k string) string {
			return fmt.Sprintf("{\"%s\":\n[\"%s\",{\"data\":\"%s\"}]}\r\n", k, value, value)
		}},
	}
	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			root, sensitive := accountCredentialKey(name)
			if !root || !sensitive {
				t.Fatal("Unicode/unknown name was not classified conservatively")
			}
			for _, form := range forms {
				t.Run(form.name, func(t *testing.T) { assertPrivateLoginStream(t, form.line(name)) })
			}
		})
	}
}

func TestLoginOutputUncertainContinuationAndPromptRecovery(t *testing.T) {
	var out bytes.Buffer
	var output accountLoginOutput
	output.write(&out, []byte("Authorization: Basic\r\n ~PRIVATE_CANARY\r\nUnlabelled ~MORE_PRIVATE\r\n"))
	if strings.Contains(out.String(), "~") {
		t.Fatal("unknown continuation escaped")
	}
	const prompt = "Paste authorization code: "
	output.write(&out, []byte(prompt))
	if !strings.HasSuffix(out.String(), prompt) {
		t.Fatal("recognized prompt did not recover without a newline")
	}
	output.write(&out, []byte("ABCD-1234"))
	if !strings.HasSuffix(out.String(), prompt+"ABCD-1234") {
		t.Fatal("prompt input echo was delayed")
	}
}

func TestLoginOutputJSONDoesNotRecoverInsideString(t *testing.T) {
	var out bytes.Buffer
	var output accountLoginOutput
	output.write(&out, []byte("{\"Authorization\":\"private\n\nPaste authorization code: ~STILL_PRIVATE"))
	if strings.Contains(out.String(), "~") || strings.Contains(out.String(), "Paste authorization") {
		t.Fatal("JSON value recovered before its closing quote")
	}
	output.write(&out, []byte("\"}\r\nPaste authorization code: "))
	if !strings.HasSuffix(out.String(), "Paste authorization code: ") {
		t.Fatal("prompt did not recover after JSON string closed")
	}
}

func TestLoginOutputUnknownNameCannotClearSensitiveState(t *testing.T) {
	for _, punctuation := range "{},?&;" {
		name := "ToK" + string(punctuation) + "en"
		for _, line := range []string{
			name + "=~PRIVATE_NAME_CANARY\r\n",
			"headers[" + name + "]=~PRIVATE_NAME_CANARY\r\n",
			fmt.Sprintf("{\"%s\":\"~PRIVATE_NAME_CANARY\"}\r\n", name),
		} {
			t.Run(line, func(t *testing.T) { assertPrivateLoginStream(t, line) })
		}
	}
}

func TestCompletedLegacyConflictHasFreshAddGuidance(t *testing.T) {
	cfg := setConfigEnv(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/telemetry/") {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if r.URL.Path != "/api/v1/agents/accounts/operations/op" {
			t.Errorf("unexpected request %s", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		_, _ = io.WriteString(w, `{"operationId":"op","action":"login","provider":"codex","phase":"complete","outcome":"legacy-conflict"}`)
	}))
	defer srv.Close()
	writeRunFileFor(t, cfg, srv)
	_, _, err := executeCLI(t, Deps{ProcessAlive: func(int) bool { return true }}, "account", "login", "--operation", "op", "--no-attach")
	if err == nil || ExitCode(err) != 1 || !strings.Contains(err.Error(), "Settings > Accounts") || !strings.Contains(err.Error(), "ao account add --provider codex") || strings.Contains(err.Error(), "retained") {
		t.Fatalf("completed conflict guidance missing: %v", err)
	}
}

func TestLoginOutputReadablePromptsAfterConcealment(t *testing.T) {
	for _, prompt := range []string{
		"Open https://example.test/device?user_code=ABCD-1234",
		"Authentication URL: https://example.test/device",
		"Paste authorization code: ",
		"Enter code: ",
		"Please enter the code: ",
		"Session status: ready",
		"Keyboard shortcuts: Enter to continue",
		"請輸入授權碼：",
		"Go to https://example.test/device",
	} {
		t.Run(prompt, func(t *testing.T) {
			var out bytes.Buffer
			var output accountLoginOutput
			output.write(&out, []byte("Authorization: Basic\r\n ~PRIVATE_VALUE\r\n"))
			output.write(&out, []byte(prompt))
			if strings.Contains(out.String(), "~") || !strings.HasSuffix(out.String(), prompt) {
				t.Fatal("safe prompt lost or previous value escaped")
			}
		})
	}
}
