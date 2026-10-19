// Package model holds the types shared across PBP packages.
package model

import (
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
	var what string
	switch p.Kind {
	case "excluded-disk":
		what = "disk " + p.Key
	case "excluded-mountpoint":
		what = "mount point " + p.Key
	case "bind-mount":
		what = "bind mount " + p.Key
	case "passthrough":
		what = "device " + p.Key
	case "network-storage":
		what = "network storage"
	default:
		what = p.Kind + " " + p.Key
	}
	if p.Detail != "" {
		what += ", " + p.Detail
	}
	path := p.Path
	if path == "" {
		path = p.Key
	}
	return fmt.Sprintf("Found %s depends on %s (%s): not backed up, skipped", n.Unit, path, what)
}

type GuestResult struct {
	Backup                        Backup
	Name                          string
	TempVMID                      int
	Verdict                       Verdict
	Reason                        string
	FailedUnits, IgnoredUnits     []string
	NotCovered                    []NotCovered
	Checks                        []CheckResult
	AgentMissing                  bool
	RestoreDuration, BootDuration time.Duration
	BootLog                       string
}

type RunSummary struct {
	ID, Trigger    string
	Started, Ended time.Time
	Results        []GuestResult
	RunError       string
}

func (s RunSummary) Counts() map[Verdict]int {
	m := make(map[Verdict]int)
	for _, r := range s.Results {
		m[r.Verdict]++
	}
	return m
}

// Severity is "error" if the run errored or any guest FAILED, "warning" if
// any guest was BOOTED/SKIPPED/ERROR, else "info".
func (s RunSummary) Severity() string {
	if s.RunError != "" {
		return "error"
	}
	c := s.Counts()
	if c[Failed] > 0 {
		return "error"
	}
	if c[Booted] > 0 || c[Skipped] > 0 || c[Error] > 0 {
		return "warning"
	}
	return "info"
}
