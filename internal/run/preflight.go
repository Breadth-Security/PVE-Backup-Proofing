package run

import (
	"context"
	"fmt"

	"git.breadthanalytics.com/tdiana/pbv/internal/state"
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

// recordedGuests is the set of VMIDs where, by the state store, a restore PBP
// started may have left a guest: such a guest is PBP's even if untagged.
func recordedGuests(st state.State) map[int]bool {
	out := map[int]bool{}
	for v, j := range st.InFlight {
		if j.MayHaveGuest() {
			out[v] = true
		}
	}
	return out
}
