package target

import (
	"context"
	"fmt"
	"strings"

	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/model"
	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/probe"
	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/sshx"
)

// Executor returns an executor for the guest. For a VMID outside the range
// it returns one whose every Exec fails with ErrOutOfRange, so PBP can never
// run commands inside a production guest.
func (n *ExistingNode) Executor(kind model.GuestKind, vmid int) probe.Executor {
	if err := n.guard(vmid); err != nil {
		return refused{err}
	}
	if kind == model.KindLXC {
		return lxcExec{n: n, vmid: vmid}
	}
	return vmExec{n: n, vmid: vmid}
}

type refused struct{ err error }

func (r refused) Exec(context.Context, []string) (probe.ExecResult, error) {
	return probe.ExecResult{}, r.err
}

// lxcExec runs commands with "pct exec" over SSH.
type lxcExec struct {
	n    *ExistingNode
	vmid int
}

func (e lxcExec) Exec(ctx context.Context, argv []string) (probe.ExecResult, error) {
	// ctx's deadline bounds the command on the node too (sshx.Runner).
	cmd := fmt.Sprintf("pct exec %d -- %s", e.vmid, sshx.Quote(argv...))
	res, err := e.n.ssh.Run(ctx, cmd, nil)
	if err != nil {
		return probe.ExecResult{}, fmt.Errorf("pct exec %d: %w", e.vmid, err)
	}
	return probe.ExecResult{Stdout: res.Stdout, Stderr: res.Stderr, ExitCode: res.ExitCode}, nil
}

// vmExec runs commands through the QEMU guest agent.
type vmExec struct {
	n    *ExistingNode
	vmid int
}

func (e vmExec) Exec(ctx context.Context, argv []string) (probe.ExecResult, error) {
	n := e.n
	pid, err := n.api.AgentExec(ctx, n.node, e.vmid, argv)
	if err != nil {
		return probe.ExecResult{}, agentErr(err)
	}
	for {
		st, err := n.api.AgentExecStatus(ctx, n.node, e.vmid, pid)
		if err != nil {
			return probe.ExecResult{}, agentErr(err)
		}
		if st.Exited {
			return probe.ExecResult{Stdout: st.OutData, Stderr: st.ErrData, ExitCode: st.ExitCode}, nil
		}
		if !sleep(ctx, n.execPoll) {
			return probe.ExecResult{}, ctx.Err()
		}
	}
}

// agentErr marks agent policy refusals as probe.ErrExecBlocked.
func agentErr(err error) error {
	msg := strings.ToLower(err.Error())
	for _, m := range []string{"disabled", "not allowed", "blocked"} {
		if strings.Contains(msg, m) {
			return fmt.Errorf("%w: %v", probe.ErrExecBlocked, err)
		}
	}
	return fmt.Errorf("agent exec: %w", err)
}
