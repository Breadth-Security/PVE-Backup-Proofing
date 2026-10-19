package probe

import (
	"context"

	"git.breadthanalytics.com/tdiana/pbv/internal/model"
)

// runWindows is a placeholder until the Windows probe lands.
func runWindows(ctx context.Context, ex Executor, in Input) Outcome {
	return Outcome{Verdict: model.Booted, Reason: "windows probe not implemented"}
}
