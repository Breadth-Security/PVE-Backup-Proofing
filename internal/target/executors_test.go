package target

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/model"
	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/pve"
	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/pve/pvefake"
)

// flakyStatus fails AgentExecStatus with err for its first fails calls (all
// of them when fails < 0), then answers like the fake.
type flakyStatus struct {
	*pvefake.Fake
	fails int
	err   error
	calls int
}

func (f *flakyStatus) AgentExecStatus(ctx context.Context, node string, vmid, pid int) (pve.AgentExecStatus, error) {
	f.calls++
	if f.fails < 0 || f.calls <= f.fails {
		return pve.AgentExecStatus{}, f.err
	}
	return f.Fake.AgentExecStatus(ctx, node, vmid, pid)
}

// qgaTimeout is the error PVE returned on pve2 when a Windows guest's agent
// was slow to answer shortly after boot (issue #41).
var qgaTimeout = &pve.APIError{Status: 500, Path: "/nodes/pve1/qemu/990000/agent/exec-status",
	Message: "VM 990000 qga command 'guest-exec-status' failed - got timeout"}

// A guest agent that times out on a status poll is still running the
// command: the poll is retried, and the command's result comes back.
func TestVMExecRetriesAgentTimeout(t *testing.T) {
	fake := healthyFake()
	api := &flakyStatus{Fake: fake, fails: 2, err: qgaTimeout}
	n := newTarget(t, api, fake, nil)
	res, err := n.Executor(model.KindQEMU, 990000).Exec(context.Background(), []string{"hostname"})
	if err != nil || res.ExitCode != 0 {
		t.Fatalf("got %+v, %v; want the command's result after 2 timeouts", res, err)
	}
	if api.calls != 3 {
		t.Errorf("status polls = %d, want 3", api.calls)
	}
}

// Timeouts that never stop end with the command's own time: the context's
// error, which the probe treats as a command timeout, not a transport error.
func TestVMExecAgentTimeoutUntilDeadline(t *testing.T) {
	fake := healthyFake()
	api := &flakyStatus{Fake: fake, fails: -1, err: qgaTimeout}
	n := newTarget(t, api, fake, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := n.Executor(model.KindQEMU, 990000).Exec(ctx, []string{"hostname"})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want the context deadline", err)
	}
}

// Any other status error still ends the command at once.
func TestVMExecOtherStatusErrorFails(t *testing.T) {
	fake := healthyFake()
	other := &pve.APIError{Status: 500, Message: "VM 990000 not running"}
	api := &flakyStatus{Fake: fake, fails: -1, err: other}
	n := newTarget(t, api, fake, nil)
	_, err := n.Executor(model.KindQEMU, 990000).Exec(context.Background(), []string{"hostname"})
	if err == nil || !strings.Contains(err.Error(), "not running") || api.calls != 1 {
		t.Fatalf("err = %v after %d polls, want the error after 1", err, api.calls)
	}
}
