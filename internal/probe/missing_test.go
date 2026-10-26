package probe_test

import (
	"context"
	"errors"
	"maps"
	"reflect"
	"slices"
	"strings"
	"testing"

	"git.breadthanalytics.com/tdiana/PVE-Backup-Proofing/internal/config"
	"git.breadthanalytics.com/tdiana/PVE-Backup-Proofing/internal/model"
	"git.breadthanalytics.com/tdiana/PVE-Backup-Proofing/internal/probe"
	"git.breadthanalytics.com/tdiana/PVE-Backup-Proofing/internal/probe/probetest"
)

func showC(unit string) string {
	return "systemctl show -p Type -p What -p Where -p RequiresMountsFor -p Requires -p BindsTo -- " + unit
}

// failing returns a degraded healthy guest whose failed units are given, with
// each unit's `systemctl show` output scripted from props.
func failing(props map[string]string) probetest.Scripted {
	ex := healthy()
	ex[sysState] = out("degraded\n")
	var lines []string
	for _, u := range slices.Sorted(maps.Keys(props)) {
		lines = append(lines, "● "+u+" loaded failed failed x")
		ex[showC(u)] = out(props[u])
	}
	ex[failedC] = out(strings.Join(lines, "\n") + "\n")
	return ex
}

func runMissing(ex probe.Executor, missing ...model.MissingPiece) probe.Outcome {
	return probe.Run(context.Background(), ex, probe.Input{OS: probe.Linux, Checks: config.Checks{}, Missing: missing})
}

func coveredMap(o probe.Outcome) map[string]model.NotCovered {
	m := map[string]model.NotCovered{}
	for _, n := range o.NotCovered {
		m[n.Unit] = n
	}
	return m
}

var scsi1 = model.MissingPiece{Kind: "excluded-disk", Key: "scsi1", Detail: "backup=0"}

func TestBindMountAndDependentServiceCovered(t *testing.T) {
	ex := failing(map[string]string{
		"media.mount":      "Type=none\nWhat=/mnt/host/media\nWhere=/media\nRequiresMountsFor=/\nRequires=-.mount\nBindsTo=\n",
		"jellyfin.service": "Type=simple\nWhat=\nWhere=\nRequiresMountsFor=/media/movies /var/lib/jellyfin\nRequires=system.slice\nBindsTo=\n",
	})
	o := runMissing(ex, model.MissingPiece{Kind: "bind-mount", Key: "mp0", Path: "/media"})
	if o.Verdict != model.Proven || len(o.FailedUnits) != 0 {
		t.Fatalf("got %+v", o)
	}
	c := coveredMap(o)
	if len(c) != 2 {
		t.Fatalf("covered %+v", o.NotCovered)
	}
	want := "Found jellyfin.service depends on /media (bind mount mp0): not backed up, skipped"
	if got := c["jellyfin.service"].LogLine(); got != want {
		t.Fatalf("got %q", got)
	}
	if c["media.mount"].Piece.Path != "/media" {
		t.Fatalf("media.mount %+v", c["media.mount"])
	}
}

func TestPathContainmentIsSegmentAware(t *testing.T) {
	ex := failing(map[string]string{
		"media2.mount": "Type=ext4\nWhat=/dev/sdb1\nWhere=/media2\n",
		"app.service":  "RequiresMountsFor=/media2/x\n",
	})
	ex["test -e /dev/sdb1"] = out("")
	o := runMissing(ex, model.MissingPiece{Kind: "bind-mount", Key: "mp0", Path: "/media/"})
	if o.Verdict != model.Failed || len(o.NotCovered) != 0 || len(o.FailedUnits) != 2 {
		t.Fatalf("got %+v", o)
	}
}

func TestMissingDiskCovered(t *testing.T) {
	ex := failing(map[string]string{
		"srv-pgdata.mount":   "Type=ext4\nWhat=/dev/disk/by-uuid/abc\nWhere=/srv/pgdata\n",
		"postgresql.service": "RequiresMountsFor=/srv/pgdata\n",
	})
	ex["test -e /dev/disk/by-uuid/abc"] = probe.ExecResult{ExitCode: 1}
	o := runMissing(ex, scsi1)
	if o.Verdict != model.Proven || len(o.FailedUnits) != 0 {
		t.Fatalf("got %+v", o)
	}
	c := coveredMap(o)
	wantPiece := model.MissingPiece{Kind: "excluded-disk", Key: "scsi1", Path: "/srv/pgdata", Detail: "backup=0"}
	if c["srv-pgdata.mount"].Piece != wantPiece {
		t.Fatalf("piece %+v", c["srv-pgdata.mount"])
	}
	want := "Found postgresql.service depends on /srv/pgdata (disk scsi1, backup=0): not backed up, skipped"
	if got := c["postgresql.service"].LogLine(); got != want {
		t.Fatalf("got %q", got)
	}
}

func TestMissingDiskSeveralCandidates(t *testing.T) {
	ex := failing(map[string]string{
		"data.mount": "Type=xfs\nWhat=UUID=abc\nWhere=/data\n",
	})
	ex["test -e /dev/disk/by-uuid/abc"] = probe.ExecResult{ExitCode: 1}
	o := runMissing(ex, scsi1, model.MissingPiece{Kind: "excluded-disk", Key: "scsi2", Detail: "backup=0"})
	if o.Verdict != model.Proven || len(o.NotCovered) != 1 {
		t.Fatalf("got %+v", o)
	}
	if p := o.NotCovered[0].Piece; p.Key != "scsi1/scsi2" || p.Path != "/data" || p.Kind != "excluded-disk" {
		t.Fatalf("piece %+v", p)
	}
}

func TestSourceResolution(t *testing.T) {
	for what, dev := range map[string]string{
		"LABEL=x":      "/dev/disk/by-label/x",
		"PARTUUID=p1":  "/dev/disk/by-partuuid/p1",
		"/dev/vdb1":    "/dev/vdb1",
		"UUID=abc-def": "/dev/disk/by-uuid/abc-def",
	} {
		ex := failing(map[string]string{"d.mount": "What=" + what + "\nWhere=/d\n"})
		ex["test -e "+dev] = probe.ExecResult{ExitCode: 1}
		if o := runMissing(ex, scsi1); o.Verdict != model.Proven {
			t.Fatalf("%s: %+v", what, o)
		}
	}
}

func TestMissingDiskWithoutExcludedDiskUntraced(t *testing.T) {
	ex := failing(map[string]string{
		"srv-pgdata.mount": "Type=ext4\nWhat=/dev/disk/by-uuid/abc\nWhere=/srv/pgdata\n",
	})
	ex["test -e /dev/disk/by-uuid/abc"] = probe.ExecResult{ExitCode: 1}
	o := runMissing(ex, model.MissingPiece{Kind: "bind-mount", Key: "mp0", Path: "/media"})
	if o.Verdict != model.Failed || o.Reason != "failed: srv-pgdata.mount" || len(o.NotCovered) != 0 {
		t.Fatalf("got %+v", o)
	}
}

func TestPresentDeviceUntraced(t *testing.T) {
	ex := failing(map[string]string{
		"srv.mount": "Type=ext4\nWhat=/dev/vdb1\nWhere=/srv\n",
	})
	ex["test -e /dev/vdb1"] = out("")
	if o := runMissing(ex, scsi1); o.Verdict != model.Failed || len(o.NotCovered) != 0 {
		t.Fatalf("got %+v", o)
	}
}

func TestNetworkStorageCovered(t *testing.T) {
	cases := []string{
		"Type=nfs4\nWhat=nas:/export\nWhere=/mnt/nas\n",
		"Type=\nWhat=nas:/export\nWhere=/mnt/nas\n",
		"Type=cifs\nWhat=//nas/share\nWhere=/mnt/nas\n",
		"Type=smb3\nWhat=whatever\nWhere=/mnt/nas\n",
	}
	for _, props := range cases {
		ex := failing(map[string]string{"mnt-nas.mount": props})
		o := runMissing(ex)
		if o.Verdict != model.Proven || len(o.NotCovered) != 1 {
			t.Fatalf("%q: %+v", props, o)
		}
		p := o.NotCovered[0].Piece
		if p.Kind != "network-storage" || p.Key != "" || p.Path != "/mnt/nas" {
			t.Fatalf("%q: piece %+v", props, p)
		}
	}
	ex := failing(map[string]string{"mnt-nas.mount": cases[0]})
	want := "Found mnt-nas.mount depends on /mnt/nas (network storage, nas:/export): not backed up, skipped"
	if got := runMissing(ex).NotCovered[0].LogLine(); got != want {
		t.Fatalf("got %q", got)
	}
}

func TestUnrelatedServiceUntraced(t *testing.T) {
	ex := failing(map[string]string{
		"media.mount":        "Type=none\nWhat=/mnt/host/media\nWhere=/media\n",
		"postgresql.service": "Type=notify\nRequiresMountsFor=/var/lib/postgresql\nRequires=system.slice\n",
	})
	ex["journalctl -b --no-pager -n 200"] = out("log")
	o := runMissing(ex, model.MissingPiece{Kind: "bind-mount", Key: "mp0", Path: "/media"})
	if o.Verdict != model.Failed || o.Reason != "failed: postgresql.service" {
		t.Fatalf("got %+v", o)
	}
	if !reflect.DeepEqual(o.FailedUnits, []string{"postgresql.service"}) || len(o.NotCovered) != 1 || o.NotCovered[0].Unit != "media.mount" {
		t.Fatalf("got %+v", o)
	}
}

func TestDependencyChainCovered(t *testing.T) {
	ex := failing(map[string]string{
		"c.service": "BindsTo=b.service\n",
		"b.service": "Requires=sysinit.target a.mount\n",
		"a.mount":   "Type=nfs\nWhat=nas:/a\nWhere=/a\n",
	})
	o := runMissing(ex)
	if o.Verdict != model.Proven || len(o.NotCovered) != 3 || len(o.FailedUnits) != 0 {
		t.Fatalf("got %+v", o)
	}
	for u, n := range coveredMap(o) {
		if n.Piece.Path != "/a" || n.Piece.Kind != "network-storage" {
			t.Fatalf("%s: %+v", u, n)
		}
	}
}

func TestCoveredButProfileFails(t *testing.T) {
	ex := failing(map[string]string{"a.mount": "Type=nfs\nWhat=nas:/a\nWhere=/a\n"})
	ex[ssC] = out("")
	o := runMissing(ex)
	if o.Verdict != model.Failed || !strings.HasPrefix(o.Reason, "check failed:") || len(o.NotCovered) != 1 {
		t.Fatalf("got %+v", o)
	}
}

// The docs' own example: postgresql.service fails because its data disk was
// not backed up. That says nothing about the backup, so the postgresql
// profile's checks (pg_isready, which fails with the server down) are not
// run either; the guest is PROVEN with the unit listed as not covered (code
// review 5.9).
func TestProfileOfNotCoveredUnitSkipped(t *testing.T) {
	ex := failing(map[string]string{
		"srv-pgdata.mount":   "Type=ext4\nWhat=/dev/disk/by-uuid/abc\nWhere=/srv/pgdata\n",
		"postgresql.service": "RequiresMountsFor=/srv/pgdata\n",
	})
	ex["test -e /dev/disk/by-uuid/abc"] = probe.ExecResult{ExitCode: 1}
	ex[enabledC] = out("postgresql.service enabled enabled\n")
	ex["sh -c pg_isready -q"] = probe.ExecResult{ExitCode: 2}
	o := runMissing(ex, scsi1)
	if o.Verdict != model.Proven || len(o.NotCovered) != 2 {
		t.Fatalf("got %v %q, not covered %v", o.Verdict, o.Reason, o.NotCovered)
	}
	for _, c := range o.Checks {
		if strings.HasPrefix(c.Name, "postgresql:") {
			t.Errorf("ran a check of a not-covered unit: %+v", c)
		}
	}
}

func TestShowFailureUntraced(t *testing.T) {
	ex := failing(map[string]string{"a.mount": ""})
	ex[showC("a.mount")] = probe.ExecResult{ExitCode: 1, Stdout: "Type=nfs\nWhat=nas:/a\nWhere=/a\n"}
	if o := runMissing(ex); o.Verdict != model.Failed || len(o.NotCovered) != 0 {
		t.Fatalf("got %+v", o)
	}
}

// cmdErr fails commands whose first argv element is cmd with err, delegating the rest.
type cmdErr struct {
	probetest.Scripted
	cmd string
	err error
}

func (s cmdErr) Exec(ctx context.Context, argv []string) (probe.ExecResult, error) {
	if argv[0] == s.cmd && (s.cmd != "systemctl" || argv[1] == "show") {
		return probe.ExecResult{}, s.err
	}
	return s.Scripted.Exec(ctx, argv)
}

// A transport error while tracing leaves the unit untraced (never covered),
// but the guest cannot be judged: the verdict is ERROR, not FAILED.
func TestExecErrorDuringTracingUntraced(t *testing.T) {
	ex := failing(map[string]string{"a.mount": "Type=nfs\nWhat=nas:/a\nWhere=/a\n"})
	o := runMissing(cmdErr{ex, "systemctl", errors.New("boom")})
	if o.Verdict != model.Error || o.Reason != "probe transport error: boom" || len(o.NotCovered) != 0 ||
		!reflect.DeepEqual(o.FailedUnits, []string{"a.mount"}) {
		t.Fatalf("got %+v", o)
	}
	ex = failing(map[string]string{"d.mount": "What=/dev/vdb\nWhere=/d\n"})
	o = runMissing(cmdErr{ex, "test", errors.New("boom")}, scsi1)
	if o.Verdict != model.Error || len(o.NotCovered) != 0 {
		t.Fatalf("got %+v", o)
	}
}

func TestRootPiecePathCoversNothing(t *testing.T) {
	ex := failing(map[string]string{"app.service": "RequiresMountsFor=/var/lib/app\n"})
	if o := runMissing(ex, model.MissingPiece{Kind: "excluded-mountpoint", Key: "rootfs", Path: "/"}); o.Verdict != model.Failed {
		t.Fatalf("got %+v", o)
	}
}

func TestNestedMountCoveredViaRequiresExtendsPaths(t *testing.T) {
	ex := failing(map[string]string{
		"a.mount":     "Type=nfs\nWhat=nas:/a\nWhere=/a\n",
		"b-c.mount":   "Type=ext4\nWhat=/a/img\nWhere=/b/c\nRequires=a.mount\n",
		"app.service": "RequiresMountsFor=/b/c/data\n",
	})
	ex["test -e /a/img"] = out("")
	if o := runMissing(ex); o.Verdict != model.Proven || len(o.NotCovered) != 3 {
		t.Fatalf("got %+v", o)
	}
}

// The absent-device rule applies only to block-device sources: a loop-mounted
// image file (or any other non-/dev path) that is missing is not explained by
// an excluded disk, so the failure stays real.
func TestMissingNonDeviceSourceUntraced(t *testing.T) {
	for _, what := range []string{"/var/lib/images/disk.img", "/srv/iso/debian.iso", "/dev-images/x.img"} {
		ex := failing(map[string]string{"img.mount": "Type=ext4\nWhat=" + what + "\nWhere=/img\n"})
		ex["test -e "+what] = probe.ExecResult{ExitCode: 1}
		ex["journalctl -b --no-pager -n 200"] = out("")
		o := runMissing(ex, scsi1)
		if o.Verdict != model.Failed || o.Reason != "failed: img.mount" || len(o.NotCovered) != 0 {
			t.Errorf("%s: got %+v", what, o)
		}
	}
}

// A unit name starting with '-' (the root mount) must not be read as an
// option: systemctl show gets `--` before it.
func TestDashUnitNameShownAfterDoubleDash(t *testing.T) {
	ex := failing(map[string]string{"-.mount": "Type=nfs\nWhat=nas:/\nWhere=/\n"})
	o := runMissing(ex)
	if o.Verdict != model.Proven || len(o.NotCovered) != 1 || o.NotCovered[0].Unit != "-.mount" {
		t.Fatalf("got %+v", o)
	}
}

// Only `test -e` exiting 1 proves a device absent; a usage error (2) or a
// missing command (127) leaves the failure untraced.
func TestTestExitCodesOtherThanOneAreUntraced(t *testing.T) {
	for _, code := range []int{0, 2, 127} {
		ex := failing(map[string]string{"d.mount": "Type=ext4\nWhat=/dev/vdb1\nWhere=/d\n"})
		ex["test -e /dev/vdb1"] = probe.ExecResult{ExitCode: code}
		ex["journalctl -b --no-pager -n 200"] = out("")
		if o := runMissing(ex, scsi1); o.Verdict != model.Failed || len(o.NotCovered) != 0 {
			t.Errorf("exit %d: got %+v", code, o)
		}
	}
}

// An EFI or TPM state disk removed from the copy cannot explain a failed
// mount of an absent device: it holds no filesystem (orphan-disk review).
func TestEFIDiskDoesNotExplainMounts(t *testing.T) {
	ex := failing(map[string]string{"data.mount": "Type=ext4\nWhat=/dev/disk/by-uuid/abc\nWhere=/data\n"})
	ex["test -e /dev/disk/by-uuid/abc"] = probe.ExecResult{ExitCode: 1}
	o := runMissing(ex, model.MissingPiece{Kind: "excluded-disk", Key: "efidisk0", Detail: "not in the backup"})
	if o.Verdict != model.Failed || len(o.NotCovered) != 0 {
		t.Fatalf("got %v %q, covered %v", o.Verdict, o.Reason, o.NotCovered)
	}
}
