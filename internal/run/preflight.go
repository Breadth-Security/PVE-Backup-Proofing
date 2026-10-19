package run

import (
	"context"
	"fmt"
)

// Preflight checks the target node, installs the notification templates,
// cleans up after any crashed run (design section 6 step 1) and then checks
// that no foreign guest sits in the VMID range. The range check comes last:
// the janitor first removes the untagged leftovers of recorded restores,
// which only Target.Destroy, given the record, accepts as PBP's (design
// section 11.2). Any failure aborts the run.
func Preflight(ctx context.Context, d Deps) error {
	if err := d.Target.Prepare(ctx); err != nil {
		return fmt.Errorf("target: %w", err)
	}
	if err := d.Notifier.EnsureTemplates(ctx); err != nil {
		return fmt.Errorf("notification templates: %w", err)
	}
	if err := Janitor(ctx, d); err != nil {
		return fmt.Errorf("janitor: %w", err)
	}
	if err := d.Target.CheckRangeClear(ctx); err != nil {
		return fmt.Errorf("target: %w", err)
	}
	return nil
}
