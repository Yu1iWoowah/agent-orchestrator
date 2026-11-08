package quota

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestProjectionAndStore(t *testing.T) {
	at := time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC)
	root := t.TempDir()
	raw := []byte(`{"accounts":[{"id":"a","provider":"claude","email":"private@example.com","problem":"","limits":{"fetchedAt":"2026-11-09T00:00:00Z","extraUsage":{"private":"secret"},"windows":[{"id":"five","label":"5h","usedPercent":0,"resetsAt":"2026-11-09T05:00:00Z"},{"id":"week","label":"weekly"}]}},{"id":"b","provider":"codex","problem":"raw provider secret","limits":null}]}`)
	sample := Project(raw, nil, root, at)
	if sample.Status != "success" || len(sample.Accounts) != 2 || sample.Accounts[0].Windows[0].UsedPercent == nil || *sample.Accounts[0].Windows[0].UsedPercent != 0 || sample.Accounts[0].Windows[1].UsedPercent != nil || sample.Accounts[1].Status != "unavailable" {
		t.Fatalf("sample=%+v", sample)
	}
	store := Store{Root: root}
	if err := store.Append(context.Background(), sample); err != nil {
		t.Fatal(err)
	}
	sample.SampledAt = at.Add(time.Minute)
	if err := store.Append(context.Background(), sample); err != nil {
		t.Fatal(err)
	}
	rows, err := store.Read(context.Background())
	if err != nil || len(rows) != 2 || !rows[0].Accounts[0].FetchedAt.Equal(*rows[1].Accounts[0].FetchedAt) {
		t.Fatalf("rows=%+v err=%v", rows, err)
	}
	saved, err := os.ReadFile(filepath.Join(root, "hcg", "quota", "samples.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	for _, private := range []string{"email", "private@example.com", "extraUsage", "secret", "problem"} {
		if strings.Contains(string(saved), private) {
			t.Fatalf("leaked %s", private)
		}
	}
	for _, failure := range []Sample{Project(raw, errors.New("raw private error"), root, at), Project([]byte(`{broken secret`), nil, root, at)} {
		encoded, _ := json.Marshal(failure)
		if failure.Status != "error" || len(failure.Accounts) != 0 || strings.Contains(string(encoded), "secret") || strings.Contains(string(encoded), "private") {
			t.Fatalf("unsafe failure %s", encoded)
		}
	}
	if err := (Store{}).Append(context.Background(), sample); err == nil {
		t.Fatal("accepted implicit root")
	}
}

func TestObserveSerialAndCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	count := 0
	err := Observe(ctx, time.Microsecond, func(ctx context.Context) error {
		count++
		if count == 3 {
			cancel()
		}
		return nil
	})
	if !errors.Is(err, context.Canceled) || count != 3 {
		t.Fatalf("count=%d err=%v", count, err)
	}
	if err := Observe(context.Background(), 0, func(context.Context) error { return nil }); err == nil {
		t.Fatal("accepted zero interval")
	}
	calls := 0
	err = Observe(context.Background(), time.Second, func(context.Context) error { calls++; return errors.New("write failed") })
	if err == nil || calls != 1 {
		t.Fatalf("calls=%d err=%v", calls, err)
	}
}

func TestStoreCancelledBeforeWrite(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	root := t.TempDir()
	sample := Project([]byte(`{"accounts":[]}`), nil, root, time.Now())
	err := (Store{Root: root}).Append(ctx, sample)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "hcg")); !os.IsNotExist(err) {
		t.Fatalf("cancelled write created state: %v", err)
	}
}
