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

// AppDiagnosticsSnapshot is deliberately metadata-only until the collector schema lands in PR2.
type AppDiagnosticsSnapshot struct {
	SchemaVersion int       `json:"schemaVersion"`
	InstanceID    string    `json:"instanceId"`
	WindowID      string    `json:"windowId"`
	Generation    uint64    `json:"rendererGeneration"`
	CapturedAt    time.Time `json:"capturedAt"`
	Completeness  string    `json:"completeness"`
}
