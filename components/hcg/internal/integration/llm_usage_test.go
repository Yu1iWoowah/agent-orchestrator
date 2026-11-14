package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

const claudeFixture = `{"type":"assistant","sessionId":"native-test","timestamp":"2026-11-09T00:00:00Z","cwd":"/private/DO_NOT_EXPOSE","uuid":"m1","message":{"id":"msg-1","role":"assistant","model":"claude-sonnet-4-5","usage":{"input_tokens":100,"output_tokens":20,"cache_read_input_tokens":30,"cache_creation_input_tokens":40},"content":[{"type":"tool_use","id":"t1","name":"Read","input":{"file_path":"DO_NOT_EXPOSE"}}]}}
{"type":"user","sessionId":"native-test","timestamp":"2026-11-09T00:00:01Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"DO_NOT_EXPOSE","is_error":false}]}}
`

const eventJSON = `{"source":"claude","sessionId":"native-test","timestamp":"2026-11-09T00:00:00Z","inputTokens":100,"cacheReadTokens":30,"cacheWriteTokens":40,"outputTokens":20,"reasoningTokens":0,"totalTokens":190,"costMode":"estimated","repoRoot":"DO_NOT_EXPOSE"}`

func TestProjection(t *testing.T) {
	events, err := projectEvents([]byte(eventJSON+"\n"), "claude", "native-test")
	if err != nil || len(events) != 1 || events[0].InputIncludingCache != 170 || events[0].OutputTokens != 20 {
		t.Fatalf("cache-write vector: %+v %v", events, err)
	}
	for _, invalid := range []string{
		strings.Replace(eventJSON, `"inputTokens":100,`, "", 1),
		strings.Replace(eventJSON, `"inputTokens":100`, `"inputTokens":-1`, 1),
		strings.Replace(eventJSON, `"inputTokens":100`, `"inputTokens":9007199254740992`, 1),
		strings.Replace(eventJSON, `"sessionId":"native-test"`, `"sessionId":"another-session"`, 1),
		strings.Replace(eventJSON, `"reasoningTokens":0`, `"reasoningTokens":21`, 1),
	} {
		if _, err := projectEvents([]byte(invalid), "claude", "native-test"); err == nil {
			t.Fatalf("bad event accepted: %s", invalid)
		}
	}
}

func engineTests(t *testing.T) (Runner, string, string) {
	t.Helper()
	if os.Getenv("HCG_ENGINE_TESTS") != "1" || runtime.GOOS != "linux" {
		t.Skip("requires explicitly enabled isolated Linux engine fixture")
	}
	node, engine := os.Getenv("HCG_TEST_NODE"), os.Getenv("HCG_TEST_ENGINE")
	if node == "" || engine == "" {
		t.Fatal("fixed engine and Node paths required")
	}
	return Runner{TempRoot: t.TempDir()}, node, engine
}

func TestFixedEngineAuthorizedView(t *testing.T) {
	r, node, path := engineTests(t)
	e, err := r.Register(context.Background(), path, node, EngineVersion)
	if err != nil {
		t.Fatal(err)
	}
	id := testIdentity(t.TempDir())
	m, err := LaunchManifest(nil, id, PolicyInputs{Project: boolean(true)}, &e)
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "allowed.jsonl"), []byte(claudeFixture), 0600); err != nil {
		t.Fatal(err)
	}
	// An adjacent file must not be discovered, even for the same native id.
	if err := os.WriteFile(filepath.Join(root, "unlisted.jsonl"), []byte(strings.ReplaceAll(claudeFixture, "msg-1", "msg-2")), 0600); err != nil {
		t.Fatal(err)
	}
	source := AuthorizedSource{root, "allowed.jsonl", "claude", id.NativeID}
	out := r.Report(context.Background(), m, []AuthorizedSource{source})
	if out.Status != "ok" || len(out.Events) != 1 || out.Events[0].InputIncludingCache != 170 || len(out.Tools) != 1 || out.Tools[0] != (ToolCounts{"read", 1, 0, 0}) {
		t.Fatalf("fixed engine report: %+v", out)
	}
	b, _ := json.Marshal(out)
	if strings.Contains(string(b), "DO_NOT_EXPOSE") || out.BucketAvailability != "engine_normalized_native_availability_unknown" || out.Source != "external_analysis" {
		t.Fatalf("unsafe output: %s", b)
	}
	if err := os.Symlink(filepath.Join(root, "allowed.jsonl"), filepath.Join(root, "absolute-link")); err != nil {
		t.Fatal(err)
	}
	source.Path = "absolute-link"
	if got := r.Report(context.Background(), m, []AuthorizedSource{source}); got.Status == "ok" {
		t.Fatal("escaping symlink accepted")
	}
	m.Inputs.EmergencyStop = true
	if got := r.Report(context.Background(), m, nil); got.Status != "disabled" {
		t.Fatalf("stop: %+v", got)
	}
	entries, err := os.ReadDir(r.TempRoot)
	if err != nil || len(entries) != 0 {
		t.Fatalf("job files left behind: %v %v", entries, err)
	}
}

func TestSandboxBoundary(t *testing.T) {
	r, node, path := engineTests(t)
	_, modules, err := engineFiles(t.Context(), path, node)
	if err != nil {
		t.Fatal(err)
	}
	dir, err := r.jobDir()
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	t.Setenv("OPENAI_API_KEY", "synthetic-secret")
	t.Setenv("AO_SESSION_ID", "synthetic-session")
	// No host HOME/source files, credentials, writable engine/view, or host net.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	program := fmt.Sprintf(`const fs=require('fs'), net=require('net');
if (process.env.OPENAI_API_KEY || process.env.AO_SESSION_ID || process.env.LLM_USAGE_SKIP_UPDATE_CHECK!=='1') process.exit(10);
if(fs.existsSync('/home/yuliwoowah') || fs.existsSync('/etc/passwd')) process.exit(11);
try{fs.writeFileSync('/engine/node_modules/forbidden','x');process.exit(12)}catch{}
try{fs.writeFileSync('/view/forbidden','x');process.exit(13)}catch{}
fs.writeFileSync('/home/report/cache','ok');
const s=net.connect({host:'127.0.0.1',port:%d}); s.on('connect',()=>process.exit(14));s.on('error',()=>console.log('isolated'));`, listener.Addr().(*net.TCPAddr).Port)
	b, err := r.sandbox(context.Background(), dir, modules, node, []string{"/runtime/node", "-e", program})
	if err != nil || strings.TrimSpace(string(b)) != "isolated" {
		t.Fatalf("sandbox boundary: %q %v", b, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err = r.sandbox(ctx, dir, modules, node, []string{"/runtime/node", "-e", `require('child_process').spawn('/runtime/node',['-e',"setTimeout(()=>require('fs').writeFileSync('/home/report/orphan','x'),500)"],{stdio:'ignore'});setInterval(()=>{},1000)`})
	if err == nil || time.Since(start) > 3*time.Second {
		t.Fatalf("cancel did not bound own job: %v %v", err, time.Since(start))
	}
	time.Sleep(700 * time.Millisecond)
	if _, err := os.Stat(filepath.Join(dir, "home", "orphan")); !os.IsNotExist(err) {
		t.Fatal("cancel left a child in its PID namespace")
	}
}

func TestFixedEngineCodexMissingBuckets(t *testing.T) {
	r, node, path := engineTests(t)
	e, err := r.Register(t.Context(), path, node, EngineVersion)
	if err != nil {
		t.Fatal(err)
	}
	id := testIdentity(t.TempDir())
	id.Harness = "codex"
	id.NativeID = "native-codex"
	m, err := LaunchManifest(nil, id, PolicyInputs{Project: boolean(true)}, &e)
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	fixture := `{"timestamp":"2026-11-09T00:00:00Z","type":"session_meta","payload":{"id":"native-codex","cwd":"/synthetic"}}
{"timestamp":"2026-11-09T00:00:01Z","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":100,"cached_input_tokens":30,"output_tokens":20,"reasoning_output_tokens":5,"total_tokens":120},"last_token_usage":{"input_tokens":100,"cached_input_tokens":30,"output_tokens":20,"reasoning_output_tokens":5,"total_tokens":120}}}}
{"timestamp":"2026-11-09T00:00:02Z","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":110,"output_tokens":22,"total_tokens":132},"last_token_usage":{"input_tokens":10,"output_tokens":2,"total_tokens":12}}}}
`
	if err := os.WriteFile(filepath.Join(root, "native.jsonl"), []byte(fixture), 0600); err != nil {
		t.Fatal(err)
	}
	out := r.Report(t.Context(), m, []AuthorizedSource{{root, "native.jsonl", "codex", id.NativeID}})
	if out.Status != "ok" || len(out.Events) != 2 || out.Events[0].InputTokens != 70 || out.Events[0].InputIncludingCache != 100 || out.Events[0].ReasoningTokens != 5 || out.Events[0].EngineTotalTokens != 120 || out.Events[1].CacheReadTokens != 0 || out.BucketAvailability != "engine_normalized_native_availability_unknown" {
		t.Fatalf("Codex cache synonym/missing bucket mapping: %+v", out)
	}
}
