package run

import (
	"context"
	"fmt"

	"git.breadthanalytics.com/tdiana/pbv/internal/state"
	"git.breadthanalytics.com/tdiana/pbv/internal/target"
)

// Preflight checks the target node, installs the notification templates and
// cleans up after any crashed run (design section 6 step 1). Any failure
// aborts the run.
func Preflight(ctx context.Context, d Deps) error {
	if err := d.Target.Prepare(ctx, recordedGuests(d.Store.Snapshot())); err != nil {
		return fmt.Errorf("target: %w", err)
	}
	if err := d.Notifier.EnsureTemplates(ctx); err != nil {
		return fmt.Errorf("notification templates: %w", err)
	}
	if err := Janitor(ctx, d); err != nil {
		return fmt.Errorf("janitor: %w", err)
	}
	return nil
}

// recordedGuests is the restores, by VMID, that the state store records as
// started and that may have left a guest. The target accepts an untagged
// guest there only as that restore's leftover (stopped, matching backup).
func recordedGuests(st state.State) map[int]target.Recorded {
	out := map[int]target.Recorded{}
	for v, j := range st.InFlight {
		if j.MayHaveGuest() {
			out[v] = target.Recorded{Kind: j.Kind, VolID: j.VolID}
		}
	}
	return out
}
