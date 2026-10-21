package target

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/nodeproto"
	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/sshx"
)

// setupHint ends every node helper failure.
const setupHint = `see docs/setup.md`

// NodeStep is the outcome of one node helper check, for `pbp check` to print.
type NodeStep struct {
	Name   string
	Detail string // shown after "ok"
	Err    error  // nil: the step passed
}

// NodeStepper is implemented by a Target that can report its node helper
// checks step by step.
type NodeStepper interface {
	CheckNodeSteps(ctx context.Context) []NodeStep
}

var _ NodeStepper = (*ExistingNode)(nil)

// CheckNode runs the node helper checks and returns the first failure.
func (n *ExistingNode) CheckNode(ctx context.Context) error {
	for _, s := range n.CheckNodeSteps(ctx) {
		if s.Err != nil {
			return fmt.Errorf("%s: %w", s.Name, s.Err)
		}
	}
	return nil
}

// CheckNodeSteps runs the checks in order and stops after the first
// failure, which is the last step returned.
//
//  1. The helper answers `version` and is compatible with the daemon.
//  2. Its config matches target.vmid_range and target.bridge.
//  3. Its request_timeout covers restore.probe_timeout (a warning only).
//  4. It refuses an exec just above the range.
func (n *ExistingNode) CheckNodeSteps(ctx context.Context) []NodeStep {
	var steps []NodeStep
	add := func(name, detail string, err error) {
		if err != nil {
			err = fmt.Errorf("%w (%s)", err, setupHint)
		}
		steps = append(steps, NodeStep{Name: name, Detail: detail, Err: err})
	}

	const stepVersion = "node helper version"
	v, err := n.helper.Version(ctx)
	if err != nil {
		add(stepVersion, "", fmt.Errorf("the pbp node helper is not reachable: %w", err))
		return steps
	}
	if !nodeproto.Compatible(n.daemonVersion, v) {
		hint := "daemon and helper must share major.minor"
		if n.daemonVersion == "dev" || v == "dev" {
			hint = "a dev build is compatible only with a dev build"
		}
		add(stepVersion, "", fmt.Errorf("helper %s is not compatible with daemon %s; %s", v, n.daemonVersion, hint))
		return steps
	}
	add(stepVersion, v, nil)

	const stepConfig = "node helper config"
	cfg, err := n.helper.Config(ctx)
	if err != nil {
		add(stepConfig, "", fmt.Errorf("cannot read the helper's config: %w", err))
		return steps
	}
	if cfg.VMIDRange != [2]int{n.lo, n.hi} {
		add(stepConfig, "", fmt.Errorf("vmid_range differs: the helper has [%d, %d], target.vmid_range is [%d, %d]",
			cfg.VMIDRange[0], cfg.VMIDRange[1], n.lo, n.hi))
		return steps
	}
	if cfg.Bridge != n.bridge {
		add(stepConfig, "", fmt.Errorf("bridge differs: the helper has %q, target.bridge is %q", cfg.Bridge, n.bridge))
		return steps
	}
	add(stepConfig, fmt.Sprintf("range [%d, %d], bridge %s", n.lo, n.hi, n.bridge), nil)

	const stepTimeout = "node helper request_timeout"
	if cfg.RequestTimeout < n.probeTimeout {
		slog.Warn("node helper request_timeout is below restore.probe_timeout; a long probe command is cut short on the node",
			"request_timeout", cfg.RequestTimeout.String(), "probe_timeout", n.probeTimeout.String(), "hint", setupHint)
		add(stepTimeout, fmt.Sprintf("warning: %s is below restore.probe_timeout %s; %s", cfg.RequestTimeout, n.probeTimeout, setupHint), nil)
	} else {
		add(stepTimeout, cfg.RequestTimeout.String(), nil)
	}

	const stepRefusal = "node helper refusal"
	// Probe a VMID above the range that no guest uses: if the helper is
	// broken in exactly the way this step exists to catch, the exec must
	// not reach a real container (code review 1.5).
	probe, err := n.unusedAbove(ctx)
	if err != nil {
		add(stepRefusal, "", err)
		return steps
	}
	_, err = n.helper.Exec(ctx, probe, []string{"true"})
	var re *sshx.RefusedError
	switch {
	case errors.As(err, &re) && re.Code == "out-of-range":
		add(stepRefusal, fmt.Sprintf("exec in %d refused", probe), nil)
	case ctx.Err() != nil:
		add(stepRefusal, "", ctx.Err())
	default:
		reason := "it ran"
		if err != nil {
			reason = err.Error()
		}
		add(stepRefusal, "", fmt.Errorf("helper did not refuse an out-of-range exec in %d: %s", probe, reason))
	}
	return steps
}

// maxVMID is the largest VMID PVE accepts.
const maxVMID = 999999999

// unusedAbove returns the first VMID above the range that no guest in the
// cluster uses. Without an API (unit tests of the helper steps alone) it is
// the one just above the range.
func (n *ExistingNode) unusedAbove(ctx context.Context) (int, error) {
	probe := n.hi + 1
	if n.api == nil {
		return probe, nil
	}
	gs, err := n.api.Guests(ctx)
	if err != nil {
		return 0, fmt.Errorf("list guests to pick an unused VMID above the range: %w", err)
	}
	used := make(map[int]bool, len(gs))
	for _, g := range gs {
		used[g.Key.VMID] = true
	}
	for used[probe] {
		if probe == maxVMID {
			return 0, errors.New("no unused VMID above the range to check the helper's refusal with")
		}
		probe++
	}
	return probe, nil
}
