package quota

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// This child is a cooperating writer: it must acquire the same OS lock before
// inserting a synthetic failed-write tail. No strace or scheduler timing needed.
func TestQuotaLockProbe(t *testing.T) {
	path := os.Getenv("HCG_QUOTA_LOCK_PROBE")
	if path == "" {
		return
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_APPEND, 0600)
	if err != nil {
		t.Fatal(err)
	}
	locked, err := tryLogLock(f)
	if err != nil {
		f.Close()
		t.Fatal(err)
	}
	if locked {
		if _, err := f.Write([]byte(`{"dataRoot":`)); err != nil {
			f.Close()
			t.Fatal(err)
		}
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if locked {
		fmt.Println("interleaved")
	} else {
		fmt.Println("blocked")
	}
}

func TestStoreLockCoversValidationAndAppend(t *testing.T) {
	root := t.TempDir()
	store := Store{Root: root}
	sample := Project([]byte(`{"accounts":[]}`), nil, root, time.Now())
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := store.Append(ctx, sample); err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "hcg", "quota", "samples.jsonl")
	blocked := false
	store.afterValidation = func() {
		cmd := exec.CommandContext(ctx, executable, "-test.run=^TestQuotaLockProbe$")
		cmd.Env = append(os.Environ(), "HCG_QUOTA_LOCK_PROBE="+path)
		output, err := cmd.CombinedOutput()
		if err != nil {
			t.Errorf("probe: %v %s", err, output)
		}
		blocked = bytes.Contains(output, []byte("blocked"))
	}
	sample.SampledAt = sample.SampledAt.Add(time.Minute)
	if err := store.Append(ctx, sample); err != nil {
		t.Fatal(err)
	}
	if !blocked {
		t.Error("another process inserted a partial record after validation")
	}
	rows, err := store.Read(ctx)
	if err != nil || len(rows) != 2 {
		t.Fatalf("append accepted a damaged interleave: rows=%d err=%v", len(rows), err)
	}
}

func TestStoreLockWaitCanBeCancelled(t *testing.T) {
	root := t.TempDir()
	store := Store{Root: root}
	sample := Project([]byte(`{"accounts":[]}`), nil, root, time.Now())
	if err := store.Append(context.Background(), sample); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "hcg", "quota", "samples.jsonl")
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	locked, err := tryLogLock(f)
	if err != nil || !locked {
		t.Fatalf("fixture lock=%t err=%v", locked, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	err = store.Append(ctx, sample)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("lock wait err=%v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(original, after) {
		t.Error("cancelled lock waiter changed the log")
	}
}

func TestStoreConcurrentAppends(t *testing.T) {
	root := t.TempDir()
	store := Store{Root: root}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	failures := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 3; j++ {
				if err := store.Append(ctx, Project([]byte(`{"accounts":[]}`), nil, root, time.Now())); err != nil {
					failures <- err
					return
				}
			}
		}()
	}
	wg.Wait()
	close(failures)
	for err := range failures {
		t.Error(err)
	}
	rows, err := store.Read(ctx)
	if err != nil || len(rows) != 24 {
		t.Fatalf("rows=%d err=%v", len(rows), err)
	}
}

func TestStoreWaiterRejectsFailedWritersTail(t *testing.T) {
	root := t.TempDir()
	store := Store{Root: root}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	sample := Project([]byte(`{"accounts":[]}`), nil, root, time.Now())
	if err := store.Append(ctx, sample); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "hcg", "quota", "samples.jsonl")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_APPEND, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	locked, err := tryLogLock(f)
	if err != nil || !locked {
		t.Fatalf("fixture lock=%t err=%v", locked, err)
	}
	tail := []byte(`{"dataRoot":`)
	if _, err := f.Write(tail); err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	result := make(chan error, 1)
	go func() { close(started); result <- store.Append(ctx, sample) }()
	<-started
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if err := <-result; err == nil || errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("failed-tail waiter err=%v", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, append(before, tail...)) {
		t.Fatal("waiter changed failed writer evidence")
	}
}
