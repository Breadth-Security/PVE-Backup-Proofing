package model

import "testing"

func TestGuestKeyString(t *testing.T) {
	if got := (GuestKey{KindLXC, 101}).String(); got != "lxc/101" {
		t.Fatal(got)
	}
}

func TestNotCoveredLogLine(t *testing.T) {
	n := NotCovered{Unit: "postgresql.service", Piece: MissingPiece{Kind: "excluded-disk", Key: "scsi1", Path: "/srv/pgdata", Detail: "backup=0"}}
	want := "Found postgresql.service depends on /srv/pgdata (disk scsi1, backup=0): not backed up, skipped"
	if got := n.LogLine(); got != want {
		t.Fatalf("got %q", got)
	}
}

func TestNotCoveredLogLineVariants(t *testing.T) {
	cases := []struct {
		p    MissingPiece
		want string
	}{
		{MissingPiece{Kind: "excluded-mountpoint", Key: "mp0", Path: "/data"}, "Found u.service depends on /data (mount point mp0): not backed up, skipped"},
		{MissingPiece{Kind: "bind-mount", Key: "mp1"}, "Found u.service depends on mp1 (bind mount mp1): not backed up, skipped"},
		{MissingPiece{Kind: "passthrough", Key: "hostpci0", Path: "/dev/x", Detail: "gpu"}, "Found u.service depends on /dev/x (device hostpci0, gpu): not backed up, skipped"},
		{MissingPiece{Kind: "network-storage", Path: "/mnt/nfs"}, "Found u.service depends on /mnt/nfs (network storage): not backed up, skipped"},
		{MissingPiece{Kind: "future-kind", Key: "x1", Path: "/p", Detail: "d"}, "Found u.service depends on /p (future-kind x1, d): not backed up, skipped"},
		{MissingPiece{Kind: "network-storage"}, "Found u.service depends on network-storage (network storage): not backed up, skipped"},
		{MissingPiece{Kind: "future-kind"}, "Found u.service depends on future-kind (future-kind): not backed up, skipped"},
		{MissingPiece{Kind: "bind-mount"}, "Found u.service depends on bind-mount (bind mount): not backed up, skipped"},
	}
	for _, c := range cases {
		if got := (NotCovered{Unit: "u.service", Piece: c.p}).LogLine(); got != c.want {
			t.Errorf("got %q want %q", got, c.want)
		}
	}
}

func TestSeverity(t *testing.T) {
	cases := []struct {
		vs     []Verdict
		runErr string
		want   string
	}{
		{[]Verdict{Proven, Proven}, "", "info"},
		{[]Verdict{Proven, Booted}, "", "warning"},
		{[]Verdict{Skipped}, "", "warning"},
		{[]Verdict{Proven, Error}, "", "warning"},
		{[]Verdict{Booted, Failed}, "", "error"},
		{nil, "ssh unreachable", "error"},
	}
	envOnly := RunSummary{EnvironmentErrors: []string{"x"}, Results: []GuestResult{{Verdict: Proven}}}
	if got := envOnly.Severity(); got != "warning" {
		t.Errorf("environment errors alone: got %s want warning", got)
	}
	envFailed := RunSummary{EnvironmentErrors: []string{"x"}, Results: []GuestResult{{Verdict: Failed}}}
	if got := envFailed.Severity(); got != "error" {
		t.Errorf("environment errors plus FAILED: got %s want error", got)
	}
	envRunErr := RunSummary{EnvironmentErrors: []string{"x"}, RunError: "boom"}
	if got := envRunErr.Severity(); got != "error" {
		t.Errorf("environment errors plus run error: got %s want error", got)
	}
	for _, c := range cases {
		s := RunSummary{RunError: c.runErr}
		for _, v := range c.vs {
			s.Results = append(s.Results, GuestResult{Verdict: v})
		}
		if got := s.Severity(); got != c.want {
			t.Errorf("%v %q: got %s want %s", c.vs, c.runErr, got, c.want)
		}
	}
}

// A guest skipped because its snapshot is already proven is good news: it
// must not raise the severity. Any other SKIPPED still warns.
func TestSeverityAlreadyProven(t *testing.T) {
	proven := GuestResult{Verdict: Proven}
	already := GuestResult{Verdict: Skipped, Reason: "already proven", AlreadyProven: true}
	other := GuestResult{Verdict: Skipped, Reason: "insufficient node memory"}
	cases := []struct {
		rs   []GuestResult
		want string
	}{
		{[]GuestResult{already}, "info"},
		{[]GuestResult{proven, already, already}, "info"},
		{[]GuestResult{already, other}, "warning"},
		{[]GuestResult{already, {Verdict: Failed}}, "error"},
	}
	for i, c := range cases {
		if got := (RunSummary{Results: c.rs}).Severity(); got != c.want {
			t.Errorf("case %d: got %s want %s", i, got, c.want)
		}
	}
}

func TestCounts(t *testing.T) {
	s := RunSummary{Results: []GuestResult{{Verdict: Proven}, {Verdict: Proven}, {Verdict: Failed}}}
	c := s.Counts()
	if c[Proven] != 2 || c[Failed] != 1 || c[Booted] != 0 {
		t.Fatal(c)
	}
}
