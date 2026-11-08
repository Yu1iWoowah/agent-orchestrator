package usage

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestProviderUsageNumericProjection(t *testing.T) {
	for _, tc := range []struct{ raw, status, want string }{
		{"", "missing", `{}`}, {`{}`, "missing", `{}`}, {`null`, "invalid", `{}`}, {`[]`, "invalid", `{}`}, {`{`, "invalid", `{}`},
		{`{"input_tokens":0,"output_tokens":5}`, "complete", `{"input_tokens":0,"output_tokens":5}`},
		{`{"input_tokens":null,"output_tokens":-1}`, "partial", `{}`},
		{`{"input_tokens":1.5,"model_context_window":9007199254740992}`, "partial", `{}`},
		{`{"input_tokens":100,"unknown":"SENSITIVE"}`, "partial", `{"input_tokens":100}`},
		{`{"last_token_usage":{"input_tokens":100,"cached_input_tokens":70,"output_tokens":5,"path":"SENSITIVE"},"total_token_usage":null}`, "partial", `{"last_token_usage":{"cached_input_tokens":70,"input_tokens":100,"output_tokens":5}}`},
	} {
		t.Run(tc.raw, func(t *testing.T) {
			got, status := projectProviderUsage(tc.raw)
			body, err := json.Marshal(got)
			if err != nil {
				t.Fatal(err)
			}
			if status != tc.status || string(body) != tc.want || strings.Contains(string(body), "SENSITIVE") {
				t.Fatalf("got %s %s", body, status)
			}
		})
	}
}
