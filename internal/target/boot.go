package target

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/model"
	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/probe"
)

// postPingGrace bounds the OS-info and test-exec calls made after the agent
// answered, so an agent that answers at the last moment still gets probed.
const postPingGrace = 30 * time.Second

// WaitBooted implements design section 7.2. A VM without a usable agent is
// not an error: it returns AgentMissing (verdict BOOTED). A guest that never
// reaches running, or a container that never accepts exec, is ErrBootFailed;
// API and SSH faults are other errors.
func (n *ExistingNode) WaitBooted(ctx context.Context, kind model.GuestKind, vmid int, timeout time.Duration) (BootInfo, error) {
	if err := n.guard(vmid); err != nil {
		return BootInfo{}, err
	}
	start := time.Now()
	tctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	if err := n.waitRunning(ctx, tctx, kind, vmid, timeout); err != nil {
		return BootInfo{}, err
	}
	var info BootInfo
	var err error
	if kind == model.KindLXC {
		info, err = n.waitLXC(ctx, tctx, vmid, timeout)
	} else {
		info, err = n.waitVM(ctx, tctx, vmid)
	}
	info.Duration = time.Since(start)
	return info, err
}

// sleep waits d or until ctx ends; it reports whether ctx is still live.
func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// waitRunning polls under tctx (the boot timeout). If the parent ctx ended,
// that is reported instead of a timeout. A guest that answered but never ran
// is a boot failure; a status API that kept failing is not.
func (n *ExistingNode) waitRunning(ctx, tctx context.Context, kind model.GuestKind, vmid int, timeout time.Duration) error {
	var last string
	var lastErr error
	for {
		st, err := n.api.GuestStatus(tctx, n.node, kind, vmid)
		if err == nil && st == "running" {
			return nil
		}
		if tctx.Err() == nil { // an error caused by the deadline itself is no evidence
			last, lastErr = st, err
		}
		if !sleep(tctx, n.statusPoll) {
			break
		}
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if lastErr != nil {
		return fmt.Errorf("guest %d status unknown after %s: %w", vmid, timeout, lastErr)
	}
	return bootFailed("guest %d not running after %s (status %q)", vmid, timeout, last)
}

// waitLXC waits until "true" runs in the container (the helper's exec). A
// container that keeps answering with a non-zero exit while the helper
// works did not boot; one the helper could not reach, or refused (see
// execErr), is a node fault.
func (n *ExistingNode) waitLXC(ctx, tctx context.Context, vmid int, timeout time.Duration) (BootInfo, error) {
	exec := n.Executor(model.KindLXC, vmid)
	var lastExit string
	var lastErr error
	for {
		res, err := exec.Exec(tctx, []string{"true"})
		if err == nil && res.ExitCode == 0 {
			break
		}
		if errors.Is(err, ErrNotOwned) || errors.Is(err, ErrOutOfRange) {
			// A definitive refusal; polling on cannot change it.
			return BootInfo{}, err
		}
		if tctx.Err() == nil {
			if err != nil {
				lastErr, lastExit = err, ""
			} else {
				lastErr, lastExit = nil, fmt.Sprintf("exit %d: %s", res.ExitCode, strings.TrimSpace(res.Stderr))
			}
		}
		if !sleep(tctx, n.lxcExecPoll) {
			if ctx.Err() != nil {
				return BootInfo{}, ctx.Err()
			}
			if lastErr != nil {
				return BootInfo{}, fmt.Errorf("container %d: exec not ready after %s: %w", vmid, timeout, lastErr)
			}
			return BootInfo{}, bootFailed("container %d: exec not ready after %s (last: %s)", vmid, timeout, lastExit)
		}
	}
	cfg, err := n.api.GuestConfig(ctx, n.node, model.KindLXC, vmid)
	if err != nil {
		return BootInfo{}, fmt.Errorf("read config %d: %w", vmid, err)
	}
	if cfg["ostype"] == "unmanaged" {
		return BootInfo{OS: probe.Unknown, OSType: cfg["ostype"], OSName: cfg["ostype"]}, nil
	}
	return BootInfo{OS: probe.Linux, OSType: cfg["ostype"], OSName: cfg["ostype"]}, nil
}

func (n *ExistingNode) waitVM(ctx, tctx context.Context, vmid int) (BootInfo, error) {
	missing := BootInfo{OS: probe.Unknown, AgentMissing: true}
	cfg, err := n.api.GuestConfig(tctx, n.node, model.KindQEMU, vmid)
	if err != nil {
		return BootInfo{}, fmt.Errorf("read config %d: %w", vmid, err)
	}
	if !agentEnabled(cfg["agent"]) {
		return missing, nil
	}
	for n.api.AgentPing(tctx, n.node, vmid) != nil {
		if !sleep(tctx, n.agentPoll) {
			if ctx.Err() != nil {
				return BootInfo{}, ctx.Err()
			}
			return missing, nil
		}
	}

	pctx, cancel := context.WithTimeout(ctx, postPingGrace)
	defer cancel()
	vm := n.Executor(model.KindQEMU, vmid)
	osi, err := n.api.AgentOSInfo(pctx, n.node, vmid)
	if err != nil {
		// The agent answers but cannot say what it runs: a guest where
		// "true" runs is Linux; otherwise PBP cannot look inside.
		if _, err := vm.Exec(pctx, []string{"true"}); err != nil {
			if ctx.Err() != nil {
				return BootInfo{}, ctx.Err()
			}
			return BootInfo{OS: probe.Unknown, ExecBlocked: true}, nil
		}
		return BootInfo{OS: probe.Linux}, nil
	}
	info := BootInfo{OS: probe.Linux, OSName: osi.Name}
	argv := []string{"true"}
	if osi.ID == "mswindows" {
		info.OS = probe.Windows
		argv = []string{"cmd", "/c", "exit", "0"}
	}
	if _, err := vm.Exec(pctx, argv); err != nil {
		if !errors.Is(err, probe.ErrExecBlocked) {
			return BootInfo{}, fmt.Errorf("agent test exec %d: %w", vmid, err)
		}
		info.ExecBlocked = true
	}
	return info, nil
}

// agentEnabled reads a qemu "agent" option: "1", "1,opts..." or
// "enabled=1,opts...".
func agentEnabled(v string) bool {
	fields := strings.Split(strings.TrimSpace(v), ",")
	if fields[0] == "1" {
		return true
	}
	for _, f := range fields {
		if f == "enabled=1" {
			return true
		}
	}
	return false
}
