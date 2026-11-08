// Package source reads the canonical AO API, never native files or SQLite.
package source

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/aoagents/agent-orchestrator/components/hcg/internal/usage"
)

// Client is a finite read-only ledger caller.
type Client struct {
	BaseURL  string
	DataRoot string
	HTTP     *http.Client
}

// FromEnvironment resolves AO namespaces. A data override must name its run-file
// explicitly because AO_DATA_DIR alone does not change AO's run-file default.
func FromEnvironment() (*Client, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	data := os.Getenv("AO_DATA_DIR")
	run := os.Getenv("AO_RUN_FILE")
	if data != "" && run == "" {
		return nil, fmt.Errorf("AO_DATA_DIR requires explicit AO_RUN_FILE to select the daemon namespace")
	}
	if data == "" {
		data = filepath.Join(home, ".ao", "data")
	}
	if run == "" {
		run = filepath.Join(home, ".ao", "running.json")
	}
	data, err = filepath.Abs(data)
	if err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(run)
	if err != nil {
		return nil, fmt.Errorf("read AO run-file: %w", err)
	}
	var info struct {
		Port int `json:"port"`
	}
	if json.Unmarshal(raw, &info) != nil || info.Port < 1 || info.Port > 65535 {
		return nil, fmt.Errorf("invalid AO run-file port")
	}
	return &Client{BaseURL: fmt.Sprintf("http://127.0.0.1:%d", info.Port), DataRoot: data, HTTP: &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}

// Scan reads every page once at a fixed insertion bound. Repairs are reread on
// each invocation; a page failure returns only this scan's partial evidence.
func (c *Client) Scan(ctx context.Context, session string) (out usage.Snapshot, err error) {
	out = usage.Snapshot{Source: "daemon_ledger", DataRoot: c.DataRoot, DataRootStatus: "unverified_configuration", ScanStatus: "unavailable", ScannedAt: time.Now().UTC(), Page: usage.Page{SessionID: session, Events: []usage.Event{}, Bindings: []usage.Binding{}}}
	defer func() {
		if err != nil {
			out.Reason = err.Error()
			if len(out.Events) > 0 {
				out.ScanStatus = "partial"
			}
		}
	}()
	ids := map[int64]bool{}
	keys := map[string]bool{}
	var after int64
	for {
		q := url.Values{"limit": {"200"}, "afterId": {strconv.FormatInt(after, 10)}}
		if after > 0 {
			q.Set("throughId", strconv.FormatInt(out.ThroughID, 10))
		}
		req, e := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+"/api/v1/usage/sessions/"+url.PathEscape(session)+"/events?"+q.Encode(), nil)
		if e != nil {
			return out, e
		}
		response, e := c.HTTP.Do(req)
		if e != nil {
			return out, e
		}
		var page usage.Page
		e = json.NewDecoder(io.LimitReader(response.Body, 16<<20)).Decode(&page)
		closeErr := response.Body.Close()
		if response.StatusCode != http.StatusOK {
			return out, fmt.Errorf("usage events HTTP %d", response.StatusCode)
		}
		if e != nil {
			return out, fmt.Errorf("decode usage page: %w", e)
		}
		if closeErr != nil {
			return out, closeErr
		}
		if page.Events == nil || page.Bindings == nil || page.SessionID != session || page.ThroughID < 0 || (after > 0 && page.ThroughID != out.ThroughID) {
			return out, fmt.Errorf("conflicting scan identity or upper bound")
		}
		last := after
		for _, event := range page.Events {
			key := strconv.FormatInt(event.BindingID, 10) + ":" + event.SourceEventKey
			if event.ID <= last || event.ID > page.ThroughID || ids[event.ID] || keys[key] || event.SourceEventKey == "" {
				return out, fmt.Errorf("conflicting event id/key")
			}
			ids[event.ID] = true
			keys[key] = true
			last = event.ID
		}
		if page.NextAfterID != last || (page.HasMore && (last == after || last >= page.ThroughID)) {
			return out, fmt.Errorf("invalid page cursor")
		}
		for _, binding := range page.Bindings {
			if binding.SessionID != session {
				return out, fmt.Errorf("conflicting binding session")
			}
		}
		events := append(out.Events, page.Events...)
		out.Page = page
		out.Events = events
		if !page.HasMore {
			out.ScanStatus = "complete"
			return out, nil
		}
		after = last
	}
}
