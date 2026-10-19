package probe

import (
	"context"

	"git.breadthanalytics.com/tdiana/pbv/internal/config"
	"git.breadthanalytics.com/tdiana/pbv/internal/model"
)

// GuestInfo identifies the guest for per-guest check matching.
type GuestInfo struct {
	VMID int
	Name string
	Tags []string
}

// Input is everything Run needs to probe one guest.
type Input struct {
	OS OSFamily
	// OSType is a container's PVE ostype (e.g. "debian", "alpine"); empty
	// for a VM. It selects the init system to probe.
	OSType  string
	Checks  config.Checks
	Guest   GuestInfo
	Missing []model.MissingPiece
}

// Outcome is the probe's decision plus the evidence behind it.
type Outcome struct {
	Verdict      model.Verdict
	Reason       string
	FailedUnits  []string // non-ignored failed units not traced to a missing piece
	IgnoredUnits []string
	NotCovered   []model.NotCovered
	Checks       []model.CheckResult
	BootLog      string
}

// Run probes the guest, dispatching on its OS family. A transport error
// (the executor could not run a command) is ERROR, never FAILED.
func Run(ctx context.Context, ex Executor, in Input) Outcome {
	g := &guard{ex: ex}
	var out Outcome
	switch in.OS {
	case Linux:
		out = runLinux(ctx, g, in)
	case Windows:
		out = runWindows(ctx, g, in)
	default:
		return Outcome{Verdict: model.Booted, Reason: "unknown guest OS"}
	}
	if err := g.transportErr(); err != nil && out.Verdict == model.Failed {
		out.Verdict, out.Reason = model.Error, transportReason(err)
	}
	return out
}
