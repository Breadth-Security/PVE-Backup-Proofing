// Package model holds the types shared across PBP packages.
package model

import (
	"cmp"
	"fmt"
	"time"
)

type GuestKind string

const (
	KindQEMU GuestKind = "qemu"
	KindLXC  GuestKind = "lxc"
)

type GuestKey struct {
	Kind GuestKind
	VMID int
}

func (k GuestKey) String() string { return fmt.Sprintf("%s/%d", k.Kind, k.VMID) }

type Backup struct {
	Storage, VolID, Notes string
	Key                   GuestKey
	Time                  time.Time
	Size                  int64
	Protected, Encrypted  bool
}

type Verdict string

const (
	Proven  Verdict = "PROVEN"
	Failed  Verdict = "FAILED"
	Booted  Verdict = "BOOTED"
	Skipped Verdict = "SKIPPED"
	Error   Verdict = "ERROR"
)

// MissingPiece describes something a backup does not contain.
// Kind is one of: excluded-disk, excluded-mountpoint, bind-mount,
// passthrough, network-storage.
type MissingPiece struct{ Kind, Key, Path, Detail string }

type CheckResult struct {
	Name   string
	OK     bool
	Detail string
}

type NotCovered struct {
	Unit  string
	Piece MissingPiece
}

// LogLine renders the human-readable "not covered" log message.
func (n NotCovered) LogLine() string {
	p := n.Piece
	name := cmp.Or(p.Path, p.Key, p.Kind)
	what := p.Kind // unknown kinds are shown as they are
	switch p.Kind {
	case "excluded-disk":
		what = "disk"
	case "excluded-mountpoint":
		what = "mount point"
	case "bind-mount":
		what = "bind mount"
	case "passthrough":
		what = "device"
	case "network-storage":
		what = "network storage"
		p.Key = "" // not part of the description
	}
	if p.Key != "" {
		what += " " + p.Key
	}
	if p.Detail != "" {
		what += ", " + p.Detail
	}
	return fmt.Sprintf("Found %s depends on %s (%s): not backed up, skipped", n.Unit, name, what)
}

type GuestResult struct {
	Backup                    Backup
	Name                      string
	TempVMID                  int
	Verdict                   Verdict
	Reason                    string
	FailedUnits, IgnoredUnits []string
	NotCovered                []NotCovered
	Checks                    []CheckResult
	AgentMissing              bool
	// AlreadyProven: SKIPPED because the snapshot already carries a PROVEN
	// verdict. Such a skip is not a problem and does not raise severity.
	AlreadyProven                 bool
	RestoreDuration, BootDuration time.Duration
	BootLog                       string
}

type RunSummary struct {
	ID, Trigger    string
	Started, Ended time.Time
	Results        []GuestResult
	RunError       string
	// EnvironmentErrors are problems with the node PBP found but did not
	// act on (e.g. a pbp-tagged guest outside the VMID range).
	EnvironmentErrors []string
}

func (s RunSummary) Counts() map[Verdict]int {
	m := make(map[Verdict]int)
	for _, r := range s.Results {
		m[r.Verdict]++
	}
	return m
}

// Severity is "error" if the run errored or any guest FAILED, "warning" if
// any guest was BOOTED/ERROR or SKIPPED for a reason other than already
// proven or there are environment errors, else "info".
func (s RunSummary) Severity() string {
	if s.RunError != "" {
		return "error"
	}
	warn := false
	for _, r := range s.Results {
		switch r.Verdict {
		case Failed:
			return "error"
		case Booted, Error:
			warn = true
		case Skipped:
			warn = warn || !r.AlreadyProven
		}
	}
	if warn || len(s.EnvironmentErrors) > 0 {
		return "warning"
	}
	return "info"
}
