package sshx

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/nodeproto"
)

// Errors returned by Helper. Each is wrapped with the helper's reason.
var (
	// ErrRefused: the helper refused the request. The error is a
	// *RefusedError carrying the fixed refusal code.
	ErrRefused = errors.New("pbp node helper refused the request")
	// ErrHelper: the helper failed internally, or replied unusably.
	ErrHelper = errors.New("pbp node helper error")
	// ErrTimeout: the helper reported that the request's deadline passed. It
	// also wraps context.DeadlineExceeded, so callers classify it like a
	// local command timeout.
	ErrTimeout = errors.New("pbp node helper timed out")
)

// RefusedError is a helper refusal. Code is the helper's fixed category
// (such as "out-of-range", "untagged" or "not-container"); Reason is the
// detail after it. It matches ErrRefused with errors.Is.
type RefusedError struct {
	Code   string
	Reason string
}

func (e *RefusedError) Error() string {
	if e.Code == "" {
		return ErrRefused.Error() + ": " + e.Reason
	}
	return ErrRefused.Error() + " (" + e.Code + "): " + e.Reason
}

func (e *RefusedError) Is(target error) bool { return target == ErrRefused }

// noDeadline is the request deadline used when ctx has none.
const noDeadline = 24 * time.Hour

// Helper is the daemon's typed client for the `pbp node` helper on the node.
type Helper struct {
	R Runner
}

// call sends one request. A helper-reported failure becomes a typed error;
// any other finished command is returned as a Result (for exec, that is the
// guest's own outcome).
func (h Helper) call(ctx context.Context, req nodeproto.Request, stdin []byte) (Result, error) {
	req.Nonce = nodeproto.NewNonce()
	if dl, ok := ctx.Deadline(); ok {
		req.Deadline = dl
	} else {
		req.Deadline = time.Now().Add(noDeadline)
	}
	cmd, err := nodeproto.Encode(req)
	if err != nil {
		return Result{}, fmt.Errorf("%w: encode %s request: %v", ErrHelper, req.Op, err)
	}
	res, err := h.R.Run(ctx, cmd, stdin)
	if err != nil {
		// Runner.Run returns promptly when ctx ends; make sure the error
		// says so even if the Runner wrapped it differently.
		if ctxErr := ctx.Err(); ctxErr != nil && !errors.Is(err, ctxErr) {
			return res, fmt.Errorf("%w: %w", ctxErr, err)
		}
		return res, err
	}
	kind, reason, ok := nodeproto.HelperFailure(res.ExitCode, res.Stderr, req.Nonce)
	if !ok {
		return res, nil
	}
	switch kind {
	case nodeproto.KindRefused:
		code, detail := splitCode(reason)
		return res, &RefusedError{Code: code, Reason: detail}
	case nodeproto.KindTimeout:
		return res, fmt.Errorf("%w: %w", ErrTimeout, context.DeadlineExceeded)
	default:
		return res, fmt.Errorf("%w: %s", ErrHelper, reason)
	}
}

// splitCode splits "<code>: <detail>". Without a lowercase-kebab code prefix
// the whole text is the detail and the code is empty.
func splitCode(reason string) (code, detail string) {
	c, d, found := strings.Cut(reason, ": ")
	if !found || c == "" {
		return "", reason
	}
	for _, r := range c {
		if !(r >= 'a' && r <= 'z' || r == '-') {
			return "", reason
		}
	}
	return c, d
}

// query runs a non-exec op and returns its stdout; any non-zero exit is a
// helper error.
func (h Helper) query(ctx context.Context, op string, stdin []byte) (string, error) {
	res, err := h.call(ctx, nodeproto.Request{Op: op}, stdin)
	if err != nil {
		return "", err
	}
	if res.ExitCode != 0 {
		return "", fmt.Errorf("%w: %s exited %d: %s", ErrHelper, op, res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	return res.Stdout, nil
}

func (h Helper) queryJSON(ctx context.Context, op string, v any) error {
	out, err := h.query(ctx, op, nil)
	if err != nil {
		return err
	}
	if err := json.Unmarshal([]byte(out), v); err != nil {
		return fmt.Errorf("%w: decode %s reply: %v", ErrHelper, op, err)
	}
	return nil
}

// Exec runs argv inside container vmid. A non-zero ExitCode is the guest
// command's own; helper failures are errors (ErrRefused, ErrHelper,
// ErrTimeout). When ctx ends first, Exec returns an error wrapping ctx.Err()
// without waiting for the helper's own timeout marker.
func (h Helper) Exec(ctx context.Context, vmid int, argv []string) (Result, error) {
	return h.call(ctx, nodeproto.Request{Op: "exec", VMID: vmid, Argv: argv}, nil)
}

// Bridge returns the state of the configured bridge.
func (h Helper) Bridge(ctx context.Context) (nodeproto.BridgeState, error) {
	var st nodeproto.BridgeState
	err := h.queryJSON(ctx, "bridge", &st)
	return st, err
}

// Notify sends payload (the helper's notify JSON) to the node's notifier.
func (h Helper) Notify(ctx context.Context, payload []byte) error {
	if payload == nil {
		payload = []byte{}
	}
	_, err := h.query(ctx, "notify", payload)
	return err
}

// Config returns the helper's node.yaml settings and version.
func (h Helper) Config(ctx context.Context) (nodeproto.NodeConfig, error) {
	var c nodeproto.NodeConfig
	err := h.queryJSON(ctx, "config", &c)
	return c, err
}

// Templates returns the installed template hashes by name.
func (h Helper) Templates(ctx context.Context) (map[string]string, error) {
	var m map[string]string
	err := h.queryJSON(ctx, "templates", &m)
	return m, err
}

// Version returns the helper's version string.
func (h Helper) Version(ctx context.Context) (string, error) {
	out, err := h.query(ctx, "version", nil)
	return strings.TrimSpace(out), err
}
