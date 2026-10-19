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
	OS      OSFamily
	Checks  config.Checks
	Guest   GuestInfo
	Missing []model.MissingPiece
}

// Outcome is the probe's decision plus the evidence behind it.
type Outcome struct {
	Verdict      model.Verdict
	Reason       string
	FailedUnits  []string // non-ignored failed units
	IgnoredUnits []string
	NotCovered   []model.NotCovered
	Checks       []model.CheckResult
	BootLog      string
}

// Run probes the guest, dispatching on its OS family.
func Run(ctx context.Context, ex Executor, in Input) Outcome {
	switch in.OS {
	case Linux:
		return runLinux(ctx, ex, in)
	case Windows:
		return runWindows(ctx, ex, in)
	default:
		return Outcome{Verdict: model.Booted, Reason: "unknown guest OS"}
	}
}
