// Package report sends the per-run summary through the PVE node's
// notification system (design section 8.3).
package report

import (
	"context"

	"git.breadthanalytics.com/tdiana/pbv/internal/model"
)

// Notifier installs PBP's notification templates and sends run summaries.
type Notifier interface {
	EnsureTemplates(ctx context.Context) error
	Send(ctx context.Context, s model.RunSummary) error
}
