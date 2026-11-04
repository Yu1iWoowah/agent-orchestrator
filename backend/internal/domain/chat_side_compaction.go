package domain

import (
	"encoding/json"
	"errors"
)

type SideCompactionState string

const (
	SideCompactionPending SideCompactionState = "pending"
	SideCompactionSettled SideCompactionState = "settled"
	SideCompactionNotSent SideCompactionState = "not_sent"
	SideCompactionUnknown SideCompactionState = "unknown"
)

var ErrSideCompactionTransition = errors.New("invalid side compaction transition")

// CompactionProcessScope is reported by the original host, not inferred from
// a replacement daemon's process or launch configuration.
type CompactionProcessScope struct {
	HostPID, ProviderPGID, LeaderPID int
	HostBirth, LeaderBirth           string
	BootID, PIDNamespace             string
}

// SideCompaction is the durable obligation; Content is stored separately so
// explicit close can discard bodies while retaining the original metadata.
type SideCompaction struct {
	ReadOnly                                bool
	ID, SideID, NativeHome, NativeID, Epoch string
	SessionID                               SessionID
	Provider                                AgentHarness
	PermissionMode                          PermissionMode
	Scope                                   *CompactionProcessScope
	State                                   SideCompactionState
	Historical, Closed                      bool
	Content                                 json.RawMessage `json:"-"`
}

func (r SideCompaction) Unresolved() bool {
	return !r.Closed && (r.State == SideCompactionPending || r.State == SideCompactionUnknown)
}

func (r SideCompaction) Transition(state SideCompactionState, historical, closed bool) (SideCompaction, error) {
	valid := state == SideCompactionPending || state == SideCompactionUnknown || state == SideCompactionSettled || state == SideCompactionNotSent
	if !valid || (historical && state != SideCompactionUnknown) || (r.Historical && (!historical || state != r.State)) || (r.Closed && !closed) ||
		(r.State != state && !r.Unresolved()) || (r.State == SideCompactionUnknown && state == SideCompactionPending) {
		return r, ErrSideCompactionTransition
	}
	r.State, r.Historical, r.Closed = state, historical, closed
	return r, nil
}
