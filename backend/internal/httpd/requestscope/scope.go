// Package requestscope carries transport facts that cannot be trusted from
// client-controlled headers. The LAN listener marks requests before they enter
// the shared API router.
package requestscope

import "context"

type lanKey struct{}

// WithLAN marks a request context as arriving through the authenticated LAN
// listener rather than the loopback desktop listener.
func WithLAN(ctx context.Context) context.Context {
	return context.WithValue(ctx, lanKey{}, true)
}

// IsLAN reports whether the physical LAN listener marked this request.
func IsLAN(ctx context.Context) bool {
	value, _ := ctx.Value(lanKey{}).(bool)
	return value
}

type rcaInterruptKey struct{}

// WithRCARawInterrupt supplies only the raw request's bounded read interrupter.
func WithRCARawInterrupt(ctx context.Context, interrupt func()) context.Context {
	return context.WithValue(ctx, rcaInterruptKey{}, interrupt)
}

// RCARawInterrupt is nil outside the physical raw transport guard.
func RCARawInterrupt(ctx context.Context) func() {
	f, _ := ctx.Value(rcaInterruptKey{}).(func())
	return f
}

type rcaFinishedKey struct{}

// WithRCARawFinished installs one transport-owned release registration.
func WithRCARawFinished(ctx context.Context, register func(func()) bool) context.Context {
	return context.WithValue(ctx, rcaFinishedKey{}, register)
}

// OnRCARawFinished registers a reader release after terminal close or a fully
// consumed success. A handed-off numeric writer keeps its independent slot.
func OnRCARawFinished(ctx context.Context, release func()) bool {
	register, _ := ctx.Value(rcaFinishedKey{}).(func(func()) bool)
	return register != nil && register(release)
}

// CredentialKind names what authenticated a request on the LAN listener.
type CredentialKind string

// The credentials the LAN listener accepts. A pairing code counts only for pairing.
const (
	CredentialPassword    CredentialKind = "password"
	CredentialAccount     CredentialKind = "account"
	CredentialDevice      CredentialKind = "device"
	CredentialPairingCode CredentialKind = "pairing-code"
)

// Credential is what the LAN listener accepted. DeviceID is set for CredentialDevice.
type Credential struct {
	Kind     CredentialKind
	DeviceID string
}

type credentialKey struct{}

// WithCredential records the credential the LAN listener accepted.
func WithCredential(ctx context.Context, credential Credential) context.Context {
	return context.WithValue(ctx, credentialKey{}, credential)
}

// CredentialFrom returns the accepted credential, or the zero Credential for a loopback request.
func CredentialFrom(ctx context.Context) Credential {
	credential, _ := ctx.Value(credentialKey{}).(Credential)
	return credential
}
