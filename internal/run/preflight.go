package run

import (
	"context"
	"fmt"
)

// Preflight checks the target node, installs the notification templates and
// cleans up after any crashed run (design section 6 step 1). Any failure
// aborts the run.
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
	return nil
}
