package domain

import "time"

// AppDiagnosticsError carries a fixed safe code; no payload, token or filesystem error is echoed.
type AppDiagnosticsError struct {
	Code   string
	Status int
}

func (e *AppDiagnosticsError) Error() string { return e.Code }

// AppDiagnosticsWindow identifies an app shell document, never a Browser tab.
type AppDiagnosticsWindow struct {
	ID         string `json:"id"`
	Generation uint64 `json:"generation"`
}

// AppDiagnosticsRuntimeState is copied by the independent authenticated desktop channel.
type AppDiagnosticsRuntimeState struct {
	InstanceID string                 `json:"instanceId"`
	Epoch      string                 `json:"epoch"`
	Ready      bool                   `json:"ready"`
	Windows    []AppDiagnosticsWindow `json:"windows"`
}

// AppDiagnosticsCaptureRequest has no selector, JavaScript, path, or remote host field.
type AppDiagnosticsCaptureRequest struct {
	WindowID  string `json:"windowId,omitempty"`
	ProjectID string `json:"projectId,omitempty"`
}

// AppDiagnosticsSnapshot contains only the explicitly versioned, content-free projection.
type AppDiagnosticsSnapshot struct {
	SchemaVersion int                      `json:"schemaVersion"`
	InstanceID    string                   `json:"instanceId"`
	WindowID      string                   `json:"windowId"`
	Generation    uint64                   `json:"rendererGeneration"`
	CapturedAt    time.Time                `json:"capturedAt"`
	Completeness  string                   `json:"completeness"`
	Collector     *AppDiagnosticsCollector `json:"collector,omitempty"`
	Identity      *AppDiagnosticsIdentity  `json:"identity,omitempty"`
}

// AppDiagnosticsIdentity never includes executable paths, account names or environment.
type AppDiagnosticsIdentity struct {
	AppVersion    string `json:"appVersion"`
	DaemonVersion string `json:"daemonVersion"`
	BuildCommit   string `json:"buildCommit"`
	OS            string `json:"os"`
}

// AppDiagnosticsProvenance is copied with every section, including unavailable sections.
type AppDiagnosticsProvenance struct {
	Availability string    `json:"availability"`
	ObservedAt   time.Time `json:"observedAt"`
	Revision     uint64    `json:"revision"`
	OmittedCount int       `json:"omittedCount"`
}

// AppDiagnosticsCollector is an allowlist, not a DOM dump or a query-cache export.
type AppDiagnosticsCollector struct {
	RedactionSchemaVersion int                      `json:"redactionSchemaVersion"`
	CaptureStartedAt       time.Time                `json:"captureStartedAt"`
	CaptureFinishedAt      time.Time                `json:"captureFinishedAt"`
	Consistency            string                   `json:"consistency"`
	Route                  AppDiagnosticsRoute      `json:"route"`
	Shell                  AppDiagnosticsShell      `json:"shell"`
	Cache                  AppDiagnosticsCache      `json:"cache"`
	Components             AppDiagnosticsComponents `json:"components"`
	Geometry               AppDiagnosticsGeometry   `json:"geometry"`
	Errors                 AppDiagnosticsErrors     `json:"errors"`
}

// AppDiagnosticsRoute contains canonical route and selection ids without a URL.
type AppDiagnosticsRoute struct {
	AppDiagnosticsProvenance
	RouteID   string `json:"routeId"`
	Source    string `json:"source"`
	ProjectID string `json:"projectId"`
	SessionID string `json:"sessionId"`
	HostID    string `json:"hostId"`
}

// AppDiagnosticsShell records viewport and shell layout without window titles.
type AppDiagnosticsShell struct {
	AppDiagnosticsProvenance
	Width            float64 `json:"width"`
	Height           float64 `json:"height"`
	DevicePixelRatio float64 `json:"devicePixelRatio"`
	SidebarCollapsed bool    `json:"sidebarCollapsed"`
	SidebarWidth     float64 `json:"sidebarWidth"`
	InspectorOpen    bool    `json:"inspectorOpen"`
}

// AppDiagnosticsSession projects only session identity and fixed lifecycle values.
type AppDiagnosticsSession struct {
	ID         string `json:"id"`
	Kind       string `json:"kind"`
	Terminated bool   `json:"terminated"`
	Status     string `json:"status"`
	Activity   string `json:"activity"`
	Column     string `json:"column"`
}

// AppDiagnosticsWorkspace separates source identity and counts from scoped session ids.
type AppDiagnosticsWorkspace struct {
	Scope         string                  `json:"scope"`
	ProjectID     string                  `json:"projectId"`
	HostID        string                  `json:"hostId"`
	Source        string                  `json:"source"`
	Shared        bool                    `json:"shared"`
	FolderMissing bool                    `json:"folderMissing"`
	SessionCount  int                     `json:"sessionCount"`
	Sessions      []AppDiagnosticsSession `json:"sessions"`
	OmittedCount  int                     `json:"omittedCount"`
}

// AppDiagnosticsQuery records allowlisted query metadata without its raw key or data.
type AppDiagnosticsQuery struct {
	Kind           string                    `json:"kind"`
	HostID         string                    `json:"hostId"`
	Status         string                    `json:"status"`
	FetchStatus    string                    `json:"fetchStatus"`
	Invalidated    bool                      `json:"invalidated"`
	DataUpdatedAt  float64                   `json:"dataUpdatedAt"`
	ErrorUpdatedAt float64                   `json:"errorUpdatedAt"`
	ObserverCount  int                       `json:"observerCount"`
	FailureCount   int                       `json:"failureCount"`
	Count          int                       `json:"count"`
	Workspaces     []AppDiagnosticsWorkspace `json:"workspaces"`
}

// AppDiagnosticsCache contains bounded query observations.
type AppDiagnosticsCache struct {
	AppDiagnosticsProvenance
	Queries []AppDiagnosticsQuery `json:"queries"`
}

// AppDiagnosticsComponent records committed component inputs, never arbitrary props.
type AppDiagnosticsComponent struct {
	OmittedCount int                       `json:"omittedCount"`
	Kind         string                    `json:"kind"`
	Revision     uint64                    `json:"revision"`
	ObservedAt   time.Time                 `json:"observedAt"`
	ProjectID    string                    `json:"projectId"`
	HostID       string                    `json:"hostId"`
	Source       string                    `json:"source"`
	Expanded     bool                      `json:"expanded"`
	Shared       bool                      `json:"shared"`
	ShowAll      bool                      `json:"showAll"`
	SessionCount int                       `json:"sessionCount"`
	HiddenCount  int                       `json:"hiddenCount"`
	LiveIDs      []string                  `json:"liveIds"`
	WorkerIDs    []string                  `json:"workerIds"`
	OrderedIDs   []string                  `json:"orderedIds"`
	ListedIDs    []string                  `json:"listedIds"`
	Workspaces   []AppDiagnosticsWorkspace `json:"workspaces"`
}

// AppDiagnosticsComponents contains bounded committed observations.
type AppDiagnosticsComponents struct {
	AppDiagnosticsProvenance
	Entries []AppDiagnosticsComponent `json:"entries"`
}

// AppDiagnosticsBox records a viewport-relative rectangle in CSS pixels.
type AppDiagnosticsBox struct {
	X      float64 `json:"x"`
	Y      float64 `json:"y"`
	Width  float64 `json:"width"`
	Height float64 `json:"height"`
}

// AppDiagnosticsStyle contains fixed CSS enums and numeric geometry without CSS text.
type AppDiagnosticsStyle struct {
	Box        AppDiagnosticsBox `json:"box"`
	Display    string            `json:"display"`
	Visibility string            `json:"visibility"`
	Opacity    float64           `json:"opacity"`
	OverflowX  string            `json:"overflowX"`
	OverflowY  string            `json:"overflowY"`
	Clip       string            `json:"clip"`
	GridRows   []float64         `json:"gridRows"`
	Transform  []float64         `json:"transform"`
	ZIndex     float64           `json:"zIndex"`
}

// AppDiagnosticsNode describes a registered shell element without DOM attributes or text.
type AppDiagnosticsNode struct {
	Source             string                `json:"source"`
	HostID             string                `json:"hostId"`
	ID                 string                `json:"id"`
	Role               string                `json:"role"`
	ProjectID          string                `json:"projectId"`
	SessionID          string                `json:"sessionId"`
	Availability       string                `json:"availability"`
	Style              *AppDiagnosticsStyle  `json:"style,omitempty"`
	Ancestors          []AppDiagnosticsStyle `json:"ancestors"`
	IntersectsViewport bool                  `json:"intersectsViewport"`
	Hit                string                `json:"hit"`
}

// AppDiagnosticsGeometry contains bounded node and ancestor observations.
type AppDiagnosticsGeometry struct {
	AppDiagnosticsProvenance
	Nodes []AppDiagnosticsNode `json:"nodes"`
}

// AppDiagnosticsErrorSummary retains a fixed error class and redacted message only.
type AppDiagnosticsErrorSummary struct {
	At      time.Time `json:"at"`
	Source  string    `json:"source"`
	Name    string    `json:"name"`
	Message string    `json:"message"`
	Count   int       `json:"count"`
}

// AppDiagnosticsErrors contains a bounded ring recorded after explicit enablement.
type AppDiagnosticsErrors struct {
	AppDiagnosticsProvenance
	EnabledAt               time.Time                    `json:"enabledAt"`
	NotRecordedBeforeEnable bool                         `json:"notRecordedBeforeEnable"`
	Records                 []AppDiagnosticsErrorSummary `json:"records"`
}
