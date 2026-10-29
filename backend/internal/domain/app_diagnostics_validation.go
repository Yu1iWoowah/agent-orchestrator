package domain

import (
	"math"
	"regexp"
	"slices"
	"strings"
)

var diagIDPattern = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`)
var diagVersionPattern = regexp.MustCompile(`^\d+\.\d+\.\d+([.-][A-Za-z0-9.-]+)?$`)
var diagCommitPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)

func diagEnum(value, allowed string) bool { return slices.Contains(strings.Split(allowed, ","), value) }
func diagID(value string) bool            { return value == "" || diagIDPattern.MatchString(value) }
func diagNumber(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0) && math.Abs(value) <= 1e6
}
func diagCount(value int) bool { return value >= 0 && value <= 10000000 }
func diagProvenance(p AppDiagnosticsProvenance) bool {
	return diagEnum(p.Availability, "available,unavailable,not_mounted,unsupported_surface,not_recorded_before_enable,disabled") && !p.ObservedAt.IsZero() && diagCount(p.OmittedCount)
}
func diagIDs(ids []string) bool {
	if ids == nil || len(ids) > 200 {
		return false
	}
	for _, id := range ids {
		if id == "" || !diagID(id) {
			return false
		}
	}
	return true
}
func diagWorkspaces(rows []AppDiagnosticsWorkspace) bool {
	if rows == nil || len(rows) > 100 {
		return false
	}
	for _, row := range rows {
		if !diagEnum(row.Scope, "ids,counts") || (row.Scope == "counts" && len(row.Sessions) > 0) || row.ProjectID == "" || !diagID(row.ProjectID) || !diagID(row.HostID) || !diagEnum(row.Source, "local,remote,cloud") || !diagCount(row.SessionCount) || !diagCount(row.OmittedCount) || row.Sessions == nil || len(row.Sessions) > 200 {
			return false
		}
		for _, session := range row.Sessions {
			if session.ID == "" || !diagID(session.ID) || !diagEnum(session.Kind, "worker,orchestrator,unknown") || !diagEnum(session.Status, "spawning,working,pr_open,draft,ci_failed,review_pending,changes_requested,commented,approved,mergeable,ready_to_merge,merged,cleanup,done,exited,killed,stuck,needs_input,no_signal,idle,terminated,unknown") || !diagEnum(session.Activity, "active,idle,waiting_input,blocked,exited,unknown") || !diagEnum(session.Column, "building,validating,needs_review,needs_input,ready,done,archive,unknown") {
				return false
			}
		}
	}
	return true
}
func diagStyle(s AppDiagnosticsStyle) bool {
	if !diagNumber(s.Box.X) || !diagNumber(s.Box.Y) || !diagNumber(s.Box.Width) || !diagNumber(s.Box.Height) || s.Box.Width < 0 || s.Box.Height < 0 || !diagNumber(s.Opacity) || s.Opacity < 0 || s.Opacity > 1 || !diagNumber(s.ZIndex) {
		return false
	}
	if !diagEnum(s.Display, "none,block,flex,grid,inline,inline-block,inline-flex,inline-grid,contents,table,other") || !diagEnum(s.Visibility, "visible,hidden,collapse,other") || !diagEnum(s.OverflowX, "visible,hidden,clip,auto,scroll,other") || !diagEnum(s.OverflowY, "visible,hidden,clip,auto,scroll,other") || !diagEnum(s.Clip, "none,inset,other") || s.GridRows == nil || len(s.GridRows) > 64 || s.Transform == nil || len(s.Transform) > 16 {
		return false
	}
	for _, value := range append(append([]float64{}, s.GridRows...), s.Transform...) {
		if !diagNumber(value) {
			return false
		}
	}
	return true
}

// ValidAppDiagnosticsSnapshot checks the redaction contract after strict decoding.
func ValidAppDiagnosticsSnapshot(s AppDiagnosticsSnapshot) bool {
	if s.SchemaVersion != 1 || s.InstanceID == "" || !diagID(s.InstanceID) || s.WindowID == "" || !diagID(s.WindowID) || s.Generation == 0 || s.CapturedAt.IsZero() || !diagEnum(s.Completeness, "partial,complete") {
		return false
	}
	if i := s.Identity; i != nil {
		if (i.AppVersion != "unknown" && !diagVersionPattern.MatchString(i.AppVersion)) || (i.DaemonVersion != "unknown" && !diagVersionPattern.MatchString(i.DaemonVersion)) || (i.BuildCommit != "unknown" && !diagCommitPattern.MatchString(i.BuildCommit)) || !diagEnum(i.OS, "linux,darwin,win32,unknown") {
			return false
		}
	}
	c := s.Collector
	// Retained PR1 metadata-only records remain readable and explicitly partial.
	if c == nil {
		return s.Completeness == "partial"
	}
	if s.Identity == nil || c.RedactionSchemaVersion != 1 || c.CaptureStartedAt.IsZero() || c.CaptureFinishedAt.Before(c.CaptureStartedAt) || !diagEnum(c.Consistency, "stable,changed") {
		return false
	}
	for _, p := range []AppDiagnosticsProvenance{c.Route.AppDiagnosticsProvenance, c.Shell.AppDiagnosticsProvenance, c.Cache.AppDiagnosticsProvenance, c.Components.AppDiagnosticsProvenance, c.Geometry.AppDiagnosticsProvenance, c.Errors.AppDiagnosticsProvenance} {
		if !diagProvenance(p) {
			return false
		}
		if s.Completeness == "complete" && (p.Availability != "available" || p.OmittedCount > 0 || c.Consistency != "stable") {
			return false
		}
	}
	if !diagEnum(c.Route.RouteID, "home,board,session,settings,terminal,unsupported_surface") || !diagEnum(c.Route.Source, "local,remote,cloud,unknown") || !diagID(c.Route.ProjectID) || !diagID(c.Route.SessionID) || !diagID(c.Route.HostID) {
		return false
	}
	if !diagNumber(c.Shell.Width) || !diagNumber(c.Shell.Height) || c.Shell.Width < 0 || c.Shell.Height < 0 || !diagNumber(c.Shell.SidebarWidth) || c.Shell.SidebarWidth < 0 || !diagNumber(c.Shell.DevicePixelRatio) || c.Shell.DevicePixelRatio <= 0 || c.Shell.DevicePixelRatio > 16 {
		return false
	}
	if !diagCount(c.Geometry.AncestorTruncatedCount) || c.Cache.Queries == nil || len(c.Cache.Queries) > 100 || c.Components.Entries == nil || len(c.Components.Entries) > 102 || c.Geometry.Nodes == nil || len(c.Geometry.Nodes) > 1000 || c.Errors.Records == nil || len(c.Errors.Records) > 100 || c.Errors.EnabledAt.IsZero() || !c.Errors.NotRecordedBeforeEnable {
		return false
	}
	for _, q := range c.Cache.Queries {
		if !diagEnum(q.Kind, "workspaces,cloud-projects,cloud-sessions,cloud-shared-workspaces,remote-workspaces,remote-projects,remote-sessions") || !diagID(q.HostID) || !diagEnum(q.Status, "pending,error,success") || !diagEnum(q.FetchStatus, "fetching,paused,idle") || q.DataUpdatedAt < 0 || q.DataUpdatedAt > 2e13 || q.ErrorUpdatedAt < 0 || q.ErrorUpdatedAt > 2e13 || !diagCount(q.ObserverCount) || !diagCount(q.FailureCount) || !diagCount(q.Count) || !diagWorkspaces(q.Workspaces) || (q.Kind != "workspaces" && len(q.Workspaces) != 0) {
			return false
		}
	}
	for _, entry := range c.Components.Entries {
		if !diagCount(entry.OmittedCount) || !diagEnum(entry.Kind, "sidebar,project,board,session,chat,terminal,error-boundary") || entry.Revision == 0 || entry.ObservedAt.IsZero() || !diagID(entry.ProjectID) || !diagID(entry.HostID) || !diagEnum(entry.Source, "local,remote,cloud") || !diagCount(entry.SessionCount) || !diagCount(entry.HiddenCount) || !diagIDs(entry.LiveIDs) || !diagIDs(entry.WorkerIDs) || !diagIDs(entry.OrderedIDs) || !diagIDs(entry.ListedIDs) || !diagWorkspaces(entry.Workspaces) {
			return false
		}
	}
	for _, node := range c.Geometry.Nodes {
		if !diagEnum(node.Source, "local,remote,cloud") || !diagID(node.HostID) || node.ID == "" || !diagID(node.ID) || !diagID(node.ProjectID) || !diagID(node.SessionID) || !diagEnum(node.Role, "sidebar,project_root,session_list,session_row,main_pane") || !diagEnum(node.Availability, "available,not_mounted,unavailable") || !diagEnum(node.Hit, "clear,registered,unregistered_overlay,not_tested") || node.Ancestors == nil || len(node.Ancestors) > 8 {
			return false
		}
		if node.Availability == "available" && node.Style == nil {
			return false
		}
		if node.Style != nil && !diagStyle(*node.Style) {
			return false
		}
		for _, style := range node.Ancestors {
			if !diagStyle(style) {
				return false
			}
		}
	}
	for _, row := range c.Errors.Records {
		if row.At.IsZero() || !diagEnum(row.Source, "console_error,window_error,unhandled_rejection") || !diagEnum(row.Name, "Error,TypeError,RangeError,ReferenceError,SyntaxError,Unknown") || row.Message != "[redacted]" || row.Count < 1 || !diagCount(row.Count) {
			return false
		}
	}
	return true
}

// ValidAppDiagnosticsCaptureSnapshot requires the full live contract; only stored PR1 records may omit it.
func ValidAppDiagnosticsCaptureSnapshot(s AppDiagnosticsSnapshot) bool {
	return !s.liveFieldsMissing && s.Collector != nil && s.Identity != nil && ValidAppDiagnosticsSnapshot(s)
}
