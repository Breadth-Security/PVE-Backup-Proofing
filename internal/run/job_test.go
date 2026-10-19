package run

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"git.breadthanalytics.com/tdiana/pbv/internal/config"
	"git.breadthanalytics.com/tdiana/pbv/internal/model"
	"git.breadthanalytics.com/tdiana/pbv/internal/notes"
	"git.breadthanalytics.com/tdiana/pbv/internal/probe"
	"git.breadthanalytics.com/tdiana/pbv/internal/probe/probetest"
	"git.breadthanalytics.com/tdiana/pbv/internal/pve/pvefake"
	"git.breadthanalytics.com/tdiana/pbv/internal/state"
	"git.breadthanalytics.com/tdiana/pbv/internal/target"
	"git.breadthanalytics.com/tdiana/pbv/internal/target/targetfake"
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
	panicNotes                            bool
	storages                              []string
}

func (a *recAPI) SetVolumeAttrs(ctx context.Context, node, storage, volid string, notes *string, protected *bool) error {
	a.storages = append(a.storages, node+"/"+storage)
	switch {
	case notes != nil:
		a.rec.add(ctx, "SetNotes")
		if a.panicNotes {
			panic("notes boom")
		}
		if a.failNotes {
			return errors.New("notes refused")
		}
	case protected != nil:
		a.rec.add(ctx, fmt.Sprintf("SetProtected=%t", *protected))
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
}

func (t *recTarget) Restore(ctx context.Context, b model.Backup, vmid int) error {
	t.rec.add(ctx, "Restore")
	return t.Fake.Restore(ctx, b, vmid)
}

func (t *recTarget) Sanitize(ctx context.Context, kind model.GuestKind, vmid int, v string) ([]model.MissingPiece, error) {
	t.rec.add(ctx, "Sanitize")
	return t.Fake.Sanitize(ctx, kind, vmid, v)
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

func (t *recTarget) Destroy(ctx context.Context, kind model.GuestKind, vmid int) error {
	t.rec.add(ctx, "Destroy")
	if t.panicDestroy {
		panic("destroy boom")
	}
	return t.Fake.Destroy(ctx, kind, vmid)
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
	return f
}

func (f *fixture) run(ctx context.Context) model.GuestResult {
	return proofOne(ctx, f.d, f.b, tmpVMID)
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
	if f.logBuf.Len() != 0 {
		t.Errorf("unexpected log: %s", f.logBuf)
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
		{"proven becomes error", func(*fixture) {}, model.Error, "cleanup failed: destroy (booted ok)", "started"},
		{"failed kept", func(f *fixture) {
			ex := healthy()
			ex[failedC] = out("postgresql.service loaded failed failed PG\n")
			f.tgt.Exec[tmpVMID] = ex
		}, model.Failed, "failed: postgresql.service; cleanup failed: destroy", "started"},
		{"booted kept", func(f *fixture) {
			f.tgt.Boot[tmpVMID] = target.BootInfo{AgentMissing: true}
		}, model.Booted, "guest agent missing; cleanup failed: destroy", "started"},
		{"error kept", func(f *fixture) {
			f.tgt.SanitizeErr = map[int]error{tmpVMID: fmt.Errorf("%w: hostpci0", target.ErrUnsafe)}
		}, model.Error, "unsafe config: hostpci0; cleanup failed: destroy", "restored"},
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
	if j, ok := f.inFlight(); !ok || j.Stage != "started" {
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
	if !ok || j.Stage != "destroyed" || j.ProtectedOrig == nil || *j.ProtectedOrig {
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
		{"no boot", target.BootInfo{}, errors.New("guest 990000 not running after 5m0s"), model.Failed, "did not boot: guest 990000 not running after 5m0s", false},
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

func TestLXCNameFromHostname(t *testing.T) {
	f := newFixture(t)
	f.b.Key = model.GuestKey{Kind: model.KindLXC, VMID: 101}
	f.api.BackupConfigs[volid] = "hostname: web01\ntags: web\n"
	if res := f.run(context.Background()); res.Name != "web01" {
		t.Errorf("name %q", res.Name)
	}
}
