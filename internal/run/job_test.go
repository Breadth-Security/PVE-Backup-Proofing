package run

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/config"
	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/model"
	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/notes"
	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/probe"
	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/probe/probetest"
	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/pve"
	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/pve/pvefake"
	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/sanitize"
	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/state"
	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/target"
	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/target/targetfake"
)

const (
	tmpVMID = 990000
	volid   = "pbs:backup/vm/101/2026-10-18T22:00:00Z"
	sysCmd  = "systemctl is-system-running --wait"
	failedC = "systemctl list-units --failed --plain --no-legend --no-pager"
)

var now = time.Date(2026, 10, 19, 2, 14, 0, 0, time.UTC)

func out(s string) probe.ExecResult { return probe.ExecResult{Stdout: s} }

func healthy() probetest.Scripted {
	return probetest.Scripted{
		sysCmd:                    out("running\n"),
		"findmnt -n -o OPTIONS /": out("rw,relatime\n"),
		failedC:                   out(""),
		"systemctl list-unit-files --state=enabled --no-legend --plain": out(""),
		"ss -ltnH": out(""),
		"systemctl list-units --type=service --state=auto-restart,auto-restart-queued --plain --no-legend --no-pager": out(""),
	}
}

// recorder keeps one ordered log across the API and target fakes. Each entry
// carries the InFlight stage recorded for tmpVMID when the call was made, and
// "!ctx" when the call got an already-cancelled context.
type recorder struct {
	mu    sync.Mutex
	log   []string
	store *state.Store
}

func (r *recorder) add(ctx context.Context, what string) {
	stage := "-"
	if j, ok := r.store.Snapshot().InFlight[tmpVMID]; ok {
		stage = j.Stage
	}
	if ctx.Err() != nil {
		what += " !ctx"
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.log = append(r.log, what+" @"+stage)
}

func (r *recorder) entries() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.log)
}

type recAPI struct {
	*pvefake.Fake
	rec                                   *recorder
	failNotes, failProtect, failUnprotect bool
	// protectErr, when set, is returned by every SetProtected call.
	protectErr error
	panicNotes bool
	storages   []string
	// onNotes runs at each notes write, before it.
	onNotes func()
}

func (a *recAPI) SetVolumeAttrs(ctx context.Context, node, storage, volid string, notes *string, protected *bool) error {
	a.storages = append(a.storages, node+"/"+storage)
	switch {
	case notes != nil:
		a.rec.add(ctx, "SetNotes")
		if a.onNotes != nil {
			a.onNotes()
		}
		if a.panicNotes {
			panic("notes boom")
		}
		if a.failNotes {
			return errors.New("notes refused")
		}
	case protected != nil:
		a.rec.add(ctx, fmt.Sprintf("SetProtected=%t", *protected))
		if a.protectErr != nil {
			return a.protectErr
		}
		if a.failUnprotect && !*protected {
			return errors.New("unprotect refused")
		}
		if a.failProtect && *protected {
			return errors.New("protect refused")
		}
	}
	return a.Fake.SetVolumeAttrs(ctx, node, storage, volid, notes, protected)
}

type recTarget struct {
	*targetfake.Fake
	rec *recorder
	// onBoot runs inside WaitBooted (e.g. to cancel the job's context).
	onBoot func(ctx context.Context) error
	// panicDestroy makes Destroy panic.
	panicDestroy bool
	// origTags and destroyRec are what Restore and Destroy were last given.
	origTags   string
	destroyRec target.Recorded
}

func (t *recTarget) Restore(ctx context.Context, b model.Backup, vmid int, origTags string) error {
	t.rec.add(ctx, "Restore")
	t.origTags = origTags
	return t.Fake.Restore(ctx, b, vmid, origTags)
}

func (t *recTarget) Sanitize(ctx context.Context, kind model.GuestKind, vmid int) (target.Sanitized, error) {
	t.rec.add(ctx, "Sanitize")
	return t.Fake.Sanitize(ctx, kind, vmid)
}

func (t *recTarget) Start(ctx context.Context, kind model.GuestKind, vmid int) error {
	t.rec.add(ctx, "Start")
	return t.Fake.Start(ctx, kind, vmid)
}

func (t *recTarget) WaitBooted(ctx context.Context, kind model.GuestKind, vmid int, timeout time.Duration) (target.BootInfo, error) {
	t.rec.add(ctx, fmt.Sprintf("WaitBooted %s", timeout))
	if t.onBoot != nil {
		if err := t.onBoot(ctx); err != nil {
			return target.BootInfo{}, err
		}
	}
	return t.Fake.WaitBooted(ctx, kind, vmid, timeout)
}

func (t *recTarget) Destroy(ctx context.Context, kind model.GuestKind, vmid int, rec target.Recorded) error {
	t.rec.add(ctx, "Destroy")
	t.destroyRec = rec
	if t.panicDestroy {
		panic("destroy boom")
	}
	return t.Fake.Destroy(ctx, kind, vmid, rec)
}

type fixture struct {
	d      Deps
	api    *recAPI
	tgt    *recTarget
	rec    *recorder
	store  *state.Store
	b      model.Backup
	logBuf *bytes.Buffer
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	store, err := state.Open(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	rec := &recorder{store: store}
	b := model.Backup{
		Storage: "pbs", VolID: volid, Notes: "db01\nmonthly keep",
		Key:  model.GuestKey{Kind: model.KindQEMU, VMID: 101},
		Time: time.Date(2026, 10, 18, 22, 0, 0, 0, time.UTC),
	}
	pf := pvefake.New()
	pf.Backups["pbs"] = []model.Backup{b}
	pf.BackupConfigs[volid] = "name: db01\ntags: prod;db\nagent: 1\n"
	tf := targetfake.New(tmpVMID, tmpVMID+9)
	tf.Boot = map[int]target.BootInfo{tmpVMID: {OS: probe.Linux}}
	tf.Exec = map[int]probe.Executor{tmpVMID: healthy()}

	var cfg config.Config
	cfg.Target.Node = "pve1"
	cfg.Restore.Storage = "local-lvm"
	cfg.Restore.BootTimeout = 5 * time.Minute

	var logBuf bytes.Buffer
	f := &fixture{
		api: &recAPI{Fake: pf, rec: rec}, tgt: &recTarget{Fake: tf, rec: rec},
		rec: rec, store: store, b: b, logBuf: &logBuf,
	}
	f.d = Deps{
		Cfg: cfg, API: f.api, Target: f.tgt, Store: store,
		Log: slog.New(slog.NewTextHandler(&logBuf, nil)),
		Now: func() time.Time { return now },
	}
	// A job writes the attributes of its own backup only (code review I19,
	// carried to this fixture by the #49 review). f.b is read at cleanup, so
	// a test that changes it is covered too.
	t.Cleanup(func() { assertWritesOnly(t, f.api.CallLog(), map[string]bool{f.b.VolID: true}) })
	return f
}

func (f *fixture) run(ctx context.Context) model.GuestResult {
	return proofOne(ctx, f.d, f.b, tmpVMID, sanitize.ParseConfigText(f.api.BackupConfigs[f.b.VolID]), guestLogger(f.d, f.b.Key))
}

// notes returns the backup's notes as stored in the fake.
func (f *fixture) notes() string { return f.api.Backups["pbs"][0].Notes }

func (f *fixture) protected() bool { return f.api.Backups["pbs"][0].Protected }

func (f *fixture) inFlight() (state.Job, bool) {
	j, ok := f.store.Snapshot().InFlight[tmpVMID]
	return j, ok
}

func TestHappyPath(t *testing.T) {
	f := newFixture(t)
	res := f.run(context.Background())

	if res.Verdict != model.Proven {
		t.Fatalf("verdict %s %q", res.Verdict, res.Reason)
	}
	want := []string{
		"SetProtected=true @protected",
		"Restore @restored",
		"Sanitize @restored",
		"Start @started",
		"WaitBooted 5m0s @started",
		"Destroy @started",
		"SetProtected=false @destroyed",
		"SetNotes @-",
	}
	if got := f.rec.entries(); !slices.Equal(got, want) {
		t.Errorf("calls:\n got %q\nwant %q", got, want)
	}
	if res.Name != "db01" || res.TempVMID != tmpVMID || res.Backup.VolID != volid {
		t.Errorf("result %+v", res)
	}
	// The guest is PBP's by the job's record; the backup's name goes with it
	// so the target need not read the original config again.
	if want := (target.Recorded{VolID: volid, Name: "db01"}); f.tgt.destroyRec != want {
		t.Errorf("Destroy got %+v, want %+v", f.tgt.destroyRec, want)
	}
	if f.tgt.origTags != "prod;db" {
		t.Errorf("Restore got tags %q", f.tgt.origTags)
	}
	if f.protected() {
		t.Error("backup left protected")
	}
	if _, ok := f.inFlight(); ok {
		t.Error("InFlight entry not removed")
	}
	wantNotes := "PBP PROVEN 2026-10-19 02:14 · " + res.Reason + "\ndb01\nmonthly keep"
	if f.notes() != wantNotes {
		t.Errorf("notes %q want %q", f.notes(), wantNotes)
	}
	if got := f.store.Snapshot().LastProven["qemu/101"]; !got.Equal(now) {
		t.Errorf("LastProven = %v", got)
	}
	for _, s := range f.api.storages {
		if s != "pve1/pbs" {
			t.Errorf("SetVolumeAttrs went to %s, want the backup's storage pve1/pbs", s)
		}
	}
	if l := f.logBuf.String(); strings.Contains(l, "level=WARN") || strings.Contains(l, "level=ERROR") {
		t.Errorf("unexpected warning or error: %s", l)
	}
}

// The run's listing is old by the time a job starts: an admin may have
// protected the backup since (code review I1). PBP reads the flag again
// just before protecting, so it neither sets it nor clears it at teardown.
func TestProtectedSinceListing(t *testing.T) {
	f := newFixture(t)
	f.api.Backups["pbs"][0].Protected = true // f.b, the listing, still says false
	res := f.run(context.Background())
	if res.Verdict != model.Proven {
		t.Fatalf("verdict %s %q", res.Verdict, res.Reason)
	}
	for _, c := range f.rec.entries() {
		if strings.HasPrefix(c, "SetProtected") {
			t.Errorf("protected written: %s", c)
		}
	}
	if !f.protected() {
		t.Error("the admin's protection was removed")
	}
}

// When the flag cannot be read, the proof stops before anything is changed.
func TestProtectedReadFailure(t *testing.T) {
	f := newFixture(t)
	f.api.Fail["VolumeProtected"] = errors.New("api down")
	res := f.run(context.Background())
	if res.Verdict != model.Error || !strings.HasPrefix(res.Reason, "read backup protected flag: ") {
		t.Fatalf("got %s %q", res.Verdict, res.Reason)
	}
	for _, c := range f.rec.entries() {
		if strings.HasPrefix(c, "SetProtected") || strings.HasPrefix(c, "Restore") {
			t.Errorf("changed something: %s", c)
		}
	}
}

func TestAlreadyProtected(t *testing.T) {
	f := newFixture(t)
	f.b.Protected = true
	f.api.Backups["pbs"][0].Protected = true
	f.tgt.onBoot = func(context.Context) error {
		if j, ok := f.inFlight(); !ok || j.ProtectedOrig == nil || !*j.ProtectedOrig {
			t.Errorf("InFlight job = %+v", j)
		}
		return nil
	}
	res := f.run(context.Background())
	if res.Verdict != model.Proven {
		t.Fatalf("verdict %s %q", res.Verdict, res.Reason)
	}
	for _, c := range f.rec.entries() {
		if strings.HasPrefix(c, "SetProtected") {
			t.Errorf("protected written: %s", c)
		}
	}
	if !f.protected() {
		t.Error("backup no longer protected")
	}
	if _, ok := f.inFlight(); ok {
		t.Error("InFlight entry not removed")
	}
}

func TestInFlightRecordsOriginalProtected(t *testing.T) {
	f := newFixture(t)
	f.tgt.onBoot = func(context.Context) error {
		j, ok := f.inFlight()
		if !ok || j.ProtectedOrig == nil || *j.ProtectedOrig || j.Kind != model.KindQEMU ||
			j.VolID != volid || j.Storage != "pbs" || j.TempVMID != tmpVMID {
			t.Errorf("InFlight job = %+v", j)
		}
		return nil
	}
	f.run(context.Background())
}

func TestUnsafeNeverStarts(t *testing.T) {
	f := newFixture(t)
	f.tgt.SanitizeErr = map[int]error{tmpVMID: fmt.Errorf("%w: hostpci0", target.ErrUnsafe)}
	res := f.run(context.Background())
	if res.Verdict != model.Error || res.Reason != "unsafe config: hostpci0" {
		t.Errorf("result %s %q", res.Verdict, res.Reason)
	}
	calls := strings.Join(f.rec.entries(), "\n")
	if strings.Contains(calls, "Start") {
		t.Errorf("guest started:\n%s", calls)
	}
	if !strings.Contains(calls, "Destroy") {
		t.Errorf("guest not destroyed:\n%s", calls)
	}
	if !strings.HasPrefix(f.notes(), "PBP ERROR 2026-10-19 02:14 · unsafe config: hostpci0\n") {
		t.Errorf("notes %q", f.notes())
	}
}

func TestRestoreDecryptError(t *testing.T) {
	f := newFixture(t)
	f.tgt.RestoreErr = map[int]error{tmpVMID: errors.New("restore failed: decryption failed - wrong key")}
	res := f.run(context.Background())
	if res.Verdict != model.Error {
		t.Errorf("verdict %s", res.Verdict)
	}
	if !strings.HasPrefix(f.notes(), "PBP ERROR 2026-10-19 02:14 · restore failed: ") {
		t.Errorf("notes %q", f.notes())
	}
	if strings.Contains(strings.Join(f.rec.entries(), "\n"), "Sanitize") {
		t.Error("sanitize after failed restore")
	}
	if f.protected() {
		t.Error("backup left protected")
	}
}

func TestRestoreCorruptIsFailed(t *testing.T) {
	f := newFixture(t)
	f.tgt.RestoreErr = map[int]error{tmpVMID: errors.New("unable to read archive: bad chunk digest")}
	if res := f.run(context.Background()); res.Verdict != model.Failed {
		t.Errorf("verdict %s %q", res.Verdict, res.Reason)
	}
}

func TestProbeFailedNotes(t *testing.T) {
	f := newFixture(t)
	ex := healthy()
	ex[failedC] = out("postgresql.service loaded failed failed PG\n")
	ex["journalctl -b --no-pager -n 200"] = out("boot log")
	f.tgt.Exec[tmpVMID] = ex
	res := f.run(context.Background())
	if res.Verdict != model.Failed || res.Reason != "failed: postgresql.service" {
		t.Fatalf("result %s %q", res.Verdict, res.Reason)
	}
	if res.BootLog != "boot log" || !slices.Equal(res.FailedUnits, []string{"postgresql.service"}) {
		t.Errorf("result %+v", res)
	}
	want := "PBP FAILED 2026-10-19 02:14 · failed: postgresql.service\ndb01\nmonthly keep"
	if f.notes() != want {
		t.Errorf("notes %q", f.notes())
	}
	if len(f.store.Snapshot().LastProven) != 0 {
		t.Error("LastProven set for FAILED")
	}
}

func TestDestroyFailure(t *testing.T) {
	tests := []struct {
		name    string
		setup   func(f *fixture)
		verdict model.Verdict
		reason  string
		stage   string
	}{
		{"proven becomes error", func(*fixture) {}, model.Error, "cleanup failed: destroy (booted ok)", state.StageStarted},
		{"failed kept", func(f *fixture) {
			ex := healthy()
			ex[failedC] = out("postgresql.service loaded failed failed PG\n")
			f.tgt.Exec[tmpVMID] = ex
		}, model.Failed, "failed: postgresql.service; cleanup failed: destroy", state.StageStarted},
		{"booted kept", func(f *fixture) {
			f.tgt.Boot[tmpVMID] = target.BootInfo{AgentMissing: true}
		}, model.Booted, "guest agent missing; cleanup failed: destroy", state.StageStarted},
		{"error kept", func(f *fixture) {
			f.tgt.SanitizeErr = map[int]error{tmpVMID: fmt.Errorf("%w: hostpci0", target.ErrUnsafe)}
		}, model.Error, "unsafe config: hostpci0; cleanup failed: destroy", state.StageRestored},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			tc.setup(f)
			f.tgt.DestroyErr = map[int]error{tmpVMID: errors.New("destroy timed out")}
			res := f.run(context.Background())
			if res.Verdict != tc.verdict || res.Reason != tc.reason {
				t.Errorf("result %s %q", res.Verdict, res.Reason)
			}
			j, ok := f.inFlight()
			if !ok || j.Stage != tc.stage {
				t.Errorf("InFlight = %+v, %v", j, ok)
			}
			if f.protected() {
				t.Error("backup left protected")
			}
			want := notes.Line(tc.verdict, now, tc.reason)
			if !strings.HasPrefix(f.notes(), want+"\n") {
				t.Errorf("notes %q", f.notes())
			}
			if len(f.store.Snapshot().LastProven) != 0 {
				t.Error("LastProven set")
			}
		})
	}
}

type panicExec struct{}

func (panicExec) Exec(context.Context, []string) (probe.ExecResult, error) {
	panic("boom from guest")
}

func TestPanicStillTearsDown(t *testing.T) {
	f := newFixture(t)
	f.tgt.Exec[tmpVMID] = panicExec{}
	res := f.run(context.Background())
	if res.Verdict != model.Error || res.Reason != "internal error: boom from guest" {
		t.Errorf("result %s %q", res.Verdict, res.Reason)
	}
	got := f.rec.entries()
	want := []string{"Destroy @started", "SetProtected=false @destroyed", "SetNotes @-"}
	if len(got) < 3 || !slices.Equal(got[len(got)-3:], want) {
		t.Errorf("calls %q", got)
	}
	if _, ok := f.inFlight(); ok {
		t.Error("InFlight entry not removed")
	}
	if !strings.Contains(f.logBuf.String(), "boom from guest") || !strings.Contains(f.logBuf.String(), "stack=") {
		t.Errorf("panic not logged with stack: %q", f.logBuf)
	}
}

func TestDestroyPanicStillCleansUp(t *testing.T) {
	f := newFixture(t)
	f.tgt.panicDestroy = true
	res := f.run(context.Background())
	if res.Verdict != model.Error || res.Reason != "cleanup failed: destroy (booted ok)" {
		t.Errorf("result %s %q", res.Verdict, res.Reason)
	}
	got := f.rec.entries()
	want := []string{"Destroy @started", "SetProtected=false @started", "SetNotes @started"}
	if len(got) < 3 || !slices.Equal(got[len(got)-3:], want) {
		t.Errorf("calls %q", got)
	}
	if f.protected() {
		t.Error("backup left protected")
	}
	if j, ok := f.inFlight(); !ok || j.Stage != state.StageStarted {
		t.Errorf("InFlight = %+v, %v", j, ok)
	}
	if !strings.HasPrefix(f.notes(), "PBP ERROR 2026-10-19 02:14 · cleanup failed: destroy (booted ok)\n") {
		t.Errorf("notes %q", f.notes())
	}
	if !strings.Contains(f.logBuf.String(), "destroy boom") || !strings.Contains(f.logBuf.String(), "stack=") {
		t.Errorf("panic not logged with stack: %q", f.logBuf)
	}
}

func TestNotesPanicReturnsNormally(t *testing.T) {
	f := newFixture(t)
	f.api.panicNotes = true
	res := f.run(context.Background())
	if res.Verdict != model.Error || res.Reason != "internal error: notes boom" {
		t.Errorf("result %s %q", res.Verdict, res.Reason)
	}
	if _, ok := f.inFlight(); ok {
		t.Error("InFlight entry kept although cleanup succeeded")
	}
	if f.protected() {
		t.Error("backup left protected")
	}
}

func TestSanitizeErrorNeverStarts(t *testing.T) {
	f := newFixture(t)
	f.tgt.SanitizeErr = map[int]error{tmpVMID: errors.New("read config 990000: connection reset")}
	res := f.run(context.Background())
	if res.Verdict != model.Error || res.Reason != "sanitize failed: read config 990000: connection reset" {
		t.Errorf("result %s %q", res.Verdict, res.Reason)
	}
	calls := strings.Join(f.rec.entries(), "\n")
	if strings.Contains(calls, "Start") || !strings.Contains(calls, "Destroy") {
		t.Errorf("calls:\n%s", calls)
	}
}

func TestProtectFailure(t *testing.T) {
	f := newFixture(t)
	f.api.failProtect = true
	res := f.run(context.Background())
	if res.Verdict != model.Error || res.Reason != "protect backup: protect refused" {
		t.Errorf("result %s %q", res.Verdict, res.Reason)
	}
	want := []string{"SetProtected=true @protected", "SetProtected=false @protected", "SetNotes @-"}
	if got := f.rec.entries(); !slices.Equal(got, want) {
		t.Errorf("calls %q", got)
	}
	if _, ok := f.inFlight(); ok {
		t.Error("InFlight entry not removed")
	}
}

// A PBS storage whose user may not change the backup (a read-only proofing
// node, issue #30) refuses protect. Nothing changed on PBS, so PBP neither
// tries to undo it nor leaves a job for the janitor, and the reason says
// which PBS rights are missing. The errors are the ones PVE returned on pve2.
func TestProtectRefusedByPBS(t *testing.T) {
	for name, err := range map[string]error{
		"403":        &pve.APIError{Status: 500, Path: "/nodes/pve2/storage/s/content/v", Message: "403 Forbidden: 403 Forbidden\n"},
		"permission": &pve.APIError{Status: 500, Path: "/nodes/pve2/storage/s/content/v", Message: "proxmox-backup-client failed: Error: permission check failed - missing Datastore.Modify|Datastore.Backup on /datastore/x"},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			f.api.protectErr = err
			res := f.run(context.Background())
			const want = "protect backup: PBS refused: the storage's PBS user needs Datastore.Backup as the backup's owner, or Datastore.Modify (docs/dedicated-node.md)"
			if res.Verdict != model.Error || res.Reason != want {
				t.Errorf("result %s %q\nwant ERROR %q", res.Verdict, res.Reason, want)
			}
			if got := f.rec.entries(); slices.Contains(got, "SetProtected=false @protected") {
				t.Errorf("tried to undo a refused protect: %q", got)
			}
			if _, ok := f.inFlight(); ok {
				t.Error("InFlight entry left for the janitor")
			}
		})
	}
}

// Were PBP to stop between a refused protect and the end of teardown, the
// in-flight record must not ask the janitor to unprotect: PBS would refuse
// that too, every night, and block all proofing (code review 1.6a). The
// record is inspected when the job logs the refusal, which is after it
// records it and before teardown drops the record.
func TestProtectRefusedLeavesNothingToUndo(t *testing.T) {
	f := newFixture(t)
	f.api.protectErr = &pve.APIError{Status: 500, Path: "/nodes/pve2/storage/s/content/v", Message: "403 Forbidden: 403 Forbidden\n"}
	seen := false
	f.d.Log = slog.New(onMessage{slog.NewTextHandler(io.Discard, nil), "PBS refused to protect", func() {
		j, ok := f.inFlight()
		if !ok {
			t.Error("no in-flight record at the refusal")
			return
		}
		seen = true
		if j.ProtectedOrig != nil && !*j.ProtectedOrig {
			t.Error("record still says PBP protected the backup: the janitor would try to unprotect it")
		}
	}})
	f.run(context.Background())
	if !seen {
		t.Error("the refusal was not logged")
	}
}

// onMessage calls fn when a record's message contains text.
type onMessage struct {
	slog.Handler
	text string
	fn   func()
}

func (h onMessage) Handle(ctx context.Context, r slog.Record) error {
	if strings.Contains(r.Message, h.text) {
		h.fn()
	}
	return h.Handler.Handle(ctx, r)
}

func (h onMessage) WithAttrs(as []slog.Attr) slog.Handler {
	return onMessage{h.Handler.WithAttrs(as), h.text, h.fn}
}

func (h onMessage) WithGroup(n string) slog.Handler {
	return onMessage{h.Handler.WithGroup(n), h.text, h.fn}
}

func TestStartFailure(t *testing.T) {
	f := newFixture(t)
	f.tgt.StartErr = map[int]error{tmpVMID: errors.New("not enough memory")}
	res := f.run(context.Background())
	if res.Verdict != model.Error || res.Reason != "start failed: not enough memory" {
		t.Errorf("result %s %q", res.Verdict, res.Reason)
	}
	calls := strings.Join(f.rec.entries(), "\n")
	if strings.Contains(calls, "WaitBooted") || !strings.Contains(calls, "Destroy") {
		t.Errorf("calls:\n%s", calls)
	}
}

func TestUnprotectFailureKeepsVerdict(t *testing.T) {
	f := newFixture(t)
	f.api.failUnprotect = true
	res := f.run(context.Background())
	if res.Verdict != model.Proven || !strings.HasSuffix(res.Reason, "; cleanup failed: unprotect") {
		t.Errorf("result %s %q", res.Verdict, res.Reason)
	}
	j, ok := f.inFlight()
	if !ok || j.Stage != state.StageDestroyed || j.ProtectedOrig == nil || *j.ProtectedOrig {
		t.Errorf("InFlight = %+v, %v", j, ok)
	}
}

func TestBootOutcomes(t *testing.T) {
	tests := []struct {
		name         string
		boot         target.BootInfo
		bootErr      error
		verdict      model.Verdict
		reason       string
		agentMissing bool
	}{
		{"agent missing", target.BootInfo{OS: probe.Unknown, AgentMissing: true}, nil, model.Booted, "guest agent missing", true},
		{"exec blocked", target.BootInfo{OS: probe.Linux, ExecBlocked: true}, nil, model.Booted, "agent cannot run commands", false},
		{"no boot", target.BootInfo{}, fmt.Errorf("%w: status stopped", target.ErrBootFailed), model.Failed, "did not boot: guest did not boot: status stopped", false},
		{"node fault", target.BootInfo{}, errors.New("pve api: 500 internal"), model.Error, "boot check error: pve api: 500 internal", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			f.tgt.Boot[tmpVMID] = tc.boot
			if tc.bootErr != nil {
				f.tgt.BootErr = map[int]error{tmpVMID: tc.bootErr}
			}
			res := f.run(context.Background())
			if res.Verdict != tc.verdict || res.Reason != tc.reason || res.AgentMissing != tc.agentMissing {
				t.Errorf("result %s %q agentMissing=%v", res.Verdict, res.Reason, res.AgentMissing)
			}
			if strings.Contains(strings.Join(f.tgt.CallLog(), "\n"), "Executor") {
				t.Error("probe ran without a usable agent")
			}
			if !strings.HasPrefix(f.notes(), "PBP "+string(tc.verdict)+" ") {
				t.Errorf("notes %q", f.notes())
			}
		})
	}
}

func TestCancelledMidJob(t *testing.T) {
	f := newFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.tgt.onBoot = func(context.Context) error {
		cancel()
		return ctx.Err()
	}
	res := f.run(ctx)
	if res.Verdict != model.Error || res.Reason != "cancelled" {
		t.Errorf("result %s %q", res.Verdict, res.Reason)
	}
	want := []string{"Destroy @started", "SetProtected=false @destroyed", "SetNotes @-"}
	got := f.rec.entries()
	if len(got) < 3 || !slices.Equal(got[len(got)-3:], want) {
		t.Errorf("teardown calls %q (a '!ctx' mark means a cancelled context was used)", got)
	}
	if _, ok := f.inFlight(); ok {
		t.Error("InFlight entry not removed")
	}
	if !strings.HasPrefix(f.notes(), "PBP ERROR 2026-10-19 02:14 · cancelled\n") {
		t.Errorf("notes %q", f.notes())
	}
}

func TestNotesWriteFailureKeepsVerdict(t *testing.T) {
	f := newFixture(t)
	f.api.failNotes = true
	res := f.run(context.Background())
	if res.Verdict != model.Proven || strings.Contains(res.Reason, "notes") {
		t.Errorf("result %s %q", res.Verdict, res.Reason)
	}
	if !strings.Contains(f.logBuf.String(), "notes refused") {
		t.Errorf("notes failure not logged: %q", f.logBuf)
	}
}

func TestProbeGetsOriginalIdentity(t *testing.T) {
	f := newFixture(t)
	f.d.Cfg.Checks.Guests = []config.GuestChecks{
		{Match: config.GuestMatch{VMID: 101}, CheckSet: config.CheckSet{Commands: []string{"by-vmid"}}},
		{Match: config.GuestMatch{Name: "db01"}, CheckSet: config.CheckSet{Commands: []string{"by-name"}}},
		{Match: config.GuestMatch{Tag: "db"}, CheckSet: config.CheckSet{Commands: []string{"by-tag"}}},
		{Match: config.GuestMatch{Tag: "pbp"}, CheckSet: config.CheckSet{Commands: []string{"by-pbp"}}},
	}
	ex := healthy()
	for _, c := range []string{"by-vmid", "by-name", "by-tag", "by-pbp"} {
		ex["sh -c "+c] = out("")
	}
	f.tgt.Exec[tmpVMID] = ex
	res := f.run(context.Background())
	var ran []string
	for _, c := range res.Checks {
		if _, cmd, ok := strings.Cut(c.Name, ":commands:"); ok {
			ran = append(ran, cmd)
		}
	}
	slices.Sort(ran)
	if !slices.Equal(ran, []string{"by-name", "by-tag", "by-vmid"}) {
		t.Errorf("guest checks run: %q (checks %+v)", ran, res.Checks)
	}
}

// The container's ostype reaches the probe: an Alpine container is probed
// with rc-status only.
func TestProbeGetsOSType(t *testing.T) {
	f := newFixture(t)
	f.b.Key = model.GuestKey{Kind: model.KindLXC, VMID: 101}
	f.tgt.Boot[tmpVMID] = target.BootInfo{OS: probe.Linux, OSType: "alpine"}
	f.tgt.Exec[tmpVMID] = settledOpenRC{probetest.Scripted{"rc-status --crashed": out(""), "findmnt -n -o OPTIONS /": out("rw\n")}}
	if res := f.run(context.Background()); res.Verdict != model.Proven {
		t.Errorf("result %s %q", res.Verdict, res.Reason)
	}
}

// settledOpenRC answers the OpenRC settle script (sh -c "… rc-status -r …")
// as settled; without it the script exits 127 and the probe tries systemd.
type settledOpenRC struct{ probetest.Scripted }

func (s settledOpenRC) Exec(ctx context.Context, argv []string) (probe.ExecResult, error) {
	if len(argv) == 3 && argv[0] == "sh" && strings.Contains(argv[2], "rc-status -r") {
		return out(""), nil
	}
	return s.Scripted.Exec(ctx, argv)
}

// hangSys blocks "systemctl is-system-running --wait" until its context ends.
type hangSys struct{ probetest.Scripted }

func (h hangSys) Exec(ctx context.Context, argv []string) (probe.ExecResult, error) {
	if strings.Join(argv, " ") == sysCmd {
		<-ctx.Done()
		return probe.ExecResult{}, ctx.Err()
	}
	return h.Scripted.Exec(ctx, argv)
}

// restore.probe_timeout bounds the whole probe: a guest whose boot never
// settles keeps PBP waiting only that long, is ERROR (not proven, not
// blamed) and is still torn down.
func TestProbeTimeout(t *testing.T) {
	f := newFixture(t)
	f.d.Cfg.Restore.ProbeTimeout = 30 * time.Millisecond
	f.d.Cfg.Checks.CommandTimeout = time.Hour
	ex := healthy()
	ex["systemctl is-system-running"] = out("starting\n")
	f.tgt.Exec[tmpVMID] = hangSys{ex}
	start := time.Now()
	res := f.run(context.Background())
	if res.Verdict != model.Error || !strings.HasPrefix(res.Reason, "guest still starting after ") {
		t.Errorf("result %s %q", res.Verdict, res.Reason)
	}
	if time.Since(start) > 5*time.Second {
		t.Error("probe not bounded")
	}
	if _, live := f.tgt.Live[tmpVMID]; live {
		t.Error("guest not destroyed")
	}
}

func TestLXCNameFromHostname(t *testing.T) {
	f := newFixture(t)
	f.b.Key = model.GuestKey{Kind: model.KindLXC, VMID: 101}
	f.api.BackupConfigs[volid] = "hostname: web01\ntags: web\n"
	if res := f.run(context.Background()); res.Name != "web01" {
		t.Errorf("name %q", res.Name)
	}
}

// A listed backup carries only the first line of its notes (PBS "comment").
// The annotation must merge into the full notes, read from the volume, so
// user lines below the first are kept (seen on pve1: a second run would have
// dropped the vzdump guest-name note).
func TestNotesMergeIntoFullVolumeNotes(t *testing.T) {
	f := newFixture(t)
	f.api.Backups["pbs"][0].Notes = "PBP ERROR 2026-10-18 02:00 · old\ndb01\nmonthly keep"
	f.b.Notes = "PBP ERROR 2026-10-18 02:00 · old" // what the listing shows
	res := f.run(context.Background())
	want := "PBP PROVEN 2026-10-19 02:14 · " + res.Reason + "\ndb01\nmonthly keep"
	if f.notes() != want {
		t.Errorf("notes %q want %q", f.notes(), want)
	}
}

// If the full notes cannot be read, writing would risk dropping user lines:
// the notes are left alone, the failure logged and the verdict kept.
func TestNotesReadFailureLeavesNotes(t *testing.T) {
	f := newFixture(t)
	f.api.Fail["VolumeNotes"] = errors.New("notes unreadable")
	res := f.run(context.Background())
	if res.Verdict != model.Proven {
		t.Errorf("verdict %s %q", res.Verdict, res.Reason)
	}
	if f.notes() != "db01\nmonthly keep" {
		t.Errorf("notes changed: %q", f.notes())
	}
	if !strings.Contains(f.logBuf.String(), "notes unreadable") {
		t.Errorf("failure not logged: %q", f.logBuf)
	}
}

// Tags in the backed-up config may be separated by ';', ',' or spaces; each
// is a tag the guest checks can match.
func TestOriginalTagsSplitLikePVE(t *testing.T) {
	f := newFixture(t)
	f.api.BackupConfigs[volid] = "name: db01\ntags: prod,db web\n"
	f.d.Cfg.Checks.Guests = []config.GuestChecks{
		{Match: config.GuestMatch{Tag: "db"}, CheckSet: config.CheckSet{Commands: []string{"by-db"}}},
		{Match: config.GuestMatch{Tag: "web"}, CheckSet: config.CheckSet{Commands: []string{"by-web"}}},
	}
	ex := healthy()
	ex["sh -c by-db"], ex["sh -c by-web"] = out(""), out("")
	f.tgt.Exec[tmpVMID] = ex
	res := f.run(context.Background())
	var ran []string
	for _, c := range res.Checks {
		if _, cmd, ok := strings.Cut(c.Name, ":commands:"); ok {
			ran = append(ran, cmd)
		}
	}
	slices.Sort(ran)
	if !slices.Equal(ran, []string{"by-db", "by-web"}) {
		t.Errorf("guest checks run: %q", ran)
	}
}

// A restore the target refuses (here: the VMID is taken) created no guest:
// teardown destroys nothing and the job leaves no InFlight entry.
func TestRestoreRefusedSkipsDestroy(t *testing.T) {
	f := newFixture(t)
	f.tgt.OwnedGuests = []pve.GuestRef{{Key: model.GuestKey{Kind: model.KindQEMU, VMID: tmpVMID}}}
	res := f.run(context.Background())
	if res.Verdict != model.Error || !strings.Contains(res.Reason, "already exists") {
		t.Errorf("result %s %q", res.Verdict, res.Reason)
	}
	want := []string{"SetProtected=true @protected", "Restore @restored", "SetProtected=false @protected", "SetNotes @-"}
	if got := f.rec.entries(); !slices.Equal(got, want) {
		t.Errorf("calls %q", got)
	}
	if _, ok := f.inFlight(); ok {
		t.Error("InFlight entry left")
	}
}

// The proof cannot cover what Sanitize removed nor what the backup excludes
// (judged from its original config); the job merges the two, in that order.
func TestMissingPieces(t *testing.T) {
	removed := []model.MissingPiece{{Kind: "passthrough", Key: "hostpci0"}}
	j := &job{
		b:    model.Backup{Key: model.GuestKey{Kind: model.KindQEMU, VMID: 101}},
		orig: map[string]string{"scsi0": "s:vm-101-disk-0", "scsi1": "s:vm-101-disk-1,backup=0"},
	}
	want := []model.MissingPiece{removed[0], {Kind: "excluded-disk", Key: "scsi1", Detail: "backup=0"}}
	if got := j.missingPieces(removed); !slices.Equal(got, want) {
		t.Errorf("got %+v, want %+v", got, want)
	}
	if len(removed) != 1 || cap(removed) != 1 {
		t.Error("removed modified")
	}
}

func TestOneLine(t *testing.T) {
	for in, want := range map[string]string{
		"plain":              "plain",
		"a\nb":               "a b",
		"a\r\n\r\nb\n":       "a b ",
		"task failed:\nx\ny": "task failed: x y",
	} {
		if got := oneLine(in); got != want {
			t.Errorf("oneLine(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestNoNestingOnlyUnprivileged(t *testing.T) {
	for _, tc := range []struct {
		kind model.GuestKind
		cfg  map[string]string
		want bool
	}{
		{model.KindLXC, map[string]string{"unprivileged": "1"}, true},
		{model.KindLXC, map[string]string{"unprivileged": "1", "features": "nesting=1"}, false},
		{model.KindLXC, map[string]string{}, false}, // privileged
		{model.KindQEMU, map[string]string{"unprivileged": "1"}, false},
	} {
		if got := noNesting(tc.kind, tc.cfg); got != tc.want {
			t.Errorf("%s %v: %v, want %v", tc.kind, tc.cfg, got, tc.want)
		}
	}
}
