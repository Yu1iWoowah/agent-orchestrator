// Package integration implements the fixed, optional external reporting consumer.
// It never writes canonical usage or changes an agent's environment.
package integration

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
)

const EngineName = "llm-usage-metrics"
const EngineVersion = "0.14.3"
const ManifestVersion = 1

var ErrManifestUnavailable = errors.New("reporting manifest unavailable")

type PolicyInputs struct {
	Project       *bool `json:"project"`
	Role          *bool `json:"role"`
	Session       *bool `json:"session"`
	EmergencyStop bool  `json:"emergencyStop"`
}

type Policy struct {
	Enabled bool   `json:"enabled"`
	Source  string `json:"source"`
}

func ResolvePolicy(role string, in PolicyInputs) Policy {
	if role != "worker" && role != "orchestrator" {
		return Policy{Source: "unsupported_role"}
	}
	p := Policy{Source: "default"}
	for _, v := range []struct {
		value *bool
		name  string
	}{{in.Project, "project"}, {in.Role, "role"}, {in.Session, "session"}} {
		if v.value != nil {
			p = Policy{*v.value, v.name}
		}
	}
	if in.EmergencyStop {
		return Policy{Source: "emergency_stop"}
	}
	return p
}

// Identity comes from AO-owned session facts, never from the tool or HTTP body.
type Identity struct {
	DataRoot             string `json:"dataRoot"`
	ProjectID            string `json:"projectId"`
	SessionID            string `json:"sessionId"`
	Role                 string `json:"role"`
	Mode                 string `json:"mode"`
	Harness              string `json:"harness"`
	Worktree             string `json:"worktree"`
	ArtifactDir          string `json:"artifactDir"`
	NativeID             string `json:"nativeId"`
	LaunchID             string `json:"launchId"`
	ControllerGeneration string `json:"controllerGeneration"`
}

func (id Identity) valid() bool {
	return filepath.IsAbs(id.DataRoot) && id.ProjectID != "" && id.SessionID != "" && id.Harness != ""
}

func (id Identity) namespace() string {
	return digest([]string{id.DataRoot, id.ProjectID, id.SessionID, EngineName})
}

type Manifest struct {
	SchemaVersion int          `json:"schemaVersion"`
	Revision      uint64       `json:"revision"`
	Identity      Identity     `json:"identity"`
	Inputs        PolicyInputs `json:"inputs"`
	Policy        Policy       `json:"policy"`
	Engine        *Engine      `json:"engine"`
	ConfigDigest  string       `json:"configDigest"`
	MountInputs   PolicyInputs `json:"mountInputs"`
	MountPolicy   Policy       `json:"mountPolicy"`
	MountEngine   *Engine      `json:"mountEngine"`
	MountDigest   string       `json:"mountDigest"`
	Namespace     string       `json:"namespace"`
	Requested     bool         `json:"requested"`
	Active        bool         `json:"active"`
	Reason        string       `json:"reason"`
}

func digest(v any) string {
	b, _ := json.Marshal(v)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// LaunchManifest is called only at an AO launch boundary. A live reconnect
// preserves the recorded mounting; a new launch inherits the saved override,
// then resolves the current project/role inputs. Controller rotation alone is
// not a new native launch. Policy operations are separate from reconnect.
func LaunchManifest(previous *Manifest, id Identity, in PolicyInputs, engine *Engine) (Manifest, error) {
	if !id.valid() {
		return Manifest{}, ErrManifestUnavailable
	}
	var revision uint64 = 1
	if previous != nil {
		if previous.SchemaVersion != ManifestVersion || previous.Namespace != id.namespace() {
			return Manifest{}, ErrManifestUnavailable
		}
		revision = previous.Revision + 1
		if id.LaunchID != "" && previous.Identity.Mode == id.Mode && previous.Identity.Harness == id.Harness && (previous.Identity.NativeID == "" || id.NativeID == "" || previous.Identity.NativeID == id.NativeID) && previous.Identity.LaunchID == id.LaunchID {
			m := *previous
			m.Revision = revision
			m.Identity.ControllerGeneration = id.ControllerGeneration
			if m.Identity.NativeID == "" {
				m.Identity.NativeID = id.NativeID
			}
			return m, nil
		}
		in.Session = previous.Inputs.Session
	}
	p := ResolvePolicy(id.Role, in)
	m := Manifest{SchemaVersion: ManifestVersion, Revision: revision, Identity: id, Inputs: in, Policy: p, Engine: engine, Namespace: id.namespace(), Reason: "disabled"}
	requested := in
	requested.EmergencyStop = false
	m.Requested = ResolvePolicy(id.Role, requested).Enabled
	m.ConfigDigest = configurationDigest(in, engine)
	m.MountInputs, m.MountPolicy, m.MountDigest = in, p, m.ConfigDigest
	m.MountEngine = engine
	if p.Enabled {
		m.Reason = "engine_unavailable"
		if id.NativeID == "" || id.LaunchID == "" {
			m.Reason = "native_identity_unknown"
		} else if engine != nil && engine.valid() {
			m.Active, m.Reason = true, "ready"
		}
	}
	return m, nil
}

func configurationDigest(inputs PolicyInputs, engine *Engine) string {
	return digest(struct {
		Inputs PolicyInputs
		Engine *Engine
	}{inputs, engine})
}

// ManifestStore provides atomic files under an explicit AO root. The owning
// service serializes policy read/modify/write with its session operation lock.
// Missing/unknown manifests are unavailable; Load never creates defaults.
type ManifestStore struct{ Root string }

func (s ManifestStore) open(id Identity) (*os.Root, string, error) {
	if !id.valid() || s.Root != id.DataRoot {
		return nil, "", ErrManifestUnavailable
	}
	r, err := os.OpenRoot(s.Root)
	if err != nil {
		return nil, "", ErrManifestUnavailable
	}
	return r, filepath.Join("hcg", "reporting", id.namespace()), nil
}

func (s ManifestStore) Load(ctx context.Context, id Identity) (Manifest, error) {
	if err := ctx.Err(); err != nil {
		return Manifest{}, err
	}
	r, dir, err := s.open(id)
	if err != nil {
		return Manifest{}, err
	}
	defer r.Close()
	f, err := r.Open(filepath.Join(dir, "manifest.json"))
	if err != nil {
		return Manifest{}, ErrManifestUnavailable
	}
	defer f.Close()
	var m Manifest
	d := json.NewDecoder(io.LimitReader(f, 64<<10))
	if d.Decode(&m) != nil || d.Decode(new(any)) != io.EOF || m.SchemaVersion != ManifestVersion || m.ConfigDigest == "" || m.MountDigest == "" || m.Identity.DataRoot != s.Root || m.Namespace != id.namespace() || m.Identity.SessionID != id.SessionID || m.Identity.ProjectID != id.ProjectID {
		return Manifest{}, ErrManifestUnavailable
	}
	return m, nil
}

func (s ManifestStore) Save(ctx context.Context, m Manifest) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	r, dir, err := s.open(m.Identity)
	if err != nil {
		return err
	}
	defer r.Close()
	if m.SchemaVersion != ManifestVersion || m.Namespace != m.Identity.namespace() {
		return ErrManifestUnavailable
	}
	if err := r.MkdirAll(dir, 0700); err != nil {
		return ErrManifestUnavailable
	}
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return atomicJSON(ctx, r, filepath.Join(dir, "manifest.json"), b)
}

func atomicJSON(ctx context.Context, r *os.Root, path string, b []byte) error {
	// Exclusive temporary names avoid clobbering another writer's staging file.
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return ErrManifestUnavailable
	}
	name := filepath.Join(filepath.Dir(path), ".reporting-"+hex.EncodeToString(nonce[:]))
	tmp, err := r.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return ErrManifestUnavailable
	}
	defer r.Remove(name)
	if _, err = tmp.Write(b); err == nil {
		err = tmp.Sync()
	}
	closeErr := tmp.Close()
	if err != nil || closeErr != nil || ctx.Err() != nil {
		return ErrManifestUnavailable
	}
	if err := r.Rename(name, path); err != nil {
		return ErrManifestUnavailable
	}
	return nil
}
