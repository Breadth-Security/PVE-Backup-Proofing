// Package report sends the per-run summary through the PVE node's
// notification system (design section 8.3).
package report

import (
	"context"

	"git.breadthanalytics.com/tdiana/PVE-Backup-Proofing/internal/model"
)

// Notifier checks PBP's notification templates on the node and sends run
// summaries.
type Notifier interface {
	// CheckTemplates fails if the node's templates are missing or differ
	// from the embedded ones. It changes nothing on the node.
	CheckTemplates(ctx context.Context) error
	Send(ctx context.Context, s model.RunSummary) error
}
