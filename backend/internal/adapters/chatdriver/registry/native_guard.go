package registry

import (
	"context"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

type guardedDriver struct {
	ports.ChatDriver
	guard ports.ChatNativeGuard
}

func (d *guardedDriver) Resume(ctx context.Context, cfg ports.ChatResumeConfig) (conv ports.ChatConversation, err error) {
	cfg.ReconnectOnly = false
	err = d.guard.Run(ctx, d.Harness(), cfg, func() error { conv, err = d.ChatDriver.Resume(ctx, cfg); return err })
	return
}
func (d *guardedDriver) reconnect(ctx context.Context, cfg ports.ChatResumeConfig) (conv ports.ChatConversation, err error) {
	cfg.ReconnectOnly = true
	err = d.guard.Run(ctx, d.Harness(), cfg, func() error { conv, err = d.ChatDriver.(ports.ChatDriverReconnector).Reconnect(ctx, cfg); return err })
	return
}
func (d *guardedDriver) fork(ctx context.Context, source, anchor string, cfg ports.ChatStartConfig) (conv ports.ChatConversation, err error) {
	if cfg.AcquireForkSource != nil {
		var release func()
		ctx, release, err = cfg.AcquireForkSource(ctx)
		if err != nil {
			return nil, err
		}
		defer release()
		cfg.AcquireForkSource = nil
	}
	key := ports.ChatResumeConfig{ProviderConversationID: source, Env: cfg.Env, WorkspacePath: cfg.WorkspacePath, DataDir: cfg.DataDir}
	err = d.guard.Run(ctx, d.Harness(), key, func() error {
		conv, err = d.ChatDriver.(ports.ChatIsolatedForker).ForkIntoHost(ctx, source, anchor, cfg)
		return err
	})
	return
}

type guardedReconnect struct{ d *guardedDriver }

func (r guardedReconnect) Reconnect(ctx context.Context, cfg ports.ChatResumeConfig) (ports.ChatConversation, error) {
	return r.d.reconnect(ctx, cfg)
}

type guardedFork struct{ d *guardedDriver }

func (f guardedFork) ForkIntoHost(ctx context.Context, source, anchor string, cfg ports.ChatStartConfig) (ports.ChatConversation, error) {
	return f.d.fork(ctx, source, anchor, cfg)
}

// Embed precisely the optional interfaces the original driver advertises.
func guardDriver(original ports.ChatDriver, guard ports.ChatNativeGuard) ports.ChatDriver {
	d := &guardedDriver{original, guard}
	r, hasR := original.(ports.ChatDriverReconnector)
	f, hasF := original.(ports.ChatIsolatedForker)
	i, hasI := original.(ports.ChatInstallChecker)
	p, hasP := original.(ports.ChatPermissionModeChecker)
	v, hasV := original.(ports.NativeCheckpointVerifier)
	if hasR {
		r = guardedReconnect{d}
	}
	if hasF {
		f = guardedFork{d}
	}
	bits := 0
	if hasR {
		bits |= 1
	}
	if hasF {
		bits |= 2
	}
	if hasI {
		bits |= 4
	}
	if hasP {
		bits |= 8
	}
	if hasV {
		bits |= 16
	}
	switch bits {
	case 1:
		return struct {
			ports.ChatDriver
			ports.ChatDriverReconnector
		}{d, r}
	case 2:
		return struct {
			ports.ChatDriver
			ports.ChatIsolatedForker
		}{d, f}
	case 3:
		return struct {
			ports.ChatDriver
			ports.ChatDriverReconnector
			ports.ChatIsolatedForker
		}{d, r, f}
	case 4:
		return struct {
			ports.ChatDriver
			ports.ChatInstallChecker
		}{d, i}
	case 5:
		return struct {
			ports.ChatDriver
			ports.ChatDriverReconnector
			ports.ChatInstallChecker
		}{d, r, i}
	case 6:
		return struct {
			ports.ChatDriver
			ports.ChatIsolatedForker
			ports.ChatInstallChecker
		}{d, f, i}
	case 7:
		return struct {
			ports.ChatDriver
			ports.ChatDriverReconnector
			ports.ChatIsolatedForker
			ports.ChatInstallChecker
		}{d, r, f, i}
	case 8:
		return struct {
			ports.ChatDriver
			ports.ChatPermissionModeChecker
		}{d, p}
	case 9:
		return struct {
			ports.ChatDriver
			ports.ChatDriverReconnector
			ports.ChatPermissionModeChecker
		}{d, r, p}
	case 10:
		return struct {
			ports.ChatDriver
			ports.ChatIsolatedForker
			ports.ChatPermissionModeChecker
		}{d, f, p}
	case 11:
		return struct {
			ports.ChatDriver
			ports.ChatDriverReconnector
			ports.ChatIsolatedForker
			ports.ChatPermissionModeChecker
		}{d, r, f, p}
	case 12:
		return struct {
			ports.ChatDriver
			ports.ChatInstallChecker
			ports.ChatPermissionModeChecker
		}{d, i, p}
	case 13:
		return struct {
			ports.ChatDriver
			ports.ChatDriverReconnector
			ports.ChatInstallChecker
			ports.ChatPermissionModeChecker
		}{d, r, i, p}
	case 14:
		return struct {
			ports.ChatDriver
			ports.ChatIsolatedForker
			ports.ChatInstallChecker
			ports.ChatPermissionModeChecker
		}{d, f, i, p}
	case 15:
		return struct {
			ports.ChatDriver
			ports.ChatDriverReconnector
			ports.ChatIsolatedForker
			ports.ChatInstallChecker
			ports.ChatPermissionModeChecker
		}{d, r, f, i, p}
	case 16:
		return struct {
			ports.ChatDriver
			ports.NativeCheckpointVerifier
		}{d, v}
	case 17:
		return struct {
			ports.ChatDriver
			ports.ChatDriverReconnector
			ports.NativeCheckpointVerifier
		}{d, r, v}
	case 18:
		return struct {
			ports.ChatDriver
			ports.ChatIsolatedForker
			ports.NativeCheckpointVerifier
		}{d, f, v}
	case 19:
		return struct {
			ports.ChatDriver
			ports.ChatDriverReconnector
			ports.ChatIsolatedForker
			ports.NativeCheckpointVerifier
		}{d, r, f, v}
	case 20:
		return struct {
			ports.ChatDriver
			ports.ChatInstallChecker
			ports.NativeCheckpointVerifier
		}{d, i, v}
	case 21:
		return struct {
			ports.ChatDriver
			ports.ChatDriverReconnector
			ports.ChatInstallChecker
			ports.NativeCheckpointVerifier
		}{d, r, i, v}
	case 22:
		return struct {
			ports.ChatDriver
			ports.ChatIsolatedForker
			ports.ChatInstallChecker
			ports.NativeCheckpointVerifier
		}{d, f, i, v}
	case 23:
		return struct {
			ports.ChatDriver
			ports.ChatDriverReconnector
			ports.ChatIsolatedForker
			ports.ChatInstallChecker
			ports.NativeCheckpointVerifier
		}{d, r, f, i, v}
	case 24:
		return struct {
			ports.ChatDriver
			ports.ChatPermissionModeChecker
			ports.NativeCheckpointVerifier
		}{d, p, v}
	case 25:
		return struct {
			ports.ChatDriver
			ports.ChatDriverReconnector
			ports.ChatPermissionModeChecker
			ports.NativeCheckpointVerifier
		}{d, r, p, v}
	case 26:
		return struct {
			ports.ChatDriver
			ports.ChatIsolatedForker
			ports.ChatPermissionModeChecker
			ports.NativeCheckpointVerifier
		}{d, f, p, v}
	case 27:
		return struct {
			ports.ChatDriver
			ports.ChatDriverReconnector
			ports.ChatIsolatedForker
			ports.ChatPermissionModeChecker
			ports.NativeCheckpointVerifier
		}{d, r, f, p, v}
	case 28:
		return struct {
			ports.ChatDriver
			ports.ChatInstallChecker
			ports.ChatPermissionModeChecker
			ports.NativeCheckpointVerifier
		}{d, i, p, v}
	case 29:
		return struct {
			ports.ChatDriver
			ports.ChatDriverReconnector
			ports.ChatInstallChecker
			ports.ChatPermissionModeChecker
			ports.NativeCheckpointVerifier
		}{d, r, i, p, v}
	case 30:
		return struct {
			ports.ChatDriver
			ports.ChatIsolatedForker
			ports.ChatInstallChecker
			ports.ChatPermissionModeChecker
			ports.NativeCheckpointVerifier
		}{d, f, i, p, v}
	case 31:
		return struct {
			ports.ChatDriver
			ports.ChatDriverReconnector
			ports.ChatIsolatedForker
			ports.ChatInstallChecker
			ports.ChatPermissionModeChecker
			ports.NativeCheckpointVerifier
		}{d, r, f, i, p, v}
	default:
		return d
	}
}
