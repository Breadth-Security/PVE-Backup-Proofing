package sshx

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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

// noDeadline is the request timeout used when ctx has no deadline.
const noDeadline = 24 * time.Hour

// remaining is the time ctx still allows, sent as the request's timeout:
// noDeadline when ctx has no deadline, and at least 1ms (the helper then
// reports a timeout) so an expiring ctx still makes a valid request. The
// helper caps it at its own request_timeout.
//
// It is computed before Run (re)connects, so a reconnect (up to dialTimeout)
// is spent before the helper's clock starts and the helper may run that
// much past the caller's deadline; the caller still returns on time, and
// the helper still bounds itself (code review 4.8).
func remaining(ctx context.Context) time.Duration {
	d := noDeadline
	if dl, ok := ctx.Deadline(); ok {
		d = time.Until(dl)
	}
	return min(max(d, time.Millisecond), nodeproto.MaxTimeout)
}

// Helper is the daemon's typed client for the `pbp node` helper on the node.
type Helper struct {
	R Runner
}

// call sends one request. A helper-reported failure becomes a typed error;
// any other finished command is returned as a Result (for exec, that is the
// guest's own outcome).
func (h Helper) call(ctx context.Context, req nodeproto.Request, stdin []byte) (Result, error) {
	req.Nonce = nodeproto.NewNonce()
	req.Timeout = remaining(ctx)
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

// refusalCodes are the fixed codes internal/node refuses with.
var refusalCodes = map[string]bool{
	"bad-request": true, "unsupported-op": true, "out-of-range": true,
	"not-container": true, "untagged": true, "payload-too-large": true,
	"bad-payload": true, "bad-severity": true,
}

// splitCode splits "<code>: <detail>". Unless the prefix is a known code the
// whole text is the detail and the code is empty.
func splitCode(reason string) (code, detail string) {
	c, d, found := strings.Cut(reason, ": ")
	if !found || !refusalCodes[c] {
		return "", reason
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

// decodeStrict decodes a helper reply that must be exactly one JSON object
// holding exactly the keys in want, none of them null (templates, whose
// keys are data, is decoded separately). Anything else is ErrHelper.
func decodeStrict(op, out string, want ...string) (map[string]json.RawMessage, error) {
	bad := func(format string, a ...any) error {
		return fmt.Errorf("%w: %s reply: %s", ErrHelper, op, fmt.Sprintf(format, a...))
	}
	dec := json.NewDecoder(strings.NewReader(out))
	var m map[string]json.RawMessage
	if err := dec.Decode(&m); err != nil {
		return nil, bad("%v", err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, bad("trailing data")
	}
	if m == nil {
		return nil, bad("not an object")
	}
	if len(m) != len(want) {
		return nil, bad("unexpected fields")
	}
	for _, k := range want {
		v, ok := m[k]
		if !ok || string(v) == "null" {
			return nil, bad("missing or null field %q", k)
		}
	}
	return m, nil
}

func (h Helper) rawReply(ctx context.Context, op string) (string, error) {
	return h.query(ctx, op, nil)
}

// Exec runs argv inside container vmid. A non-zero ExitCode is the guest
// command's own; helper failures are errors (ErrRefused, ErrHelper,
// ErrTimeout). When ctx ends first, Exec returns an error wrapping ctx.Err()
// without waiting for the helper's own timeout marker.
func (h Helper) Exec(ctx context.Context, vmid int, argv []string) (Result, error) {
	return h.call(ctx, nodeproto.Request{Op: "exec", VMID: vmid, Argv: argv}, nil)
}

// Bridge returns the state of the configured bridge. The reply must carry
// every field (an isolated bridge sends "members":[]), so a truncated or
// empty reply is an error rather than a zero-valued bridge.
func (h Helper) Bridge(ctx context.Context) (nodeproto.BridgeState, error) {
	var st nodeproto.BridgeState
	out, err := h.rawReply(ctx, "bridge")
	if err != nil {
		return st, err
	}
	if _, err := decodeStrict("bridge", out, "name", "ovs", "disable_ipv6", "addrs", "members"); err != nil {
		return st, err
	}
	if err := json.Unmarshal([]byte(out), &st); err != nil {
		return nodeproto.BridgeState{}, fmt.Errorf("%w: bridge reply: %v", ErrHelper, err)
	}
	if st.Name == "" || st.Members == nil {
		return nodeproto.BridgeState{}, fmt.Errorf("%w: bridge reply: empty name or members", ErrHelper)
	}
	return st, nil
}

// Notify sends payload (the helper's notify JSON) to the node's notifier.
func (h Helper) Notify(ctx context.Context, payload []byte) error {
	if payload == nil {
		payload = []byte{}
	}
	_, err := h.query(ctx, "notify", payload)
	return err
}

// Config returns the helper's node.yaml settings and version, validated.
func (h Helper) Config(ctx context.Context) (nodeproto.NodeConfig, error) {
	out, err := h.rawReply(ctx, "config")
	if err != nil {
		return nodeproto.NodeConfig{}, err
	}
	if _, err := decodeStrict("config", out, "vmid_range", "bridge", "request_timeout", "version"); err != nil {
		return nodeproto.NodeConfig{}, err
	}
	var c nodeproto.NodeConfig
	if err := json.Unmarshal([]byte(out), &c); err != nil {
		return nodeproto.NodeConfig{}, fmt.Errorf("%w: config reply: %v", ErrHelper, err)
	}
	if c.Bridge == "" || c.Version == "" || c.RequestTimeout <= 0 || c.VMIDRange[0] <= 0 || c.VMIDRange[0] > c.VMIDRange[1] {
		return nodeproto.NodeConfig{}, fmt.Errorf("%w: config reply has invalid values: %+v", ErrHelper, c)
	}
	return c, nil
}

// Templates returns the installed template hashes by name.
func (h Helper) Templates(ctx context.Context) (map[string]string, error) {
	out, err := h.rawReply(ctx, "templates")
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(strings.NewReader(out))
	var m map[string]string
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("%w: templates reply: %v", ErrHelper, err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, fmt.Errorf("%w: templates reply: trailing data", ErrHelper)
	}
	if m == nil {
		return nil, fmt.Errorf("%w: templates reply: not an object", ErrHelper)
	}
	return m, nil
}

// Version returns the helper's version string.
func (h Helper) Version(ctx context.Context) (string, error) {
	out, err := h.query(ctx, "version", nil)
	if err != nil {
		return "", err
	}
	v := strings.TrimSpace(out)
	if v == "" {
		return "", fmt.Errorf("%w: version reply is empty", ErrHelper)
	}
	return v, nil
}
