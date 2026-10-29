package cli

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"regexp"
	"time"

	"github.com/google/uuid"

	"github.com/aoagents/agent-orchestrator/backend/internal/config"
)

type appCredential struct {
	ProjectID  string    `json:"projectId"`
	ID         string    `json:"grantId"`
	Token      string    `json:"token"`
	SessionID  string    `json:"sessionId"`
	InstanceID string    `json:"instanceId"`
	ExpiresAt  time.Time `json:"expiresAt"`
}

var errAppCredential = errors.New("APP_DIAGNOSTICS_CREDENTIAL_UNAVAILABLE: check local app diagnostics in Settings")

func privateAppDirectory(parent *os.Root, name string) (*os.Root, error) {
	info, err := parent.Lstat(name)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, errAppCredential
	}
	root, err := parent.OpenRoot(name)
	if err != nil {
		return nil, errAppCredential
	}
	file, err := root.Open(".")
	if err != nil {
		_ = root.Close()
		return nil, errAppCredential
	}
	defer func() { _ = file.Close() }()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) || validateAppCredentialFile(file, true) != nil {
		_ = root.Close()
		return nil, errAppCredential
	}
	return root, nil
}
func loadAppCredential(session, grant string) (appCredential, error) {
	cfg, err := config.Load()
	if err != nil {
		return appCredential{}, errAppCredential
	}
	base, err := os.OpenRoot(cfg.DataDir)
	if err != nil {
		return appCredential{}, errAppCredential
	}
	defer func() { _ = base.Close() }()
	diagnostics, err := privateAppDirectory(base, "diagnostics")
	if err != nil {
		return appCredential{}, err
	}
	defer func() { _ = diagnostics.Close() }()
	root, err := privateAppDirectory(diagnostics, "grants")
	if err != nil {
		return appCredential{}, err
	}
	defer func() { _ = root.Close() }()
	directory, err := root.Open(".")
	if err != nil {
		return appCredential{}, errAppCredential
	}
	defer func() { _ = directory.Close() }()
	entries, err := directory.ReadDir(-1)
	if err != nil {
		return appCredential{}, errAppCredential
	}
	var matches []appCredential
	for _, entry := range entries {
		name := entry.Name()
		if len(name) != 41 || name[36:] != ".json" {
			continue
		}
		if _, err := uuid.Parse(name[:36]); err != nil {
			continue
		}
		if grant != "" && name[:36] != grant {
			continue
		}
		info, err := root.Lstat(name)
		if err != nil || !info.Mode().IsRegular() {
			return appCredential{}, errAppCredential
		}
		file, err := root.Open(name)
		if err != nil {
			return appCredential{}, errAppCredential
		}
		opened, statErr := file.Stat()
		if statErr != nil || !os.SameFile(info, opened) || validateAppCredentialFile(file, false) != nil {
			_ = file.Close()
			return appCredential{}, errAppCredential
		}
		var credential appCredential
		decoder := json.NewDecoder(io.LimitReader(file, 4097))
		decoder.DisallowUnknownFields()
		decodeErr := decoder.Decode(&credential)
		endErr := decoder.Decode(new(any))
		_ = file.Close()
		if decodeErr != nil || endErr != io.EOF || opened.Size() > 4096 || credential.ID != name[:36] || !appIDPattern.MatchString(credential.InstanceID) || !appIDPattern.MatchString(credential.ProjectID) || !regexp.MustCompile(`^[A-Za-z0-9_-]{32,128}$`).MatchString(credential.Token) {
			return appCredential{}, errAppCredential
		}
		if credential.SessionID == session && time.Now().Before(credential.ExpiresAt) {
			matches = append(matches, credential)
		}
	}
	if len(matches) > 1 {
		return appCredential{}, usageError{errors.New("multiple matching grants; specify --grant")}
	}
	if len(matches) != 1 {
		return appCredential{}, errAppCredential
	}
	return matches[0], nil
}
