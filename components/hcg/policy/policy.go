// Package policy contains the fixed project/role/session policy mechanism shared
// by named consumers. It has no engine registry, filesystem or launch behavior.
package policy

type Inputs struct {
	Project       *bool `json:"project"`
	Role          *bool `json:"role"`
	Session       *bool `json:"session"`
	EmergencyStop bool  `json:"emergencyStop"`
}

type Policy struct {
	Enabled bool   `json:"enabled"`
	Source  string `json:"source"`
}

func Resolve(role string, in Inputs) Policy {
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

// Manifest is the policy-only portion of a consumer's owned metadata. Storage,
// native mounting and source authority stay with their respective consumers.
type Manifest struct {
	Revision uint64 `json:"revision"`
	Inputs   Inputs `json:"inputs"`
	Policy   Policy `json:"policy"`
}

// Revise preserves the common monotonic revision rule. Reconnect is deliberately
// not modeled here: reporting's immutable launch mounts remain its own boundary.
func Revise(previous *Manifest, role string, in Inputs) Manifest {
	revision := uint64(1)
	if previous != nil {
		revision = previous.Revision + 1
	}
	return Manifest{revision, in, Resolve(role, in)}
}
