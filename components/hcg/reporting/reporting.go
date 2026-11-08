// Package reporting is AO's narrow entry point for the fixed optional
// llm-usage-metrics consumer. It exposes no arbitrary commands or environments.
package reporting

import (
	"context"
	"os/exec"
	"time"

	"github.com/aoagents/agent-orchestrator/components/hcg/internal/integration"
)

const EngineName = "llm-usage-metrics"
const EngineVersion = "0.14.3"

type Inputs struct {
	Project       *bool `json:"project"`
	Role          *bool `json:"role"`
	Session       *bool `json:"session"`
	EmergencyStop bool  `json:"emergencyStop"`
}

// Identity and NativeFile are constructed by AO from its owned session/source
// records. HTTP callers must never supply source paths or native ownership.
type Identity struct {
	DataRoot             string
	ProjectID            string
	SessionID            string
	Role                 string
	Mode                 string
	Harness              string
	Worktree             string
	ArtifactDir          string
	NativeID             string
	LaunchID             string
	ControllerGeneration string
}

type NativeFile struct {
	Root     string
	Path     string
	Source   string
	NativeID string
}

type View struct {
	ProjectID        string `json:"projectId"`
	SessionID        string `json:"sessionId"`
	Inputs           Inputs `json:"inputs"`
	Source           string `json:"source"`
	SessionOverride  string `json:"sessionOverride"`
	EffectiveEnabled bool   `json:"effectiveEnabled"`
	EmergencyStop    bool   `json:"emergencyStop"`
	Requested        bool   `json:"requested"`
	Active           bool   `json:"active"`
	Available        bool   `json:"available"`
	Reason           string `json:"reason"`
	EngineVersion    string `json:"engineVersion"`
	ManifestRevision uint64 `json:"manifestRevision"`
}

type EngineStatus struct {
	Engine    string `json:"engine"`
	Version   string `json:"version"`
	Available bool   `json:"available"`
	Reason    string `json:"reason"`
}

type Event struct {
	At                  time.Time `json:"at"`
	InputTokens         int64     `json:"inputTokens"`
	CacheReadTokens     int64     `json:"cacheReadTokens"`
	CacheWriteTokens    int64     `json:"cacheWriteTokens"`
	InputIncludingCache int64     `json:"inputIncludingCache"`
	OutputTokens        int64     `json:"outputTokens"`
	ReasoningTokens     int64     `json:"reasoningTokens"`
	EngineTotalTokens   int64     `json:"engineTotalTokens"`
}

type ToolCounts struct {
	Category string `json:"category"`
	Calls    int64  `json:"calls"`
	Errors   int64  `json:"errors"`
	Unknown  int64  `json:"unknown"`
}

type Snapshot struct {
	Source             string       `json:"source"`
	Engine             string       `json:"engine"`
	EngineVersion      string       `json:"engineVersion"`
	ProjectID          string       `json:"projectId"`
	SessionID          string       `json:"sessionId"`
	NativeID           string       `json:"nativeId"`
	ObservedAt         time.Time    `json:"observedAt"`
	Status             string       `json:"status"`
	Reason             string       `json:"reason"`
	BucketAvailability string       `json:"bucketAvailability"`
	Events             []Event      `json:"events"`
	Tools              []ToolCounts `json:"tools"`
}

type Consumer struct{ core *integration.Consumer }

func New(dataRoot string) (*Consumer, error) {
	c, err := integration.NewConsumer(dataRoot)
	if err != nil {
		return nil, err
	}
	return &Consumer{core: c}, nil
}

func (c *Consumer) Probe(ctx context.Context) EngineStatus {
	return EngineStatus(c.core.Probe(ctx))
}

func (c *Consumer) Register(ctx context.Context, executable, version string) (EngineStatus, error) {
	// Runtime resolution occurs only during explicit deployment registration;
	// the resulting absolute Node path/digest is persisted. Reports never use PATH.
	node, err := exec.LookPath("node")
	if err != nil {
		return EngineStatus{Engine: EngineName, Reason: "runtime_unavailable"}, err
	}
	status, err := c.core.Register(ctx, executable, node, version)
	return EngineStatus(status), err
}

func (c *Consumer) EmergencyStop(ctx context.Context, stop bool) error {
	return c.core.EmergencyStop(ctx, stop)
}

func (c *Consumer) ProjectPolicyChanged(projectID string) {
	c.core.ProjectPolicyChanged(projectID)
}

func (c *Consumer) ObserveLaunch(ctx context.Context, id Identity, inputs Inputs, fresh bool) error {
	if fresh {
		return c.core.Launch(ctx, integration.Identity(id), integration.PolicyInputs(inputs))
	}
	return c.core.Reconnect(ctx, integration.Identity(id), integration.PolicyInputs(inputs))
}

func (c *Consumer) CancelSessionReports(sessionID string) {
	c.core.CancelSessionReports(sessionID)
}

func (c *Consumer) SetOverride(ctx context.Context, id Identity, inputs Inputs, enabled *bool) error {
	return c.core.SetOverride(ctx, integration.Identity(id), integration.PolicyInputs(inputs), enabled)
}

func (c *Consumer) Get(ctx context.Context, id Identity, inputs Inputs) View {
	v := c.core.Get(ctx, integration.Identity(id), integration.PolicyInputs(inputs))
	return View{v.ProjectID, v.SessionID, Inputs(v.Inputs), v.Source, v.SessionOverride, v.EffectiveEnabled, v.EmergencyStop, v.Requested, v.Active, v.Available, v.Reason, v.EngineVersion, v.ManifestRevision}
}

func (c *Consumer) Report(ctx context.Context, id Identity, inputs Inputs, files []NativeFile) Snapshot {
	sources := make([]integration.AuthorizedSource, len(files))
	for i, f := range files {
		sources[i] = integration.AuthorizedSource(f)
	}
	s := c.core.Report(ctx, integration.Identity(id), integration.PolicyInputs(inputs), sources)
	out := Snapshot{Source: s.Source, Engine: s.Engine, EngineVersion: s.EngineVersion, ProjectID: s.ProjectID, SessionID: s.SessionID, NativeID: s.NativeID, ObservedAt: s.ObservedAt, Status: s.Status, Reason: s.Reason, BucketAvailability: s.BucketAvailability, Events: []Event{}, Tools: []ToolCounts{}}
	for _, e := range s.Events {
		out.Events = append(out.Events, Event(e))
	}
	for _, t := range s.Tools {
		out.Tools = append(out.Tools, ToolCounts(t))
	}
	return out
}
