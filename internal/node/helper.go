package node

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strings"
	"sync/atomic"
	"unicode/utf8"

	"git.breadthanalytics.com/tdiana/PVE-Backup-Proofing/internal/nodeproto"
	"git.breadthanalytics.com/tdiana/PVE-Backup-Proofing/internal/report"
	"git.breadthanalytics.com/tdiana/PVE-Backup-Proofing/internal/sanitize"
)

// ownerTag marks the containers PBP created (same tag the daemon checks).
const ownerTag = "pbp"

// maxNotifyPayload bounds the notify payload read from stdin.
const maxNotifyPayload = 1 << 20

var severities = map[string]bool{"info": true, "notice": true, "warning": true, "error": true}

// reNonce finds a nonce in a request Parse rejected, so the refusal can
// still carry it (see refuseUnparsed).
var reNonce = regexp.MustCompile(`(?:^| )--nonce ([0-9a-f]{16})(?: |$)`)

// failure is a helper-side failure: kind is a nodeproto Kind*.
// code is a fixed, payload-free category for refusals, safe for audit logs;
// reason is the detailed text for the client and may echo request content.
type failure struct {
	kind, code, reason string
}

func refused(code, format string, a ...any) *failure {
	return &failure{nodeproto.KindRefused, code, fmt.Sprintf(format, a...)}
}

func internalErr(format string, a ...any) *failure {
	return &failure{kind: nodeproto.KindError, reason: fmt.Sprintf(format, a...)}
}

var timedOut = &failure{kind: nodeproto.KindTimeout}

// lineTracker remembers whether the last byte written ended a line, so the
// marker always lands on its own final line even after guest output that
// did not end in a newline.
type lineTracker struct {
	w       io.Writer
	midLine bool
}

func (t *lineTracker) Write(p []byte) (int, error) {
	n, err := t.w.Write(p)
	if n > 0 {
		t.midLine = p[n-1] != '\n'
	}
	return n, err
}

// singleLine makes reason safe to put in a one-line marker.
func singleLine(reason string) string {
	r := strings.NewReplacer("\r\n", " | ", "\n", " | ", "\r", " | ")
	return strings.TrimSpace(r.Replace(reason))
}

// Serve handles one request (the SSH_ORIGINAL_COMMAND) and returns the
// process exit code. Helper refusals, errors and timeouts exit
// nodeproto.ExitRefused with a nodeproto.Marker as the final stderr line;
// only Serve writes that marker. An exec request otherwise returns the guest
// command's own exit code, and its output is passed through untouched.
//
// A request Parse rejects has no trustworthy nonce. If one is still present
// syntactically (` --nonce <16 hex>`) the refusal carries it; otherwise the
// final line is the plain `pbp-node: refused: <reason>`, which the daemon
// treats as a transport error. The real client never sends such requests.
func Serve(ctx context.Context, cfg Config, sys System, version string, request string, stdin io.Reader, stdout, stderr io.Writer) int {
	exit, _ := ServeCoded(ctx, cfg, sys, version, request, stdin, stdout, stderr)
	return exit
}

// ServeCoded is Serve that also returns the fixed refusal category (such as
// "out-of-range" or "bad-payload") when the helper refused the request, and ""
// otherwise. The category never contains request or subprocess text, so it is
// what callers should audit instead of the marker reason.
func ServeCoded(ctx context.Context, cfg Config, sys System, version string, request string, stdin io.Reader, stdout, stderr io.Writer) (int, string) {
	errw := &lineTracker{w: stderr}
	req, err := nodeproto.Parse(request)
	if err != nil {
		nonce := ""
		if m := reNonce.FindStringSubmatch(request); m != nil {
			nonce = m[1]
		}
		f0 := refused("bad-request", "invalid request: %v", err)
		writeMarker(errw, nonce, f0)
		return nodeproto.ExitRefused, f0.code
	}

	// The client sends the time it still allows, so the deadline is set on
	// the node's own clock (daemon/node clock skew cannot stretch it), and
	// request_timeout caps it.
	ctx, cancel := context.WithTimeout(ctx, min(req.Timeout, cfg.RequestTimeout))
	defer cancel()
	if ctx.Err() != nil {
		writeMarker(errw, req.Nonce, timedOut)
		return nodeproto.ExitRefused, ""
	}

	exit, f := dispatch(ctx, cfg, sys, version, req, stdin, stdout, errw)
	if f == nil {
		return exit, ""
	}
	if ctx.Err() != nil {
		f = timedOut
	}
	writeMarker(errw, req.Nonce, f)
	return nodeproto.ExitRefused, f.code
}

// maxReason bounds the reason in a marker line; notify passes the notifier's
// stderr through, which is otherwise unbounded.
const maxReason = 2048

// truncateUTF8 cuts s to at most n bytes, on a rune boundary.
func truncateUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// MarkerLine renders the helper's final stderr line (without newline) for a
// failure of the given kind. With an empty nonce it is the plain
// `pbp-node: <kind>: <reason>` form.
func MarkerLine(kind, nonce, reason string) string {
	reason = truncateUTF8(singleLine(reason), maxReason)
	if reason == "" {
		reason = "unspecified"
	}
	if nonce == "" {
		return "pbp-node: " + kind + ": " + reason
	}
	return nodeproto.Marker(kind, nonce, reason)
}

func writeMarker(w *lineTracker, nonce string, f *failure) {
	reason := f.reason
	if f.kind == nodeproto.KindRefused && f.code != "" {
		// The fixed code leads the reason so the client can read it without
		// parsing prose: `refused: <code>: <detail>`.
		reason = f.code + ": " + reason
	}
	line := MarkerLine(f.kind, nonce, reason)
	if w.midLine {
		line = "\n" + line
	}
	_, _ = io.WriteString(w, line+"\n")
}

func dispatch(ctx context.Context, cfg Config, sys System, version string, req nodeproto.Request, stdin io.Reader, stdout io.Writer, stderr io.Writer) (int, *failure) {
	switch req.Op {
	case "exec":
		return execGuest(ctx, cfg, sys, req, stdin, stdout, stderr)
	case "bridge":
		st, err := sys.BridgeState(ctx, cfg.Bridge)
		if err != nil {
			return 0, internalErr("%v", err)
		}
		return 0, writeJSON(stdout, st)
	case "notify":
		return 0, notify(ctx, sys, stdin)
	case "config":
		return 0, writeJSON(stdout, nodeproto.NodeConfig{
			VMIDRange: cfg.VMIDRange, Bridge: cfg.Bridge,
			RequestTimeout: cfg.RequestTimeout, Version: version,
		})
	case "templates":
		h, err := sys.TemplateHashes()
		if err != nil {
			return 0, internalErr("%v", err)
		}
		return 0, writeJSON(stdout, h)
	case "version":
		if _, err := io.WriteString(stdout, version+"\n"); err != nil {
			return 0, internalErr("write: %v", err)
		}
		return 0, nil
	}
	// Parse accepts only the ops above.
	return 0, refused("unsupported-op", "unknown op %q", req.Op)
}

func writeJSON(w io.Writer, v any) *failure {
	b, err := json.Marshal(v)
	if err != nil {
		return internalErr("encode reply: %v", err)
	}
	if _, err := w.Write(append(b, '\n')); err != nil {
		return internalErr("write: %v", err)
	}
	return nil
}

// execGuest runs argv in the container only if it is in range, is an LXC
// and carries the pbp tag; pct exec is never reached otherwise.
//
// There is an inherent check-then-use window between reading the container
// config and pct exec: in it, the container could be removed, untagged, or
// replaced by a VM. Doing any of that needs root (or PVE API rights) on the
// node, and the range check, which does not depend on the config, still
// holds, so the window cannot be used to reach a guest outside the range.
func execGuest(ctx context.Context, cfg Config, sys System, req nodeproto.Request, stdin io.Reader, stdout, stderr io.Writer) (int, *failure) {
	lo, hi := cfg.VMIDRange[0], cfg.VMIDRange[1]
	if req.VMID < lo || req.VMID > hi {
		return 0, refused("out-of-range", "vmid %d is outside the reserved range [%d, %d]", req.VMID, lo, hi)
	}
	conf, err := sys.CTConfig(ctx, req.VMID)
	if err != nil {
		if ctx.Err() != nil {
			return 0, timedOut
		}
		return 0, refused("not-container", "guest %d is not a container on this node: %v", req.VMID, err)
	}
	if !slices.Contains(sanitize.SplitList(conf["tags"]), ownerTag) {
		return 0, refused("untagged", "container %d does not carry the %s tag", req.VMID, ownerTag)
	}
	ectx, cancel := context.WithCancel(ctx)
	defer cancel()
	out := &capWriter{w: stdout, room: maxExecOutput, full: cancel}
	errOut := &capWriter{w: stderr, room: maxExecOutput, full: cancel}
	exit, err := sys.PctExec(ectx, req.VMID, req.Argv, stdin, out, errOut)
	if out.over.Load() || errOut.over.Load() {
		return 0, internalErr("guest output exceeds %d MiB on stdout or stderr; command killed", maxExecOutput>>20)
	}
	if err != nil {
		return 0, internalErr("pct exec %d: %v", req.VMID, err)
	}
	return exit, nil
}

// maxExecOutput bounds what the helper passes on from a guest command, per
// stream: a hostile or broken guest must not flood the daemon (code review
// I14). Probe commands print a few KiB.
const maxExecOutput = 8 << 20

// capWriter passes writes on until room runs out; then it calls full (which
// kills the command) and drops the rest. Writing to the wrapped writer
// through a non-*os.File also gives the command pipes, so waitDelay applies.
type capWriter struct {
	w    io.Writer
	room int
	full func()
	over atomic.Bool
}

func (c *capWriter) Write(p []byte) (int, error) {
	if c.over.Load() {
		return len(p), nil
	}
	if len(p) > c.room {
		if c.room > 0 {
			_, _ = c.w.Write(p[:c.room])
		}
		c.room = 0
		c.over.Store(true)
		c.full()
		return len(p), nil
	}
	n, err := c.w.Write(p)
	c.room -= n
	if err != nil {
		// The client went away: stop the command rather than leave it
		// blocked on a full pipe until the deadline.
		c.full()
	}
	return n, err
}

// notify validates the {severity, data, fields} payload, forces
// fields.type to pbp and hands it to PVE::Notify.
func notify(ctx context.Context, sys System, stdin io.Reader) *failure {
	// The read must not outlive the deadline; on expiry the reader
	// goroutine is left behind and reclaimed when the process exits.
	type readResult struct {
		b   []byte
		err error
	}
	ch := make(chan readResult, 1)
	go func() {
		b, err := io.ReadAll(io.LimitReader(stdin, maxNotifyPayload+1))
		ch <- readResult{b, err}
	}()
	var raw []byte
	select {
	case <-ctx.Done():
		return timedOut
	case r := <-ch:
		if r.err != nil {
			return internalErr("read payload: %v", r.err)
		}
		raw = r.b
	}
	if len(raw) > maxNotifyPayload {
		return refused("payload-too-large", "notify payload larger than %d bytes", maxNotifyPayload)
	}
	var p struct {
		Severity string                     `json:"severity"`
		Data     map[string]json.RawMessage `json:"data"`
		Fields   map[string]string          `json:"fields"`
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		return refused("bad-payload", "notify payload: %v", err)
	}
	if dec.More() {
		return refused("bad-payload", "notify payload: trailing data")
	}
	if _, err := dec.Token(); err != io.EOF {
		return refused("bad-payload", "notify payload: trailing data")
	}
	if !severities[p.Severity] {
		return refused("bad-severity", "notify severity %q is not one of info, notice, warning, error", p.Severity)
	}
	// Only what the daemon sends: a stolen key must not shape fields a
	// notification matcher keys on, or data the templates never show
	// (code review 2.4).
	for k, v := range p.Fields {
		switch {
		case k == "type":
		case k == "hostname" && len(v) <= 253 && strings.IndexFunc(v, func(r rune) bool { return r <= ' ' || r > '~' }) < 0:
		default:
			return refused("bad-payload", "notify field %q is not one PBP sends", k)
		}
	}
	for k := range p.Data {
		if !slices.Contains(report.DataKeys, k) {
			return refused("bad-payload", "notify data key %q is not one PBP sends", k)
		}
	}
	if p.Data == nil {
		p.Data = map[string]json.RawMessage{}
	}
	if p.Fields == nil {
		p.Fields = map[string]string{}
	}
	p.Fields["type"] = "pbp"
	payload, err := json.Marshal(p)
	if err != nil {
		return internalErr("encode payload: %v", err)
	}
	if err := sys.Notify(ctx, payload); err != nil {
		return internalErr("%v", err)
	}
	return nil
}
