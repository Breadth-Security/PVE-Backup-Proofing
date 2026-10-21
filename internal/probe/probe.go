package probe

import (
	"context"
	"time"

	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/config"
	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/model"
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
	// NoNesting: an unprivileged container without features nesting=1. Its
	// systemd cannot sandbox units, so units that fail only for that (as
	// they do in the production guest) make it BOOTED, not FAILED.
	NoNesting bool
	// WindowsSettlePoll is how often a Windows guest's stopped automatic
	// services are re-checked while they may still be starting (0 = 15s).
	WindowsSettlePoll time.Duration
	// SettleTimeout bounds the wait for a Linux guest's boot to settle
	// (`systemctl is-system-running --wait`) when the probe context has no
	// deadline (0 = DefaultSettleTimeout). With a deadline, the wait uses
	// the probe's remaining time instead, minus a reserve for the checks.
	SettleTimeout time.Duration
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

// Run probes the guest, dispatching on its OS family. Every command is
// bounded by Checks.CommandTimeout; one that times out fails its check. The
// whole probe is bounded by ctx: if it ends first the verdict is ERROR
// ("probe timed out"), as is a transport error (the executor could not run
// a command). Neither is ever FAILED.
func Run(ctx context.Context, ex Executor, in Input) Outcome {
	g := newGuard(ex, in.Checks.CommandTimeout)
	switch in.OS {
	case Linux:
		out := runLinux(ctx, g, in)
		// Tracing tolerates Exec errors (conservatively untraced); if any
		// was the probe ending or a broken transport, the FAILED it led to
		// is not the backup's.
		switch {
		case out.Verdict == model.Proven:
		case g.endedErr() != nil:
			out.Verdict, out.Reason = model.Error, endedReason(g.endedErr())
		case g.transportErr() != nil && out.Verdict == model.Failed:
			out.Verdict, out.Reason = model.Error, transportReason(g.transportErr())
		}
		return bound(out)
	case Windows:
		return bound(runWindows(ctx, g, in))
	default:
		return Outcome{Verdict: model.Booted, Reason: "unknown guest OS"}
	}
}
