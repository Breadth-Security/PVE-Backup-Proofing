package run

import (
	"context"
	"fmt"
)

// Preflight checks the node helper first, so nothing on the node is touched
// if the helper is missing or misconfigured. It then verifies the target node
// (PVE version, bridge, restore storage), checks the notification templates,
// cleans up after any crashed run (design section 6 step 1) and finally checks
// that no foreign guest sits in the VMID range. The range check comes last:
// the janitor first removes the untagged leftovers of recorded restores,
// which only Target.Destroy, given the record, accepts as PBP's (design
// section 11.2). Any failure aborts the run.
func Preflight(ctx context.Context, d Deps) error { return preflight(ctx, d, true) }

// PreflightReadOnly is Preflight without the janitor: it changes nothing on
// the node and needs no state store. pbp setup runs it without the run lock,
// where a janitor could destroy the guest of a run in progress.
func PreflightReadOnly(ctx context.Context, d Deps) error { return preflight(ctx, d, false) }

func preflight(ctx context.Context, d Deps, janitor bool) error {
	if err := d.Target.CheckNode(ctx); err != nil {
		return fmt.Errorf("node helper: %w", err)
	}
	if err := d.Target.Prepare(ctx); err != nil {
		return fmt.Errorf("target: %w", err)
	}
	if err := d.Notifier.CheckTemplates(ctx); err != nil {
		return fmt.Errorf("notification templates: %w", err)
	}
	if janitor {
		if err := Janitor(ctx, d); err != nil {
			return fmt.Errorf("janitor: %w", err)
		}
	}
	if err := d.Target.CheckRangeClear(ctx); err != nil {
		return fmt.Errorf("target: %w", err)
	}
	return nil
}
