package delegate

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestPALMissingCriticalScopeNeverDispatches(t *testing.T) {
	if runtime.GOOS != "linux" || os.Getenv("HCG_PAL_TESTS") != "1" {
		t.Skip("isolated PAL required")
	}
	for _, provider := range []string{"openai", "anthropic"} {
		for _, reason := range []string{"one_file_too_large", "total_too_large", "pal_skips_critical_b", "sensitive_question"} {
			t.Run(provider+"/"+reason, func(t *testing.T) {
				root := t.TempDir()
				paths := []string{}
				bodies := []string{"critical A\n", strings.Repeat("b", 60000) + "\n"}
				switch reason {
				case "one_file_too_large":
					bodies[1] = strings.Repeat("b", maxSourceBytes+1)
				case "total_too_large":
					bodies = []string{strings.Repeat("a", 60000), strings.Repeat("b", 60000), strings.Repeat("c", 60000)}
				}
				for i, body := range bodies {
					path := filepath.Join(root, string(rune('A'+i))+".txt")
					if err := os.WriteFile(path, []byte(body), 0600); err != nil {
						t.Fatal(err)
					}
					paths = append(paths, path)
				}
				calls := 0
				c := Consumer{PAL: PAL{Python: os.Getenv("HCG_TEST_PAL_PYTHON"), Source: os.Getenv("HCG_TEST_PAL_SOURCE"), TempRoot: root}, Recorder: &memoryRecorder{}, Transport: transportFunc(func(context.Context, WireRequest) (WireResponse, error) { calls++; return WireResponse{}, nil })}
				question := "Find the difference between critical A and B"
				if reason == "sensitive_question" {
					question = "HCG_CANARY_DO_NOT_SEND"
				}
				request := approvedRequest(t, &c, question, paths...)
				p := Profile{ID: provider + "-fixture", Revision: "r1", BillingProvider: provider, Model: "fixture-model", ContextTokens: 8192, MaxOutputTokens: 4096, OutputTokens: 128}
				result, err := c.DelegateRead(t.Context(), testExecution, p, request)
				var scopeErr *ScopeError
				if !errors.As(err, &scopeErr) || result.text != "" || calls != 0 {
					t.Fatalf("missing scope accepted: %+v %v calls=%d", result, err, calls)
				}
				if reason == "one_file_too_large" || reason == "total_too_large" {
					if scopeErr.Status != "scope_too_large" {
						t.Fatalf("wrong size failure: %v", err)
					}
				}
				if reason == "pal_skips_critical_b" && (result.Audit.Coverage != "none" || len(result.Audit.Omissions) != 2) {
					t.Fatalf("missing B not disclosed: %+v", result.Audit)
				}
			})
		}
	}
}

func TestPALLineNumberFormat(t *testing.T) {
	if got := numbered("a\nb\n"); got != "   1│ a\n   2│ b\n   3│ " {
		t.Fatalf("fixed PAL minimum width changed: %q", got)
	}
	if got := numbered(strings.Repeat("\n", 10000)); !strings.HasPrefix(got, "    1│ ") || !strings.HasSuffix(got, "10001│ ") {
		t.Fatal("PAL dynamic width changed")
	}
}
