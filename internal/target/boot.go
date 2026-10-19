package target

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"git.breadthanalytics.com/tdiana/pbv/internal/model"
	"git.breadthanalytics.com/tdiana/pbv/internal/probe"
)

// postPingGrace bounds the OS-info and test-exec calls made after the agent
// answered, so an agent that answers at the last moment still gets probed.
const postPingGrace = 30 * time.Second

// WaitBooted implements design section 7.2. A VM without a usable agent is
// not an error: it returns AgentMissing (verdict BOOTED). A guest that never
// reaches running, or a container that never accepts exec, is an error.
func (n *ExistingNode) WaitBooted(ctx context.Context, kind model.GuestKind, vmid int, timeout time.Duration) (BootInfo, error) {
	if err := n.guard(vmid); err != nil {
		return BootInfo{}, err
	}
	start := time.Now()
	tctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	if err := n.waitRunning(tctx, kind, vmid, timeout); err != nil {
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

func (n *ExistingNode) waitRunning(ctx context.Context, kind model.GuestKind, vmid int, timeout time.Duration) error {
	var last string
	var lastErr error
	for {
		last, lastErr = n.api.GuestStatus(ctx, n.node, kind, vmid)
		if lastErr == nil && last == "running" {
			return nil
		}
		if !sleep(ctx, n.statusPoll) {
			break
		}
	}
	if lastErr != nil {
		return fmt.Errorf("guest %d not running after %s: %w", vmid, timeout, lastErr)
	}
	return fmt.Errorf("guest %d not running after %s (status %q)", vmid, timeout, last)
}

func (n *ExistingNode) waitLXC(ctx, tctx context.Context, vmid int, timeout time.Duration) (BootInfo, error) {
	exec := n.Executor(model.KindLXC, vmid)
	var last string
	for {
		res, err := exec.Exec(tctx, []string{"true"})
		if err == nil && res.ExitCode == 0 {
			break
		}
		if err != nil {
			last = err.Error()
		} else {
			last = fmt.Sprintf("exit %d: %s", res.ExitCode, strings.TrimSpace(res.Stderr))
		}
		if !sleep(tctx, n.lxcExecPoll) {
			if ctx.Err() != nil {
				return BootInfo{}, ctx.Err()
			}
			return BootInfo{}, fmt.Errorf("container %d: exec not ready after %s (last: %s)", vmid, timeout, last)
		}
	}
	cfg, err := n.api.GuestConfig(ctx, n.node, model.KindLXC, vmid)
	if err != nil {
		return BootInfo{}, fmt.Errorf("read config %d: %w", vmid, err)
	}
	if cfg["ostype"] == "unmanaged" {
		return BootInfo{OS: probe.Unknown}, nil
	}
	return BootInfo{OS: probe.Linux}, nil
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
	osi, err := n.api.AgentOSInfo(pctx, n.node, vmid)
	if err != nil {
		return BootInfo{}, fmt.Errorf("agent osinfo %d: %w", vmid, err)
	}
	info := BootInfo{OS: probe.Linux}
	argv := []string{"true"}
	if osi.ID == "mswindows" {
		info.OS = probe.Windows
		argv = []string{"cmd", "/c", "exit", "0"}
	}
	if _, err := n.Executor(model.KindQEMU, vmid).Exec(pctx, argv); err != nil {
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
