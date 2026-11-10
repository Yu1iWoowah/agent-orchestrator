package rca

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"strings"
	"testing"
)

func otlpField(field int, data []byte) []byte {
	out := binary.AppendUvarint(nil, uint64(field<<3|2))
	out = binary.AppendUvarint(out, uint64(len(data)))
	return append(out, data...)
}
func otlpRecord(attributes map[string]string) []byte {
	var record []byte
	for key, value := range attributes {
		attr := append(otlpField(1, []byte(key)), otlpField(2, otlpField(1, []byte(value)))...)
		record = append(record, otlpField(6, attr)...)
	}
	return record
}
func otlpBatch(records ...[]byte) []byte {
	var scope []byte
	for _, record := range records {
		scope = append(scope, otlpField(2, record)...)
	}
	return otlpField(1, otlpField(2, scope))
}
func syntheticClaudeRecord() []byte {
	return otlpRecord(map[string]string{"event.name": "claude_code.api_request_body", "session.id": "synthetic-native", "request_body_id": "body-1", "request_id": "request-1", "body": `{"system":"系統","messages":[{"role":"user","content":"hello"}]}`})
}
func TestClaudeOTLPBoundedRecords(t *testing.T) {
	for _, n := range []int{255, 256, 257} {
		records := make([][]byte, n)
		for i := range records {
			records[i] = syntheticClaudeRecord()
		}
		batch := otlpBatch(records...)
		count := 0
		reason := readClaudeOTLP(context.Background(), bytes.NewReader(batch), int64(len(batch)), func(event claudeEvent) string {
			count++
			if event.session != "synthetic-native" || event.requestID != "request-1" {
				t.Fatal("incorrect identity")
			}
			return ""
		})
		if n <= 256 && (reason != "" || count != n) {
			t.Fatalf("n=%d count=%d reason=%s", n, count, reason)
		}
		if n == 257 && (reason != "record_count_limit" || count != 256) {
			t.Fatalf("count=%d reason=%s", count, reason)
		}
	}
}
func TestClaudeOTLPRefusesRecordBeforeTail(t *testing.T) {
	// The outer length is known but the oversized record body has not arrived.
	// The decoder must stop after its length, never attempting to read the tail.
	for _, size := range []int{claudeRecordBytes - 1, claudeRecordBytes, claudeRecordBytes + 1} {
		prefix := binary.AppendUvarint(nil, 18)
		prefix = binary.AppendUvarint(prefix, uint64(size))
		scopeSize := len(prefix) + size
		scopeHeader := binary.AppendUvarint(nil, 18)
		scopeHeader = binary.AppendUvarint(scopeHeader, uint64(scopeSize))
		resourceSize := len(scopeHeader) + scopeSize
		rootHeader := binary.AppendUvarint(nil, 10)
		rootHeader = binary.AppendUvarint(rootHeader, uint64(resourceSize))
		header := append(append(rootHeader, scopeHeader...), prefix...)
		r := &claudeTailReader{prefix: header}
		reason := readClaudeOTLP(context.Background(), r, int64(len(header)+size), func(claudeEvent) string { return "" })
		if size > claudeRecordBytes {
			if reason != "record_limit" || r.tailReads != 0 {
				t.Fatalf("reason=%s tail reads=%d", reason, r.tailReads)
			}
		} else if reason != "source_read_failed" || r.tailReads == 0 {
			t.Fatalf("boundary reason=%s reads=%d", reason, r.tailReads)
		}
	}
}

type claudeTailReader struct {
	prefix    []byte
	tailReads int
}

func (r *claudeTailReader) Read(p []byte) (int, error) {
	if len(r.prefix) > 0 {
		n := copy(p, r.prefix)
		r.prefix = r.prefix[n:]
		return n, nil
	}
	r.tailReads++
	return 0, io.ErrUnexpectedEOF
}
func TestClaudeOTLPRejectsMalformedAndBoundedIdentity(t *testing.T) {
	for _, tc := range []struct {
		name   string
		data   []byte
		reason string
	}{
		{"empty", nil, "unknown_schema"},
		{"bad tag", []byte{0}, "invalid_otlp"},
		{"truncated", []byte{10, 3, 18}, "invalid_otlp"},
		{"identity", otlpBatch(otlpRecord(map[string]string{"event.name": "api_request_body", "session.id": strings.Repeat("x", 257)})), "identity_limit"},
		{"missing", otlpBatch(otlpRecord(map[string]string{"event.name": "api_request_body"})), "unknown_schema"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if why := readClaudeOTLP(context.Background(), bytes.NewReader(tc.data), int64(len(tc.data)), func(claudeEvent) string { return "" }); why != tc.reason {
				t.Fatalf("got %s want %s", why, tc.reason)
			}
		})
	}
	for _, n := range []int{255, 256} {
		data := otlpBatch(otlpRecord(map[string]string{"event.name": "api_request_body", "session.id": strings.Repeat("x", n)}))
		if why := readClaudeOTLP(context.Background(), bytes.NewReader(data), int64(len(data)), func(claudeEvent) string { return "" }); why != "" {
			t.Fatal(why)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	data := otlpBatch(syntheticClaudeRecord())
	if why := readClaudeOTLP(ctx, bytes.NewReader(data), int64(len(data)), func(claudeEvent) string { t.Fatal("consumed after cancel"); return "" }); why != "canceled" {
		t.Fatal(why)
	}
}
func TestClaudeClassifiesMutuallyExclusiveObservedBytes(t *testing.T) {
	body := []byte(`{"system":"系統","tools":[{"name":"mcp__private_name","input_schema":{"type":"object"}}],"messages":[{"role":"user","content":[{"type":"text","text":"hello"},{"type":"tool_result","content":"結果"}]},{"role":"assistant","content":[{"type":"thinking","thinking":"思考"},{"type":"redacted_thinking","data":"secret"},{"type":"tool_use","name":"private_name","input":{"x":1}}]}]}`)
	r, mcp, why := classifyClaudeBody(body, false)
	if why != "" {
		t.Fatal(why)
	}
	toolBytes := int64(len(`{"name":"mcp__private_name","input_schema":{"type":"object"}}`))
	want := []int64{6, toolBytes, 5, 6, 6, 7}
	for i, bucket := range r.Buckets {
		if bucket.Bytes == nil || *bucket.Bytes != want[i] || bucket.EstimatedTokens == nil || *bucket.EstimatedTokens != (want[i]+3)/4 {
			t.Fatalf("bucket %d=%+v want %d", i, bucket, want[i])
		}
	}
	if mcp != toolBytes || !hasReason(r, "hidden_thinking") || !hasReason(r, "retained_context_unknown") {
		t.Fatalf("mcp=%d reasons=%v", mcp, r.Reasons)
	}
	serialized, _ := json.Marshal(r)
	for _, text := range []string{"private_name", "secret", "結果", "思考"} {
		if bytes.Contains(serialized, []byte(text)) {
			t.Fatal("content escaped classifier")
		}
	}
}
func TestClaudeBodySizeDepthAndQuality(t *testing.T) {
	for _, n := range []int{claudeBodyBytes - 1, claudeBodyBytes, claudeBodyBytes + 1} {
		prefix := `{"messages":[{"role":"user","content":"`
		suffix := `"}]}`
		body := []byte(prefix + strings.Repeat("x", n-len(prefix)-len(suffix)) + suffix)
		r, _, why := classifyClaudeBody(body, false)
		if n > claudeBodyBytes {
			if why != "body_limit" {
				t.Fatal(why)
			}
		} else if why != "" || r.Buckets[2].Bytes == nil || *r.Buckets[2].Bytes != int64(n-len(prefix)-len(suffix)) {
			t.Fatalf("n=%d reason=%s", n, why)
		}
	}
	for _, depth := range []int{31, 32, 33} {
		body := []byte(`{"metadata":` + strings.Repeat("[", depth-1) + `0` + strings.Repeat("]", depth-1) + `}`)
		_, _, why := classifyClaudeBody(body, false)
		if depth <= 32 && why != "" || depth > 32 && why != "parse_limit" {
			t.Fatalf("depth=%d reason=%s", depth, why)
		}
	}
	r, _, why := classifyClaudeBody([]byte(`{"messages":[{"role":"user","content":[{"type":"image","source":{"data":"synthetic-canary"}}]}]}`), true)
	if why != "" || !hasReason(r, "source_truncated") || !hasReason(r, "unsupported_content") {
		t.Fatalf("reason=%s gaps=%v", why, r.Reasons)
	}
	for _, bucket := range r.Buckets {
		if bucket.Bytes != nil {
			t.Fatal("unseen content guessed")
		}
	}
}

func TestClaudeWideJSONContainersRefusedBeforeDecode(t *testing.T) {
	body := []byte(`{"messages":[` + strings.Repeat(`{},`, 40000) + `{}]}`)
	if len(body) > claudeBodyBytes {
		t.Fatal("invalid fixture")
	}
	if _, _, why := classifyClaudeBody(body, false); why != "parse_limit" {
		t.Fatal(why)
	}
}

func TestClaudeEncodedBatchBoundaries(t *testing.T) {
	base := otlpBatch(syntheticClaudeRecord())
	for _, size := range []int{claudeBatchBytes - 1, claudeBatchBytes, claudeBatchBytes + 1} {
		padding := size - len(base) - 4
		payload := append(append([]byte{}, base...), otlpField(15, make([]byte, padding))...)
		if len(payload) != size {
			t.Fatalf("fixture length=%d want=%d", len(payload), size)
		}
		reader := bytes.NewReader(payload)
		why := readClaudeOTLP(t.Context(), reader, int64(len(payload)), func(claudeEvent) string { return "" })
		if size <= claudeBatchBytes && why != "" || size > claudeBatchBytes && why != "batch_limit" {
			t.Fatalf("size=%d reason=%s", size, why)
		}
		if size > claudeBatchBytes && reader.Len() != size {
			t.Fatal("oversized batch read before admission")
		}
	}
}
