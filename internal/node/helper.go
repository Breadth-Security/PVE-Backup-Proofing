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
	"time"

	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/nodeproto"
	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/sanitize"
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
type failure struct {
	kind, reason string
}

func refused(format string, a ...any) *failure {
	return &failure{nodeproto.KindRefused, fmt.Sprintf(format, a...)}
}

func internalErr(format string, a ...any) *failure {
	return &failure{nodeproto.KindError, fmt.Sprintf(format, a...)}
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
	errw := &lineTracker{w: stderr}
	req, err := nodeproto.Parse(request)
	if err != nil {
		nonce := ""
		if m := reNonce.FindStringSubmatch(request); m != nil {
			nonce = m[1]
		}
		writeMarker(errw, nonce, refused("invalid request: %v", err))
		return nodeproto.ExitRefused
	}

	deadline := req.Deadline
	if capAt := time.Now().Add(cfg.RequestTimeout); capAt.Before(deadline) {
		deadline = capAt
	}
	if !time.Now().Before(deadline) {
		writeMarker(errw, req.Nonce, timedOut)
		return nodeproto.ExitRefused
	}
	ctx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()

	exit, f := dispatch(ctx, cfg, sys, version, req, stdin, stdout, errw)
	if f == nil {
		return exit
	}
	if ctx.Err() != nil {
		f = timedOut
	}
	writeMarker(errw, req.Nonce, f)
	return nodeproto.ExitRefused
}

func writeMarker(w *lineTracker, nonce string, f *failure) {
	reason := singleLine(f.reason)
	if reason == "" {
		reason = "unspecified"
	}
	line := ""
	if nonce == "" {
		line = "pbp-node: " + f.kind + ": " + reason
	} else {
		line = nodeproto.Marker(f.kind, nonce, reason)
	}
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
	return 0, refused("unknown op %q", req.Op)
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
func execGuest(ctx context.Context, cfg Config, sys System, req nodeproto.Request, stdin io.Reader, stdout, stderr io.Writer) (int, *failure) {
	lo, hi := cfg.VMIDRange[0], cfg.VMIDRange[1]
	if req.VMID < lo || req.VMID > hi {
		return 0, refused("vmid %d is outside the reserved range [%d, %d]", req.VMID, lo, hi)
	}
	conf, err := sys.PctConfig(ctx, req.VMID)
	if err != nil {
		if ctx.Err() != nil {
			return 0, timedOut
		}
		return 0, refused("guest %d is not a container on this node: %v", req.VMID, err)
	}
	if !slices.Contains(sanitize.SplitList(conf["tags"]), ownerTag) {
		return 0, refused("container %d does not carry the %s tag", req.VMID, ownerTag)
	}
	exit, err := sys.PctExec(ctx, req.VMID, req.Argv, stdin, stdout, stderr)
	if err != nil {
		return 0, internalErr("pct exec %d: %v", req.VMID, err)
	}
	return exit, nil
}

// notify validates the {severity, data, fields} payload, forces
// fields.type to pbp and hands it to PVE::Notify.
func notify(ctx context.Context, sys System, stdin io.Reader) *failure {
	raw, err := io.ReadAll(io.LimitReader(stdin, maxNotifyPayload+1))
	if err != nil {
		return internalErr("read payload: %v", err)
	}
	if len(raw) > maxNotifyPayload {
		return refused("notify payload larger than %d bytes", maxNotifyPayload)
	}
	var p struct {
		Severity string                     `json:"severity"`
		Data     map[string]json.RawMessage `json:"data"`
		Fields   map[string]string          `json:"fields"`
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		return refused("notify payload: %v", err)
	}
	if dec.More() {
		return refused("notify payload: trailing data")
	}
	if _, err := dec.Token(); err != io.EOF {
		return refused("notify payload: trailing data")
	}
	if !severities[p.Severity] {
		return refused("notify severity %q is not one of info, notice, warning, error", p.Severity)
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
