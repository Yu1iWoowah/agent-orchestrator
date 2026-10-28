// Package documentread provides side-effect-free discovery of registered AO documents.
package documentread

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// Store reads durable registration facts without reconciling sessions.
type Store interface {
	GetProject(context.Context, string) (domain.ProjectRecord, bool, error)
	ListProjects(context.Context) ([]domain.ProjectRecord, error)
	ListSessions(context.Context, domain.ProjectID) ([]domain.SessionRecord, error)
}

// SharedRoots locates validated living-document roots without taking a write lease.
type SharedRoots interface {
	ReadRoot(context.Context, domain.ProjectID) (string, error)
}

// Deps supplies registered sources and the daemon data directory.
type Deps struct {
	Store   Store
	Shared  SharedRoots
	DataDir string
}

// Service resolves managed locations and builds bounded metadata catalogs.
type Service struct {
	store   Store
	shared  SharedRoots
	dataDir string
}

// New constructs the daemon document read service.
func New(d Deps) *Service { return &Service{store: d.Store, shared: d.Shared, dataDir: d.DataDir} }

// ArtifactTarget identifies a file owned by one registered session.
type ArtifactTarget struct {
	Kind      string  `json:"kind" enum:"artifact"`
	ProjectID string  `json:"projectId"`
	SessionID string  `json:"sessionId"`
	Path      string  `json:"path"`
	Fragment  *string `json:"fragment,omitempty"`
}

// SharedTarget identifies a living project document independently of a session.
type SharedTarget struct {
	Kind      string  `json:"kind" enum:"shared"`
	ProjectID string  `json:"projectId"`
	Path      string  `json:"path"`
	Fragment  *string `json:"fragment,omitempty"`
}

// LocalPathTarget is an untrusted absolute path or local file URL.
type LocalPathTarget struct {
	Kind  string `json:"kind" enum:"local-path"`
	Value string `json:"value"`
}
type managedTarget interface{ managedDocument() }

func (ArtifactTarget) managedDocument() {}
func (SharedTarget) managedDocument()   {}

// Target is a closed union; a shared target cannot carry a session ID.
type Target struct{ value managedTarget }

// MarshalJSON encodes the managed source variant.
func (t Target) MarshalJSON() ([]byte, error) {
	if t.value == nil {
		return nil, fmt.Errorf("missing document target")
	}
	return json.Marshal(t.value)
}

// JSONSchemaOneOf describes the managed source union.
func (Target) JSONSchemaOneOf() []interface{} { return []interface{}{ArtifactTarget{}, SharedTarget{}} }

// Input parses the external union once, rejecting fields belonging to another variant.
type Input struct {
	Kind  string `json:"kind" enum:"artifact,shared,local-path"`
	value any
}

// InlineJSONSchema embeds this variant in source unions.
func (ArtifactTarget) InlineJSONSchema() {}

// InlineJSONSchema embeds this variant in source unions.
func (SharedTarget) InlineJSONSchema() {}

// InlineJSONSchema embeds this variant in source unions.
func (LocalPathTarget) InlineJSONSchema() {}

// UnmarshalJSON validates exactly one source variant and rejects unknown fields.
func (i *Input) UnmarshalJSON(b []byte) error {
	var tag struct {
		Kind string `json:"kind"`
	}
	if e := json.Unmarshal(b, &tag); e != nil {
		return e
	}
	var v any
	switch tag.Kind {
	case "artifact":
		v = &ArtifactTarget{}
	case "shared":
		v = &SharedTarget{}
	case "local-path":
		v = &LocalPathTarget{}
	default:
		return fmt.Errorf("unsupported document kind")
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if e := d.Decode(v); e != nil {
		return e
	}
	if e := d.Decode(new(any)); e != io.EOF {
		return fmt.Errorf("one document target is required")
	}
	i.Kind = tag.Kind
	switch t := v.(type) {
	case *ArtifactTarget:
		i.value = *t
	case *SharedTarget:
		i.value = *t
	case *LocalPathTarget:
		i.value = *t
	}
	return nil
}

// MarshalJSON preserves the parsed request variant.
func (i Input) MarshalJSON() ([]byte, error) { return json.Marshal(i.value) }

// JSONSchemaOneOf describes the accepted request variants.
func (Input) JSONSchemaOneOf() []interface{} {
	return []interface{}{ArtifactTarget{}, SharedTarget{}, LocalPathTarget{}}
}

// Resolved contains verified local metadata and never file contents.
type Resolved struct {
	Target       Target     `json:"target"`
	CanonicalURI string     `json:"canonicalUri"`
	LocalPath    string     `json:"localPath"`
	Size         int64      `json:"size"`
	MediaType    string     `json:"mediaType"`
	UpdatedAt    *time.Time `json:"updatedAt,omitempty"`
	Fingerprint  string     `json:"fingerprint" description:"Metadata validator, not a content hash."`
}

// CatalogQuery selects source, archive state and a revision-bound page.
type CatalogQuery struct {
	Source  string `query:"source" enum:"all,worker,orchestrator,shared"`
	Archive string `query:"archive" enum:"all,current,archive"`
	Cursor  string `query:"cursor"`
	Limit   int    `query:"limit" minimum:"1" maximum:"200"`
}

// CatalogItem is one source-qualified file; it contains no physical path.
type CatalogItem struct {
	Target      Target     `json:"target"`
	Path        string     `json:"path"`
	Name        string     `json:"name"`
	Source      string     `json:"source" enum:"worker,orchestrator,shared"`
	Archived    bool       `json:"archived"`
	Size        int64      `json:"size"`
	UpdatedAt   *time.Time `json:"updatedAt,omitempty"`
	Fingerprint string     `json:"fingerprint"`
}

// SourceError reports an incomplete source without exposing filesystem details.
type SourceError struct {
	Source    string `json:"source" enum:"worker,orchestrator,shared"`
	SessionID string `json:"sessionId,omitempty"`
	Code      string `json:"code"`
}

// Catalog contains one metadata page and explicit source failures.
type Catalog struct {
	Items      []CatalogItem `json:"items"`
	Errors     []SourceError `json:"errors"`
	Incomplete bool          `json:"incomplete"`
	NextCursor string        `json:"nextCursor,omitempty"`
	Revision   string        `json:"revision"`
}
