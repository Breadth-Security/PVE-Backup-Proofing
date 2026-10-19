package probe

import (
	"context"

	"git.breadthanalytics.com/tdiana/pbv/internal/model"
)

// traceMissing splits failed units into those explained by a piece missing
// from the backup (covered) and those that are not (untraced). Placeholder:
// every failure is untraced.
func traceMissing(ctx context.Context, ex Executor, failed []string, missing []model.MissingPiece) (covered []model.NotCovered, untraced []string) {
	return nil, failed
}
