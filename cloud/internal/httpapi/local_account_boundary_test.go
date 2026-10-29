package httpapi

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCloudCreateSessionRejectsLocalAccountSelectors(t *testing.T) {
	for _, field := range []string{`"agentAccountId":"private-A"`, `"agentAccounts":{"codex":"private-A"}`, `"codexHome":"/fake/private/home"`} {
		req := httptest.NewRequest("POST", "/sessions", strings.NewReader(`{"projectId":"p","kind":"worker",`+field+`}`))
		var decoded createSessionRequest
		if err := decodeJSON(httptest.NewRecorder(), req, &decoded); err == nil {
			t.Fatalf("Cloud accepted local selector: %s", field)
		}
	}
}
