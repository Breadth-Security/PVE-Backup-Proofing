// Package probetest provides test doubles for the probe package.
package probetest

import (
	"context"
	"strings"

	"git.breadthanalytics.com/tdiana/PVE-Backup-Proofing/internal/probe"
)

// Scripted is an Executor keyed by strings.Join(argv, " "). Unknown commands
// exit 127.
type Scripted map[string]probe.ExecResult

// Exec implements probe.Executor.
func (s Scripted) Exec(_ context.Context, argv []string) (probe.ExecResult, error) {
	if r, ok := s[strings.Join(argv, " ")]; ok {
		return r, nil
	}
	return probe.ExecResult{ExitCode: 127}, nil
}
