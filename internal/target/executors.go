package target

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/model"
	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/probe"
	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/pve"
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

// lxcExec runs commands through the helper's exec request, which runs
// "pct exec" on the node only for a pbp-tagged container in the range.
type lxcExec struct {
	n    *ExistingNode
	vmid int
}

func (e lxcExec) Exec(ctx context.Context, argv []string) (probe.ExecResult, error) {
	// The time ctx still allows travels in the request; the helper bounds
	// the command on the node by it.
	res, err := e.n.helper.Exec(ctx, e.vmid, argv)
	if err != nil {
		return probe.ExecResult{}, execErr(e.vmid, err)
	}
	return probe.ExecResult{Stdout: res.Stdout, Stderr: res.Stderr, ExitCode: res.ExitCode}, nil
}

// execErr maps a helper refusal by its code: out-of-range is ErrOutOfRange,
// untagged and not-container are ErrNotOwned. Any other error, including
// any other refusal, stays a plain error, which a probe treats as a
// transport fault (ERROR, never FAILED).
func execErr(vmid int, err error) error {
	var re *sshx.RefusedError
	if errors.As(err, &re) {
		switch re.Code {
		case "out-of-range":
			return fmt.Errorf("exec in container %d: %w: %w", vmid, ErrOutOfRange, err)
		case "untagged", "not-container":
			return fmt.Errorf("exec in container %d: %w: %w", vmid, ErrNotOwned, err)
		}
	}
	if errors.Is(err, sshx.ErrNoSession) {
		return fmt.Errorf("exec in container %d: %w: %w", vmid, probe.ErrNoAnswer, err)
	}
	return fmt.Errorf("exec in container %d: %w", vmid, err)
}

// vmExec runs commands through the QEMU guest agent.
type vmExec struct {
	n    *ExistingNode
	vmid int
}

func (e vmExec) Exec(ctx context.Context, argv []string) (probe.ExecResult, error) {
	n := e.n
	pid, err := n.api.AgentExec(ctx, n.node, e.vmid, argv)
	if missingBinary(err) {
		// What `pct exec` reports as exit 127 for a container.
		return probe.ExecResult{ExitCode: 127, Stderr: err.Error()}, nil
	}
	if err != nil {
		return probe.ExecResult{}, agentErr(err)
	}
	for {
		st, err := n.api.AgentExecStatus(ctx, n.node, e.vmid, pid)
		if agentTimeout(err) {
			// The agent was slow to answer this poll (a Windows guest
			// shortly after boot, issue #41); the command still runs. Poll
			// again, within the command's own time.
			if !sleep(ctx, n.execPoll) {
				return probe.ExecResult{}, ctx.Err()
			}
			continue
		}
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

// missingBinary reports whether err is the guest agent failing to start a
// command because it does not exist ("Failed to execute child process …
// (No such file or directory)", qemu-ga via PVE 9.2), which the probe
// treats like exit 127 (code review I13).
func missingBinary(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "Failed to execute child process") && strings.Contains(msg, "No such file or directory")
}

// agentTimeout reports whether err is PVE's qga call timing out ("got
// timeout"): the agent did not answer in time, which says nothing about
// the command it is running.
func agentTimeout(err error) bool {
	var ae *pve.APIError
	return errors.As(err, &ae) && strings.Contains(ae.Message, "got timeout")
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
