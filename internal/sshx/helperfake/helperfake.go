// Package helperfake fakes the `pbp node` helper behind an sshx.Runner, for
// other packages' tests: it decodes each request the daemon's sshx.Helper
// sends and answers it like the helper would.
package helperfake

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sync"

	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/nodeproto"
	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/sshx"
)

// Fake is a mutex-safe sshx.Runner that speaks the helper protocol.
type Fake struct {
	// Handler answers one decoded request; stdin is the request's payload.
	// Nil answers every request with a zero Result (exit 0, no output).
	Handler func(req nodeproto.Request, stdin []byte) (sshx.Result, error)

	mu   sync.Mutex
	reqs []nodeproto.Request
}

var _ sshx.Runner = (*Fake)(nil)

// Helper returns a client that talks to f.
func (f *Fake) Helper() sshx.Helper { return sshx.Helper{R: f} }

// Run decodes cmd as a helper request, records it and delegates to Handler.
// Like sshx.Client it sends nothing once ctx has ended. A command that is
// not a valid helper request (a shell string, say) is an error: the real
// helper would refuse it.
func (f *Fake) Run(ctx context.Context, cmd string, stdin []byte) (sshx.Result, error) {
	if err := ctx.Err(); err != nil {
		return sshx.Result{}, err
	}
	req, err := nodeproto.Parse(cmd)
	if err != nil {
		return sshx.Result{}, fmt.Errorf("helperfake: not a helper request %q: %w", cmd, err)
	}
	f.mu.Lock()
	f.reqs = append(f.reqs, req)
	f.mu.Unlock()
	if f.Handler == nil {
		return sshx.Result{}, nil
	}
	return f.Handler(req, stdin)
}

// Requests returns the decoded requests received so far, in order.
func (f *Fake) Requests() []nodeproto.Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.reqs)
}

// Ops returns the op of each request received so far, in order.
func (f *Fake) Ops() []string {
	var ops []string
	for _, r := range f.Requests() {
		ops = append(ops, r.Op)
	}
	return ops
}

func marker(kind string, req nodeproto.Request, reason string) sshx.Result {
	return sshx.Result{ExitCode: nodeproto.ExitRefused, Stderr: nodeproto.Marker(kind, req.Nonce, reason) + "\n"}
}

// Refuse is the helper's refusal of req with a fixed code (such as
// "out-of-range") and a detail.
func Refuse(req nodeproto.Request, code, detail string) sshx.Result {
	return marker(nodeproto.KindRefused, req, code+": "+detail)
}

// Fail is the helper's internal error for req.
func Fail(req nodeproto.Request, reason string) sshx.Result {
	return marker(nodeproto.KindError, req, reason)
}

// Timeout is the helper's report that req's deadline passed.
func Timeout(req nodeproto.Request) sshx.Result {
	return marker(nodeproto.KindTimeout, req, "")
}

// JSON is a successful reply carrying v as one line of JSON. It panics if v
// cannot be encoded (a test bug).
func JSON(v any) sshx.Result {
	b, err := json.Marshal(v)
	if err != nil {
		panic("helperfake: encode reply: " + err.Error())
	}
	return sshx.Result{Stdout: string(b) + "\n"}
}
