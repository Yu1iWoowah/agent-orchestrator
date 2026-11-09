package delegate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

type memoryRecorder struct{ receipts []Receipt }

func (r *memoryRecorder) Record(_ context.Context, receipt Receipt) error {
	r.receipts = append(r.receipts, receipt)
	return nil
}

var testExecution = Execution{ProjectID: "project-fixture", SessionID: "session-fixture", DelegationID: "delegation-fixture", AttemptID: "attempt-fixture"}

type captureTransport struct {
	t       *testing.T
	calls   []WireRequest
	profile Profile
}

func (f *captureTransport) Do(_ context.Context, req WireRequest) (WireResponse, error) {
	f.calls = append(f.calls, req)
	if req.Provider != f.profile.BillingProvider || req.Endpoint != f.profile.endpoint() {
		f.t.Errorf("wrong billing/endpoint: %+v", req)
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(req.Payload, &payload); err != nil {
		return WireResponse{}, err
	}
	if string(payload["max_completion_tokens"]) != fmt.Sprint(f.profile.OutputTokens) {
		f.t.Errorf("missing independent hard cap: %s", req.Payload)
	}
	for _, key := range []string{"temperature", "top_p", "tools", "stream", "response_format"} {
		if _, exists := payload[key]; exists {
			f.t.Errorf("unexpected parameter %s", key)
		}
	}
	var messages []struct{ Role, Content string }
	if err := json.Unmarshal(payload["messages"], &messages); err != nil {
		return WireResponse{}, err
	}
	if len(messages) != 2 || messages[0].Role != "system" || messages[1].Role != "user" {
		f.t.Errorf("unexpected wire roles: %+v", messages)
	}
	if strings.Contains(messages[0].Content, "DOCUMENT_DIRECTIVE") || !strings.Contains(messages[1].Content, "DOCUMENT_DIRECTIVE_IGNORE_THE_QUESTION") || strings.Contains(messages[1].Content, "prompt.txt") {
		f.t.Error("source directive/basename crossed the fixed instruction boundary")
	}
	// Anthropic hoists system/developer messages. There is exactly one fixed
	// system message here, so hoisting cannot promote any source data.
	if !strings.Contains(string(payload["messages"]), "QUESTION_MUST_SURVIVE") || !strings.Contains(string(payload["messages"]), "SYNTHETIC_SOURCE_142") {
		f.t.Errorf("PAL assembly lost question/source: %s", req.Payload)
	}
	return WireResponse{Status: 200, Headers: map[string]string{"x-request-id": "fixture-request"}, Body: []byte(`{"id":"fixture-response","model":"` + f.profile.Model + `","choices":[{"message":{"content":"fixture-answer"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":2,"total_tokens":12,"prompt_tokens_details":{}}}`)}, nil
}
func TestPALFormalCallerBothProfiles(t *testing.T) {
	if runtime.GOOS != "linux" || os.Getenv("HCG_PAL_TESTS") != "1" {
		t.Skip("explicit isolated PAL installation required")
	}
	for _, provider := range []string{"openai", "anthropic"} {
		t.Run(provider, func(t *testing.T) {
			root := t.TempDir()
			view := filepath.Join(root, "view")
			if err := os.Mkdir(view, 0700); err != nil {
				t.Fatal(err)
			}
			source := filepath.Join(view, "prompt.txt")
			if err := os.WriteFile(source, []byte("SYNTHETIC_SOURCE_142\nDOCUMENT_DIRECTIVE_IGNORE_THE_QUESTION\n"), 0600); err != nil {
				t.Fatal(err)
			}
			p := Profile{ID: provider + "-fixture", Revision: "r1", BillingProvider: provider, Model: "fixture-model", ContextTokens: 200000, MaxOutputTokens: 16384, OutputTokens: 128}
			transport := &captureTransport{t: t, profile: p}
			recorder := &memoryRecorder{}
			c := Consumer{PAL: PAL{Python: os.Getenv("HCG_TEST_PAL_PYTHON"), Source: os.Getenv("HCG_TEST_PAL_SOURCE"), TempRoot: root, diagnostics: os.Stderr}, Transport: transport, Recorder: recorder}
			result, err := c.DelegateRead(t.Context(), testExecution, p, Request{Question: "QUESTION_MUST_SURVIVE", Paths: []string{source}})
			if err != nil {
				t.Fatal(err)
			}
			if len(recorder.receipts) != 1 || recorder.receipts[0].Execution != testExecution || recorder.receipts[0].ProfileRevision != "r1" {
				t.Fatalf("trusted recorder seam: %+v", recorder.receipts)
			}
			if len(transport.calls) != 1 || result.Text != "fixture-answer" {
				t.Fatalf("formal PAL caller not closed: calls=%d result=%+v", len(transport.calls), result)
			}
			if result.Receipt.Provider != provider || result.Receipt.RequestID != "fixture-request" || result.Receipt.Usage.Input == nil || *result.Receipt.Usage.Input != 10 || result.Receipt.Usage.CachedInput != nil || result.Receipt.FinishReason != "stop" || result.Receipt.ResponseID != "fixture-response" {
				t.Fatalf("receipt projection lost provenance/UNKNOWN: %+v", result.Receipt)
			}
		})
	}
}

func TestProviderResponseDoesNotInventUsage(t *testing.T) {
	p := Profile{BillingProvider: "anthropic", Model: "fixture-model", MaxOutputTokens: 16384, OutputTokens: 128}
	for _, usage := range []string{`null`, `{}`, `{"prompt_tokens":10,"completion_tokens":2}`, `{"prompt_tokens":10,"completion_tokens":129,"total_tokens":139}`, `{"prompt_tokens":10,"completion_tokens":2,"total_tokens":13}`} {
		response := WireResponse{Status: 200, Headers: map[string]string{"x-request-id": "request"}, Body: []byte(`{"model":"fixture-model","choices":[{"message":{"content":"not an accepted answer"}}],"usage":` + usage + `}`)}
		out, err := projectResponse(p, response)
		if err == nil || out.Text != "" {
			t.Fatalf("missing/invalid usage became accepted: %s %+v %v", usage, out, err)
		}
	}
}

type transportFunc func(context.Context, WireRequest) (WireResponse, error)

func (f transportFunc) Do(ctx context.Context, r WireRequest) (WireResponse, error) { return f(ctx, r) }

func TestPALCallerCancelsUncooperativeTransport(t *testing.T) {
	if runtime.GOOS != "linux" || os.Getenv("HCG_PAL_TESTS") != "1" {
		t.Skip("explicit isolated PAL installation required")
	}
	entered, release := make(chan struct{}), make(chan struct{})
	defer close(release)
	p := Profile{ID: "anthropic-fixture", Revision: "r1", BillingProvider: "anthropic", Model: "fixture-model", ContextTokens: 200000, MaxOutputTokens: 16384, OutputTokens: 128}
	c := Consumer{PAL: PAL{Python: os.Getenv("HCG_TEST_PAL_PYTHON"), Source: os.Getenv("HCG_TEST_PAL_SOURCE"), TempRoot: t.TempDir()}, Recorder: &memoryRecorder{}, Transport: transportFunc(func(context.Context, WireRequest) (WireResponse, error) {
		close(entered)
		<-release
		return WireResponse{}, nil
	})}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := c.DelegateRead(ctx, testExecution, p, Request{Question: "synthetic cancellation"})
		done <- err
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("formal provider did not start")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancellation reason lost: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("caller waited for uncooperative provider")
	}
}

func TestPALCallerRetainsMissingUsageReceipt(t *testing.T) {
	if runtime.GOOS != "linux" || os.Getenv("HCG_PAL_TESTS") != "1" {
		t.Skip("explicit isolated PAL installation required")
	}
	p := Profile{ID: "anthropic-fixture", Revision: "r1", BillingProvider: "anthropic", Model: "fixture-model", ContextTokens: 200000, MaxOutputTokens: 16384, OutputTokens: 128}
	recorder := &memoryRecorder{}
	calls := 0
	c := Consumer{PAL: PAL{Python: os.Getenv("HCG_TEST_PAL_PYTHON"), Source: os.Getenv("HCG_TEST_PAL_SOURCE"), TempRoot: t.TempDir()}, Recorder: recorder, Transport: transportFunc(func(context.Context, WireRequest) (WireResponse, error) {
		calls++
		return WireResponse{Status: 200, Headers: map[string]string{"x-request-id": "known-request"}, Body: []byte(`{"id":"known-response","model":"fixture-model","choices":[{"message":{"content":"must not pass"}}]}`)}, nil
	})}
	out, err := c.DelegateRead(t.Context(), testExecution, p, Request{Question: "synthetic missing usage"})
	if err == nil || out.Text != "" || calls != 1 {
		t.Fatalf("unknown usage accepted/retried: %+v %v calls=%d", out, err, calls)
	}
	if len(recorder.receipts) != 1 || recorder.receipts[0].RequestID != "known-request" || recorder.receipts[0].Provider != "anthropic" || recorder.receipts[0].Usage.Input != nil {
		t.Fatalf("lost dispatched UNKNOWN receipt: %+v", recorder.receipts)
	}
}

func TestPALProviderFailureNeverRetriesOrFallsBack(t *testing.T) {
	if runtime.GOOS != "linux" || os.Getenv("HCG_PAL_TESTS") != "1" {
		t.Skip("explicit isolated PAL installation required")
	}
	for _, provider := range []string{"openai", "anthropic"} {
		t.Run(provider, func(t *testing.T) {
			p := Profile{ID: provider + "-fixture", Revision: "r1", BillingProvider: provider, Model: "fixture-model", ContextTokens: 200000, MaxOutputTokens: 16384, OutputTokens: 128}
			calls := 0
			recorder := &memoryRecorder{}
			c := Consumer{PAL: PAL{Python: os.Getenv("HCG_TEST_PAL_PYTHON"), Source: os.Getenv("HCG_TEST_PAL_SOURCE"), TempRoot: t.TempDir()}, Recorder: recorder, Transport: transportFunc(func(_ context.Context, r WireRequest) (WireResponse, error) {
				calls++
				if r.Provider != provider {
					t.Errorf("cross-provider fallback: %s", r.Provider)
				}
				return WireResponse{Status: 503, Headers: map[string]string{"x-request-id": "failed-request"}}, nil
			})}
			out, err := c.DelegateRead(t.Context(), testExecution, p, Request{Question: "synthetic provider failure"})
			if err == nil || out.Text != "" || calls != 1 || len(recorder.receipts) != 1 || recorder.receipts[0].UnknownReason == "" {
				t.Fatalf("failed provider retried/accepted/lost hold identity: %+v %v calls=%d receipts=%+v", out, err, calls, recorder.receipts)
			}
		})
	}
}

func TestRawUsageDetailsRemainProviderSpecific(t *testing.T) {
	for _, provider := range []string{"openai", "anthropic"} {
		p := Profile{BillingProvider: provider, Model: "fixture-model", ContextTokens: 200000, MaxOutputTokens: 16384, OutputTokens: 128}
		response := WireResponse{Status: 200, Headers: map[string]string{"x-request-id": "request"}, Body: []byte(`{"id":"response","model":"fixture-model","choices":[{"message":{"content":"answer"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":2,"total_tokens":12,"prompt_tokens_details":{"cached_tokens":3},"completion_tokens_details":{"reasoning_tokens":1}}}`)}
		result, err := projectResponse(p, response)
		if err != nil {
			t.Fatal(err)
		}
		if provider == "openai" {
			if result.Receipt.Usage.CachedInput == nil || *result.Receipt.Usage.CachedInput != 3 || result.Receipt.Usage.ReasoningOutput == nil || *result.Receipt.Usage.ReasoningOutput != 1 {
				t.Fatalf("lost raw OpenAI details: %+v", result.Receipt)
			}
		} else if result.Receipt.Usage.CachedInput != nil || result.Receipt.Usage.ReasoningOutput != nil {
			t.Fatalf("compatibility details incorrectly promoted: %+v", result.Receipt)
		}
	}
}
