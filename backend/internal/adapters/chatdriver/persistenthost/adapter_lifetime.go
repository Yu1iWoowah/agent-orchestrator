package persistenthost

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/processscope"
)

type adapterIdentity struct {
	Epoch string
	Scope *processscope.Scope
}

type adapterOwner struct {
	adapterIdentity
	Session                          domain.SessionID
	Conversation, Branch, Generation string
}

const adapterOwnerPrefix = "adapter-owner-"

func recordAdapterIdentity(dataDir string, d Descriptor, pid int) error {
	dir, err := hostDir(dataDir, d.SessionID)
	if err != nil {
		return err
	}
	identity := adapterIdentity{Epoch: deliveryEpoch(d), Scope: processscope.Capture(pid)}
	return writeAdapterRecord(dir, "adapter.json", identity)
}

// BindAdapterOwner preserves the original adapter identity across a later
// host descriptor replacement. An older host might have no identity to bind.
func BindAdapterOwner(dataDir string, session domain.SessionID, epoch, conversation, branch, generation string) error {
	dir, err := hostDir(dataDir, string(session))
	if err != nil {
		return err
	}
	if generation == "" || filepath.Base(generation) != generation || strings.ContainsAny(generation, "/\\") {
		return errors.New("invalid adapter owner generation")
	}
	var identity adapterIdentity
	if err := readAdapterRecord(filepath.Join(dir, "adapter.json"), &identity); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	if epoch != identity.Epoch || identity.Scope == nil {
		return nil
	}
	owner := adapterOwner{adapterIdentity: identity, Session: session, Conversation: conversation, Branch: branch, Generation: generation}
	return writeAdapterRecord(dir, adapterOwnerPrefix+generation+".json", owner)
}

// ReconcileExitedAdapters examines only bound launch records. A successful
// callback consumes that generation's record, leaving later owners untouched.
func ReconcileExitedAdapters(ctx context.Context, dataDir string, stopped func(domain.SessionID, string) bool, settle func(context.Context, domain.SessionID, string, string, string) error) error {
	root := filepath.Join(dataDir, "chat-hosts")
	entries, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var failures []error
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !entry.IsDir() {
			continue
		}
		dir := filepath.Join(root, entry.Name())
		records, err := filepath.Glob(filepath.Join(dir, adapterOwnerPrefix+"*.json"))
		if err != nil {
			failures = append(failures, err)
		}
		for _, path := range records {
			var owner adapterOwner
			if err := readAdapterRecord(path, &owner); err != nil || string(owner.Session) != entry.Name() ||
				filepath.Base(path) != adapterOwnerPrefix+owner.Generation+".json" || !stopped(owner.Session, owner.Generation) || !processscope.AdapterExited(owner.Scope) {
				continue
			}
			if err := settle(ctx, owner.Session, owner.Conversation, owner.Branch, owner.Generation); err != nil {
				failures = append(failures, err)
				continue
			}
			if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
				failures = append(failures, err)
			}
		}
	}
	return errors.Join(failures...)
}

func readAdapterRecord(path string, out any) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, out)
}

func writeAdapterRecord(dir, name string, value any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".adapter-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	_, writeErr := tmp.Write(raw)
	if err := errors.Join(writeErr, tmp.Close()); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), filepath.Join(dir, name))
}
