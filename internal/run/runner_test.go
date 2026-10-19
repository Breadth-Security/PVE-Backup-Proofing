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
	"git.breadthanalytics.com/tdiana/pbv/internal/probe"
	"git.breadthanalytics.com/tdiana/pbv/internal/pve"
	"git.breadthanalytics.com/tdiana/pbv/internal/pve/pvefake"
	"git.breadthanalytics.com/tdiana/pbv/internal/state"
	"git.breadthanalytics.com/tdiana/pbv/internal/target"
	"git.breadthanalytics.com/tdiana/pbv/internal/target/targetfake"
)

const (
	gib = int64(1) << 30
	tib = int64(1) << 40
)

// fakeNotifier records what the runner asks of the notification system.
type fakeNotifier struct {
	mu        sync.Mutex
	ensureErr error
	sendErr   error
	ensured   int
	sent      []model.RunSummary
}

func (n *fakeNotifier) EnsureTemplates(context.Context) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.ensured++
	return n.ensureErr
}

func (n *fakeNotifier) Send(_ context.Context, s model.RunSummary) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.sent = append(n.sent, s)
	return n.sendErr
}

func (n *fakeNotifier) sends() []model.RunSummary {
	n.mu.Lock()
	defer n.mu.Unlock()
	return slices.Clone(n.sent)
}

// fakeClock is a settable clock safe for concurrent use.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// hookAPI wraps pvefake with per-storage ListBackups failures and an
// optional panic in NodeMemory.
type hookAPI struct {
	*pvefake.Fake
	listErr     map[string]error
	panicMemory bool
}

func (a *hookAPI) ListBackups(ctx context.Context, node, storage string) ([]model.Backup, error) {
	if err := a.listErr[storage]; err != nil {
		return nil, err
	}
	return a.Fake.ListBackups(ctx, node, storage)
}

func (a *hookAPI) NodeMemory(ctx context.Context, node string) (pve.NodeMemory, error) {
	if a.panicMemory {
		panic("memory boom")
	}
	return a.Fake.NodeMemory(ctx, node)
}

// hookTarget wraps targetfake to observe allocation and restores and to
// detect two jobs holding the same VMID at once.
type hookTarget struct {
	*targetfake.Fake
	mu        sync.Mutex
	allocated chan struct{} // receives after every AllocateVMID
	onRestore func(vmid int)
	held      map[int]bool
	maxHeld   int
	dupes     []int
}

func (t *hookTarget) AllocateVMID(ctx context.Context, inUse map[int]bool) (int, error) {
	v, err := t.Fake.AllocateVMID(ctx, inUse)
	if t.allocated != nil {
		t.allocated <- struct{}{}
	}
	return v, err
}

func (t *hookTarget) Restore(ctx context.Context, b model.Backup, vmid int, origTags string) error {
	t.mu.Lock()
	if t.held[vmid] {
		t.dupes = append(t.dupes, vmid)
	}
	t.held[vmid] = true
	t.maxHeld = max(t.maxHeld, len(t.held))
	t.mu.Unlock()
	if t.onRestore != nil {
		t.onRestore(vmid)
	}
	return t.Fake.Restore(ctx, b, vmid, origTags)
}

func (t *hookTarget) Destroy(ctx context.Context, kind model.GuestKind, vmid int, rec target.Recorded) error {
	t.mu.Lock()
	delete(t.held, vmid)
	t.mu.Unlock()
	return t.Fake.Destroy(ctx, kind, vmid, rec)
}

func (t *hookTarget) restores() []string {
	var out []string
	for _, c := range t.CallLog() {
		if strings.HasPrefix(c, "Restore ") {
			out = append(out, c)
		}
	}
	return out
}

type runFixture struct {
	d      Deps
	api    *hookAPI
	tgt    *hookTarget
	store  *state.Store
	notif  *fakeNotifier
	clock  *fakeClock
	logBuf *bytes.Buffer
}

// newRunFixture: VMID range [lo, hi], storages pbs and pbs-sync, restore
// storage local-zfs with plenty of room, 64 GiB free node memory.
func newRunFixture(t *testing.T, lo, hi int) *runFixture {
	t.Helper()
	store, err := state.Open(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	pf := pvefake.New()
	pf.Storage["local-zfs"] = pve.StorageStatus{Active: true, Enabled: true, Avail: 1 * tib, Total: 2 * tib}
	pf.Memory = pve.NodeMemory{Total: 128 * gib, Available: 64 * gib}

	tf := targetfake.New(lo, hi)
	tf.Boot = map[int]target.BootInfo{}
	tf.Exec = map[int]probe.Executor{}
	for v := lo; v <= hi; v++ {
		tf.Boot[v] = target.BootInfo{OS: probe.Linux}
		tf.Exec[v] = healthy()
	}

	var cfg config.Config
	cfg.Workers = 1
	cfg.Target.Node = "pve1"
	cfg.Target.VMIDRange = [2]int{lo, hi}
	cfg.Sources.Storages = []string{"pbs", "pbs-sync"}
	cfg.Restore.Storage = "local-zfs"
	cfg.Restore.SpaceMarginPercent = 10
	cfg.Restore.BootTimeout = 5 * time.Minute
	cfg.Notify.When = "always"
	cfg.Logging.RetainRuns = 30

	clock := &fakeClock{t: now}
	var logBuf bytes.Buffer
	f := &runFixture{
		api:    &hookAPI{Fake: pf},
		tgt:    &hookTarget{Fake: tf, held: map[int]bool{}},
		store:  store,
		notif:  &fakeNotifier{},
		clock:  clock,
		logBuf: &logBuf,
	}
	f.d = Deps{
		Cfg: cfg, API: f.api, Target: f.tgt, Store: store,
		Log:      slog.New(slog.NewTextHandler(&logBuf, nil)),
		Now:      clock.Now,
		Notifier: f.notif,
	}
	return f
}

// add seeds a backup of kind/vmid on storage with a 1 GiB size and an
// original config holding memory (MiB; 0 = no memory key).
func (f *runFixture) add(storage string, kind model.GuestKind, vmid, memMiB int, notesText string) model.Backup {
	b := model.Backup{
		Storage: storage, Notes: notesText, Size: gib,
		VolID: fmt.Sprintf("%s:backup/%s/%d/2026-10-18T22:00:00Z", storage, kind, vmid),
		Key:   model.GuestKey{Kind: kind, VMID: vmid},
		Time:  time.Date(2026, 10, 18, 22, 0, 0, 0, time.UTC),
	}
	f.api.Backups[storage] = append(f.api.Backups[storage], b)
	cfg := fmt.Sprintf("name: g%d\n", vmid)
	if memMiB > 0 {
		cfg += fmt.Sprintf("memory: %d\n", memMiB)
	}
	f.api.BackupConfigs[b.VolID] = cfg
	return b
}

func (f *runFixture) run(ctx context.Context, opt Options) model.RunSummary {
	return Run(ctx, f.d, opt)
}

// byVMID indexes results by original VMID.
func byVMID(t *testing.T, s model.RunSummary) map[int]model.GuestResult {
	t.Helper()
	m := map[int]model.GuestResult{}
	for _, r := range s.Results {
		if _, dup := m[r.Backup.Key.VMID]; dup {
			t.Errorf("duplicate result for %d", r.Backup.Key.VMID)
		}
		m[r.Backup.Key.VMID] = r
	}
	return m
}

func wantResult(t *testing.T, r model.GuestResult, v model.Verdict, reason string) {
	t.Helper()
	if r.Verdict != v || (reason != "" && r.Reason != reason) {
		t.Errorf("guest %s: got %s %q, want %s %q", r.Backup.Key, r.Verdict, r.Reason, v, reason)
	}
}

func (f *runFixture) lastRun(t *testing.T) state.RunRecord {
	t.Helper()
	runs := f.store.Snapshot().Runs
	if len(runs) == 0 {
		t.Fatal("no run recorded")
	}
	return runs[len(runs)-1]
}

func TestRunPreflightNotIsolated(t *testing.T) {
	f := newRunFixture(t, 990000, 990009)
	f.add("pbs", model.KindQEMU, 101, 1024, "")
	f.tgt.PrepareErr = fmt.Errorf("%w: vmbr9 has ports eno1", target.ErrNotIsolated)

	s := f.run(context.Background(), Options{Trigger: "schedule"})

	if len(f.tgt.restores()) != 0 {
		t.Errorf("restored despite failed preflight: %q", f.tgt.restores())
	}
	if !strings.Contains(s.RunError, "bridge is not isolated") {
		t.Errorf("RunError = %q", s.RunError)
	}
	if len(s.Results) != 0 {
		t.Errorf("results = %+v", s.Results)
	}
	sent := f.notif.sends()
	if len(sent) != 1 || sent[0].Severity() != "error" {
		t.Fatalf("sent = %+v", sent)
	}
	if r := f.lastRun(t); r.Outcome != state.OutcomeError {
		t.Errorf("outcome = %q", r.Outcome)
	}
}

func TestRunPreflightTemplatesFailure(t *testing.T) {
	f := newRunFixture(t, 990000, 990009)
	f.add("pbs", model.KindQEMU, 101, 1024, "")
	f.notif.ensureErr = errors.New("permission denied")

	s := f.run(context.Background(), Options{Trigger: "schedule"})
	if !strings.Contains(s.RunError, "permission denied") || len(f.tgt.restores()) != 0 {
		t.Errorf("RunError = %q restores = %q", s.RunError, f.tgt.restores())
	}
	if len(f.notif.sends()) != 1 {
		t.Error("preflight failure not notified")
	}
}

func TestRunDiscoveryAndAlreadyProven(t *testing.T) {
	f := newRunFixture(t, 990000, 990009)
	old := f.add("pbs-sync", model.KindQEMU, 101, 1024, "")
	old.VolID = "pbs-sync:old"
	f.api.Backups["pbs-sync"][0].VolID = old.VolID
	f.api.Backups["pbs-sync"][0].Time = old.Time.Add(-24 * time.Hour)
	newest := f.add("pbs", model.KindQEMU, 101, 1024, "")
	f.add("pbs-sync", model.KindLXC, 102, 512, "")
	provenNotes := "PBP PROVEN 2026-10-18 02:14 · agent ok\nweb"
	f.add("pbs", model.KindQEMU, 103, 1024, provenNotes)

	s := f.run(context.Background(), Options{Trigger: "manual"})

	if s.RunError != "" {
		t.Fatalf("RunError = %q", s.RunError)
	}
	if len(s.Results) != 3 {
		t.Fatalf("results = %d", len(s.Results))
	}
	r := byVMID(t, s)
	wantResult(t, r[101], model.Proven, "")
	wantResult(t, r[102], model.Proven, "")
	wantResult(t, r[103], model.Skipped, "already proven")
	if r[101].Backup.VolID != newest.VolID {
		t.Errorf("101 proofed %s, want newest %s", r[101].Backup.VolID, newest.VolID)
	}
	if got := f.tgt.restores(); len(got) != 2 {
		t.Errorf("restores = %q", got)
	}
	if got := f.api.Backups["pbs"][1].Notes; got != provenNotes {
		t.Errorf("already-proven notes rewritten: %q", got)
	}
	if got := s.Counts(); got[model.Proven] != 2 || got[model.Skipped] != 1 {
		t.Errorf("counts = %v", got)
	}
	if !r[103].AlreadyProven || r[101].AlreadyProven {
		t.Errorf("AlreadyProven: 103=%v 101=%v", r[103].AlreadyProven, r[101].AlreadyProven)
	}
	if sev := s.Severity(); sev != "info" {
		t.Errorf("severity = %s, want info (already proven is not a problem)", sev)
	}
}

// A backup whose VMID lies in PBP's own range is a backup of a temporary
// guest: never proofed, never listed as a result, logged once per run.
func TestRunSkipsBackupsInsideRange(t *testing.T) {
	f := newRunFixture(t, 990000, 990009)
	f.add("pbs", model.KindQEMU, 101, 1024, "")
	f.add("pbs", model.KindQEMU, 990003, 1024, "")
	f.add("pbs-sync", model.KindQEMU, 990003, 1024, "")
	f.add("pbs", model.KindLXC, 990009, 512, "")

	s := f.run(context.Background(), Options{Trigger: "manual"})

	if len(s.Results) != 1 || s.Results[0].Backup.Key.VMID != 101 {
		t.Fatalf("results = %+v", s.Results)
	}
	if got := f.tgt.restores(); len(got) != 1 {
		t.Errorf("restores = %q", got)
	}
	logs := f.logBuf.String()
	if n := strings.Count(logs, "skipping backups of guests in the PBP VMID range"); n != 1 {
		t.Errorf("logged %d times:\n%s", n, logs)
	}
	if !strings.Contains(logs, "qemu/990003") || !strings.Contains(logs, "lxc/990009") {
		t.Errorf("log does not name the guests:\n%s", logs)
	}
}

func TestRunSpaceGate(t *testing.T) {
	f := newRunFixture(t, 990000, 990009)
	f.api.Storage["local-zfs"] = pve.StorageStatus{Active: true, Enabled: true, Avail: 100 * gib, Total: 1 * tib}
	b := f.add("pbs", model.KindQEMU, 101, 1024, "db01")
	f.api.Backups["pbs"][0].Size = 10 * gib

	s := f.run(context.Background(), Options{Trigger: "manual"})

	wantResult(t, byVMID(t, s)[101], model.Skipped, "insufficient space on local-zfs")
	if len(f.tgt.restores()) != 0 {
		t.Errorf("restored: %q", f.tgt.restores())
	}
	if got := f.api.Backups["pbs"][0].Notes; got != b.Notes {
		t.Errorf("notes written for SKIPPED: %q", got)
	}
}

func TestRunSpaceFitsExactly(t *testing.T) {
	f := newRunFixture(t, 990000, 990009)
	// 100 GiB free, margin 10% of 500 GiB = 50 GiB → exactly 50 GiB fits.
	f.api.Storage["local-zfs"] = pve.StorageStatus{Active: true, Enabled: true, Avail: 100 * gib, Total: 500 * gib}
	f.add("pbs", model.KindQEMU, 101, 1024, "")
	f.api.Backups["pbs"][0].Size = 50 * gib

	s := f.run(context.Background(), Options{Trigger: "manual"})
	wantResult(t, byVMID(t, s)[101], model.Proven, "")
}

// holdFirstRestore makes the first restore wait until n signals arrive on
// ch, so later jobs are admitted while the first is still in flight.
func holdFirstRestore(t *testing.T, f *runFixture, ch <-chan struct{}, n int) {
	t.Helper()
	var once sync.Once
	f.tgt.onRestore = func(int) {
		once.Do(func() {
			for range n {
				select {
				case <-ch:
				case <-time.After(5 * time.Second):
					t.Error("second job never admitted while the first was in flight")
					return
				}
			}
		})
	}
}

func TestRunNoFreeVMIDWithTwoWorkers(t *testing.T) {
	f := newRunFixture(t, 990000, 990000)
	f.d.Cfg.Workers = 2
	f.add("pbs", model.KindQEMU, 101, 1024, "")
	f.add("pbs", model.KindQEMU, 102, 1024, "")
	f.tgt.allocated = make(chan struct{}, 4)
	holdFirstRestore(t, f, f.tgt.allocated, 2)

	s := f.run(context.Background(), Options{Trigger: "manual"})

	r := byVMID(t, s)
	wantResult(t, r[101], model.Proven, "")
	wantResult(t, r[102], model.Skipped, "no free VMID")
	for _, c := range f.tgt.restores() {
		if c != "Restore 990000 "+r[101].Backup.VolID {
			t.Errorf("restore outside range: %s", c)
		}
	}
	if r[101].TempVMID != 990000 {
		t.Errorf("temp vmid = %d", r[101].TempVMID)
	}
}

func TestRunNeverSharesVMID(t *testing.T) {
	f := newRunFixture(t, 990000, 990003)
	f.d.Cfg.Workers = 4
	for v := 101; v <= 116; v++ {
		f.add("pbs", model.KindQEMU, v, 512, "")
	}
	f.tgt.onRestore = func(int) { time.Sleep(2 * time.Millisecond) }

	s := f.run(context.Background(), Options{Trigger: "manual"})

	if len(f.tgt.dupes) != 0 {
		t.Errorf("VMIDs given to two jobs at once: %v", f.tgt.dupes)
	}
	if f.tgt.maxHeld > 4 {
		t.Errorf("max concurrent guests = %d", f.tgt.maxHeld)
	}
	for _, r := range s.Results {
		if r.Verdict != model.Proven {
			t.Errorf("%s: %s %q", r.Backup.Key, r.Verdict, r.Reason)
		}
		if r.TempVMID < 990000 || r.TempVMID > 990003 {
			t.Errorf("temp vmid %d outside range", r.TempVMID)
		}
	}
	if len(s.Results) != 16 {
		t.Errorf("results = %d", len(s.Results))
	}
}

func TestRunDeadline(t *testing.T) {
	f := newRunFixture(t, 990000, 990009)
	f.d.Cfg.Restore.MaxRunDuration = time.Hour
	f.add("pbs", model.KindQEMU, 101, 1024, "")
	f.add("pbs", model.KindQEMU, 102, 1024, "")
	f.add("pbs", model.KindQEMU, 103, 1024, "")
	var once sync.Once
	f.tgt.onRestore = func(int) { once.Do(func() { f.clock.advance(61 * time.Minute) }) }

	s := f.run(context.Background(), Options{Trigger: "schedule"})

	r := byVMID(t, s)
	wantResult(t, r[101], model.Proven, "")
	wantResult(t, r[102], model.Skipped, "run deadline")
	wantResult(t, r[103], model.Skipped, "run deadline")
	if len(f.tgt.restores()) != 1 {
		t.Errorf("restores = %q", f.tgt.restores())
	}
}

func TestRunNotifyWhenProblems(t *testing.T) {
	f := newRunFixture(t, 990000, 990009)
	f.d.Cfg.Notify.When = "problems"
	f.add("pbs", model.KindQEMU, 101, 1024, "")

	s := f.run(context.Background(), Options{Trigger: "schedule"})
	wantResult(t, byVMID(t, s)[101], model.Proven, "")
	if n := len(f.notif.sends()); n != 0 {
		t.Errorf("sent %d notifications for an all-PROVEN run", n)
	}
	if r := f.lastRun(t); r.Outcome != state.OutcomeOK {
		t.Errorf("outcome = %q", r.Outcome)
	}

	// A problem is notified under "problems".
	f2 := newRunFixture(t, 990000, 990009)
	f2.d.Cfg.Notify.When = "problems"
	f2.add("pbs", model.KindQEMU, 101, 1024, "")
	f2.api.Storage["local-zfs"] = pve.StorageStatus{Avail: 0, Total: tib}
	f2.run(context.Background(), Options{Trigger: "schedule"})
	if sent := f2.notif.sends(); len(sent) != 1 || sent[0].Severity() != "warning" {
		t.Errorf("sent = %+v", sent)
	}
	if r := f2.lastRun(t); r.Outcome != state.OutcomeProblems {
		t.Errorf("outcome = %q", r.Outcome)
	}
}

func TestRunNotifyAlways(t *testing.T) {
	f := newRunFixture(t, 990000, 990009)
	f.add("pbs", model.KindQEMU, 101, 1024, "")
	s := f.run(context.Background(), Options{Trigger: "schedule"})
	sent := f.notif.sends()
	if len(sent) != 1 || sent[0].Severity() != "info" || len(sent[0].Results) != 1 {
		t.Fatalf("sent = %+v", sent)
	}
	if !sent[0].Ended.Equal(s.Ended) || s.Ended.IsZero() {
		t.Errorf("ended = %v / %v", sent[0].Ended, s.Ended)
	}
}

func TestRunSendFailureMakesOutcomeError(t *testing.T) {
	f := newRunFixture(t, 990000, 990009)
	f.add("pbs", model.KindQEMU, 101, 1024, "")
	f.notif.sendErr = errors.New("ssh: handshake failed")

	s := f.run(context.Background(), Options{Trigger: "schedule"})

	wantResult(t, byVMID(t, s)[101], model.Proven, "")
	if r := f.lastRun(t); r.Outcome != state.OutcomeError || r.Counts[model.Proven] != 1 {
		t.Errorf("record = %+v", r)
	}
	if !strings.Contains(f.logBuf.String(), "ssh: handshake failed") {
		t.Errorf("send failure not logged: %s", f.logBuf)
	}
}

func TestRunOnlyVMID(t *testing.T) {
	f := newRunFixture(t, 990000, 990009)
	f.add("pbs", model.KindQEMU, 101, 1024, "")
	f.add("pbs", model.KindQEMU, 102, 1024, "")
	f.add("pbs-sync", model.KindLXC, 103, 512, "")

	s := f.run(context.Background(), Options{Trigger: "manual", OnlyVMID: 101})

	if len(s.Results) != 1 || s.Results[0].Backup.Key.VMID != 101 {
		t.Fatalf("results = %+v", s.Results)
	}
	if got := f.tgt.restores(); len(got) != 1 {
		t.Errorf("restores = %q", got)
	}
}

func TestRunRAMGate(t *testing.T) {
	f := newRunFixture(t, 990000, 990009)
	// 600 MiB free: an lxc without a memory key (512 MiB + 10% = 563.2)
	// fits; a qemu without one (2048 MiB) and a qemu at 560 MiB (616) do not.
	f.api.Memory = pve.NodeMemory{Total: 8 * gib, Available: 600 << 20}
	f.add("pbs", model.KindLXC, 101, 0, "")
	f.add("pbs", model.KindQEMU, 102, 0, "")
	f.add("pbs", model.KindQEMU, 103, 560, "")

	s := f.run(context.Background(), Options{Trigger: "manual"})

	r := byVMID(t, s)
	wantResult(t, r[101], model.Proven, "")
	wantResult(t, r[102], model.Skipped, "insufficient node memory")
	wantResult(t, r[103], model.Skipped, "insufficient node memory")
	if n := strings.Count(strings.Join(f.api.CallLog(), "\n"), "NodeMemory pve1"); n != 3 {
		t.Errorf("NodeMemory fetched %d times, want once per job", n)
	}
}

func TestRunRAMReservedByInFlightJob(t *testing.T) {
	f := newRunFixture(t, 990000, 990009)
	f.d.Cfg.Workers = 2
	// 4 GiB free; each guest needs 2048 MiB + 10%: one fits, two do not.
	f.api.Memory = pve.NodeMemory{Total: 8 * gib, Available: 4 * gib}
	f.add("pbs", model.KindQEMU, 101, 2048, "")
	f.add("pbs", model.KindQEMU, 102, 2048, "")
	memChecks := make(chan struct{}, 4)
	f.d.API = &countingAPI{hookAPI: f.api, memCh: memChecks}
	holdFirstRestore(t, f, memChecks, 2)

	s := f.run(context.Background(), Options{Trigger: "manual"})

	r := byVMID(t, s)
	wantResult(t, r[101], model.Proven, "")
	wantResult(t, r[102], model.Skipped, "insufficient node memory")
}

func TestRunSpaceReservedByInFlightJob(t *testing.T) {
	f := newRunFixture(t, 990000, 990009)
	f.d.Cfg.Workers = 2
	// 100 GiB free minus a 20 GiB margin (10% of 200 GiB) leaves 80 GiB;
	// each backup is 50 GiB: one fits, two do not.
	f.api.Storage["local-zfs"] = pve.StorageStatus{Avail: 100 * gib, Total: 200 * gib}
	f.add("pbs", model.KindQEMU, 101, 1024, "")
	f.add("pbs", model.KindQEMU, 102, 1024, "")
	f.api.Backups["pbs"][0].Size = 50 * gib
	f.api.Backups["pbs"][1].Size = 50 * gib
	ch := make(chan struct{}, 4)
	f.d.API = &countingAPI{hookAPI: f.api, storageCh: ch}
	holdFirstRestore(t, f, ch, 2)

	s := f.run(context.Background(), Options{Trigger: "manual"})

	r := byVMID(t, s)
	wantResult(t, r[101], model.Proven, "")
	wantResult(t, r[102], model.Skipped, "insufficient space on local-zfs")
}

// A finished job releases its reservation: with one worker, two backups
// that each fit only alone are proofed one after the other.
func TestRunReservationReleased(t *testing.T) {
	t.Run("space", func(t *testing.T) {
		f := newRunFixture(t, 990000, 990009)
		// 80 GiB usable (100 free minus a 20 GiB margin); 50 GiB each.
		f.api.Storage["local-zfs"] = pve.StorageStatus{Avail: 100 * gib, Total: 200 * gib}
		f.add("pbs", model.KindQEMU, 101, 1024, "")
		f.add("pbs", model.KindQEMU, 102, 1024, "")
		f.api.Backups["pbs"][0].Size = 50 * gib
		f.api.Backups["pbs"][1].Size = 50 * gib
		s := f.run(context.Background(), Options{Trigger: "manual"})
		r := byVMID(t, s)
		wantResult(t, r[101], model.Proven, "")
		wantResult(t, r[102], model.Proven, "")
	})
	t.Run("memory", func(t *testing.T) {
		f := newRunFixture(t, 990000, 990009)
		// 4 GiB free; each needs 2048 MiB + 10%.
		f.api.Memory = pve.NodeMemory{Total: 8 * gib, Available: 4 * gib}
		f.add("pbs", model.KindQEMU, 101, 2048, "")
		f.add("pbs", model.KindQEMU, 102, 2048, "")
		s := f.run(context.Background(), Options{Trigger: "manual"})
		r := byVMID(t, s)
		wantResult(t, r[101], model.Proven, "")
		wantResult(t, r[102], model.Proven, "")
	})
}

// countingAPI signals after each NodeMemory / StorageStatus call.
type countingAPI struct {
	*hookAPI
	memCh, storageCh chan struct{}
}

func (a *countingAPI) NodeMemory(ctx context.Context, node string) (pve.NodeMemory, error) {
	m, err := a.hookAPI.NodeMemory(ctx, node)
	if a.memCh != nil {
		a.memCh <- struct{}{}
	}
	return m, err
}

func (a *countingAPI) StorageStatus(ctx context.Context, node, storage string) (pve.StorageStatus, error) {
	s, err := a.hookAPI.StorageStatus(ctx, node, storage)
	if a.storageCh != nil {
		a.storageCh <- struct{}{}
	}
	return s, err
}

func TestRunOriginalConfigFailure(t *testing.T) {
	f := newRunFixture(t, 990000, 990009)
	b := f.add("pbs", model.KindQEMU, 101, 1024, "")
	delete(f.api.BackupConfigs, b.VolID)

	s := f.run(context.Background(), Options{Trigger: "manual"})

	r := byVMID(t, s)[101]
	if r.Verdict != model.Error || !strings.HasPrefix(r.Reason, "cannot read backup config: ") {
		t.Errorf("got %s %q", r.Verdict, r.Reason)
	}
	if len(f.tgt.restores()) != 0 {
		t.Errorf("restored: %q", f.tgt.restores())
	}
	// The failure is this backup's own: its notes say so.
	if got := f.api.Backups["pbs"][0].Notes; !strings.HasPrefix(got, "PBP ERROR 2026-10-19 02:14 · cannot read backup config: ") {
		t.Errorf("notes = %q", got)
	}
}

// Node-wide capacity errors (storage status, node memory, VMID allocation)
// say nothing about the backup: they stay out of its notes.
func TestRunNodeCapacityErrorsStayOutOfNotes(t *testing.T) {
	for _, method := range []string{"StorageStatus", "NodeMemory"} {
		t.Run(method, func(t *testing.T) {
			f := newRunFixture(t, 990000, 990009)
			f.add("pbs", model.KindQEMU, 101, 1024, "db01")
			f.api.Fail[method] = errors.New("api down")
			s := f.run(context.Background(), Options{Trigger: "manual"})
			if r := byVMID(t, s)[101]; r.Verdict != model.Error {
				t.Errorf("got %s %q", r.Verdict, r.Reason)
			}
			if got := f.api.Backups["pbs"][0].Notes; got != "db01" {
				t.Errorf("notes = %q", got)
			}
		})
	}
	t.Run("AllocateVMID", func(t *testing.T) {
		f := newRunFixture(t, 990000, 990009)
		f.add("pbs", model.KindQEMU, 101, 1024, "db01")
		f.d.Target = allocFail{f.tgt}
		s := f.run(context.Background(), Options{Trigger: "manual"})
		if r := byVMID(t, s)[101]; r.Verdict != model.Error {
			t.Errorf("got %s %q", r.Verdict, r.Reason)
		}
		if got := f.api.Backups["pbs"][0].Notes; got != "db01" {
			t.Errorf("notes = %q", got)
		}
	})
}

type allocFail struct{ target.Target }

func (allocFail) AllocateVMID(context.Context, map[int]bool) (int, error) {
	return 0, errors.New("api down")
}

func TestRunStorageDiscoveryErrors(t *testing.T) {
	f := newRunFixture(t, 990000, 990009)
	f.add("pbs", model.KindQEMU, 101, 1024, "")
	f.add("pbs-sync", model.KindQEMU, 102, 1024, "")
	f.api.listErr = map[string]error{"pbs-sync": errors.New("datastore offline")}

	s := f.run(context.Background(), Options{Trigger: "schedule"})

	if s.RunError != "storage pbs-sync: datastore offline" {
		t.Errorf("RunError = %q", s.RunError)
	}
	if len(s.Results) != 1 || s.Results[0].Verdict != model.Proven {
		t.Errorf("results = %+v", s.Results)
	}
	if r := f.lastRun(t); r.Outcome != state.OutcomeError {
		t.Errorf("outcome = %q", r.Outcome)
	}

	f2 := newRunFixture(t, 990000, 990009)
	f2.add("pbs", model.KindQEMU, 101, 1024, "")
	f2.api.listErr = map[string]error{"pbs": errors.New("a"), "pbs-sync": errors.New("b")}
	s2 := f2.run(context.Background(), Options{Trigger: "schedule"})
	if s2.RunError != "storage pbs: a; storage pbs-sync: b" || len(s2.Results) != 0 {
		t.Errorf("RunError = %q results = %+v", s2.RunError, s2.Results)
	}
	if len(f2.tgt.restores()) != 0 || len(f2.notif.sends()) != 1 {
		t.Errorf("restores = %q sends = %d", f2.tgt.restores(), len(f2.notif.sends()))
	}
}

func TestRunCancelled(t *testing.T) {
	f := newRunFixture(t, 990000, 990009)
	f.add("pbs", model.KindQEMU, 101, 1024, "")
	f.add("pbs", model.KindQEMU, 102, 1024, "")
	f.add("pbs", model.KindQEMU, 103, 1024, "")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.tgt.onRestore = func(int) { cancel() }

	s := f.run(ctx, Options{Trigger: "schedule"})

	r := byVMID(t, s)
	wantResult(t, r[101], model.Error, "cancelled")
	wantResult(t, r[102], model.Skipped, "run cancelled")
	wantResult(t, r[103], model.Skipped, "run cancelled")
	if len(f.tgt.Live) != 0 {
		t.Errorf("guest left behind: %v", f.tgt.Live)
	}
	if len(f.notif.sends()) != 1 {
		t.Error("cancelled run not notified")
	}
	if _, ok := f.store.Snapshot().InFlight[990000]; ok {
		t.Error("InFlight entry left")
	}
}

func TestRunRecord(t *testing.T) {
	f := newRunFixture(t, 990000, 990009)
	f.add("pbs", model.KindQEMU, 101, 1024, "")
	var during state.RunRecord
	f.tgt.onRestore = func(int) {
		runs := f.store.Snapshot().Runs
		during = runs[len(runs)-1]
		f.clock.advance(3 * time.Minute)
	}

	s := f.run(context.Background(), Options{Trigger: "schedule"})

	if during.Outcome != state.OutcomeRunning || during.ID != "20261019T021400.000Z-schedule" {
		t.Errorf("record during run = %+v", during)
	}
	rec := f.lastRun(t)
	if rec.ID != during.ID || rec.Outcome != state.OutcomeOK || rec.Trigger != "schedule" ||
		!rec.Started.Equal(now) || !rec.Ended.Equal(now.Add(3*time.Minute)) || rec.Counts[model.Proven] != 1 {
		t.Errorf("record = %+v", rec)
	}
	if len(f.store.Snapshot().Runs) != 1 {
		t.Errorf("runs = %+v", f.store.Snapshot().Runs)
	}
	if s.ID != rec.ID || s.Trigger != "schedule" || !s.Started.Equal(now) || !s.Ended.Equal(rec.Ended) {
		t.Errorf("summary = %+v", s)
	}
}

func TestRunRetainRuns(t *testing.T) {
	f := newRunFixture(t, 990000, 990009)
	f.d.Cfg.Logging.RetainRuns = 2
	for range 3 {
		f.run(context.Background(), Options{Trigger: "schedule"})
		f.clock.advance(time.Hour)
	}
	if runs := f.store.Snapshot().Runs; len(runs) != 2 || runs[1].ID != "20261019T041400.000Z-schedule" {
		t.Errorf("runs = %+v", runs)
	}
}

func TestRunGatingPanicBecomesRunError(t *testing.T) {
	f := newRunFixture(t, 990000, 990009)
	f.add("pbs", model.KindQEMU, 101, 1024, "")
	f.api.panicMemory = true

	s := f.run(context.Background(), Options{Trigger: "schedule"})

	if !strings.Contains(s.RunError, "memory boom") {
		t.Errorf("RunError = %q", s.RunError)
	}
	r := byVMID(t, s)[101]
	if r.Verdict != model.Error {
		t.Errorf("got %s %q", r.Verdict, r.Reason)
	}
	if len(f.notif.sends()) != 1 {
		t.Error("not notified")
	}
}

// cancelAPI cancels the run while reading a backup's config and fails.
type cancelAPI struct {
	*hookAPI
	cancel context.CancelFunc
}

func (a *cancelAPI) OriginalConfig(ctx context.Context, node, volid string) (string, error) {
	a.cancel()
	return "", context.Canceled
}

func TestRunAdmissionErrorWhileCancelledIsSkipped(t *testing.T) {
	f := newRunFixture(t, 990000, 990009)
	f.add("pbs", model.KindQEMU, 101, 1024, "")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.d.API = &cancelAPI{hookAPI: f.api, cancel: cancel}

	s := f.run(ctx, Options{Trigger: "schedule"})

	wantResult(t, byVMID(t, s)[101], model.Skipped, "run cancelled")
	if got := f.api.Backups["pbs"][0].Notes; got != "" {
		t.Errorf("notes written for a cancelled admission: %q", got)
	}
}

func TestRunUsesGivenRunID(t *testing.T) {
	f := newRunFixture(t, 990000, 990009)
	s := f.run(context.Background(), Options{Trigger: "manual", RunID: "fixed-id"})
	if s.ID != "fixed-id" || f.lastRun(t).ID != "fixed-id" {
		t.Errorf("summary ID = %q, record = %+v", s.ID, f.lastRun(t))
	}
}

// panicOnSkip panics when the dispatcher logs a skipped guest.
type panicOnSkip struct{ slog.Handler }

func (h panicOnSkip) Handle(ctx context.Context, r slog.Record) error {
	if r.Message == "guest skipped" {
		panic("dispatcher boom")
	}
	return h.Handler.Handle(ctx, r)
}

func (h panicOnSkip) WithAttrs(as []slog.Attr) slog.Handler {
	return panicOnSkip{h.Handler.WithAttrs(as)}
}

func (h panicOnSkip) WithGroup(n string) slog.Handler { return panicOnSkip{h.Handler.WithGroup(n)} }

func TestRunDispatcherPanicWaitsForJobs(t *testing.T) {
	f := newRunFixture(t, 990000, 990009)
	f.add("pbs", model.KindQEMU, 101, 1024, "")
	f.add("pbs", model.KindQEMU, 102, 1024, "PBP PROVEN 2026-10-18 02:14 · agent ok")
	f.d.Log = slog.New(panicOnSkip{slog.NewTextHandler(f.logBuf, nil)})
	entered, release := make(chan struct{}), make(chan struct{})
	f.tgt.onRestore = func(int) {
		close(entered)
		<-release
	}

	done := make(chan model.RunSummary)
	go func() { done <- f.run(context.Background(), Options{Trigger: "schedule"}) }()
	<-entered
	select {
	case <-done:
		close(release)
		t.Fatal("Run returned while a job was still running")
	case <-time.After(200 * time.Millisecond):
	}
	close(release)
	s := <-done
	if !strings.Contains(s.RunError, "dispatcher boom") {
		t.Errorf("RunError = %q", s.RunError)
	}
	if len(f.tgt.Live) != 0 {
		t.Errorf("guest left behind: %v", f.tgt.Live)
	}
}

// Two runs started in the same second get distinct IDs (and log files).
func TestNewRunIDHasMilliseconds(t *testing.T) {
	t0 := time.Date(2026, 10, 19, 2, 14, 0, 0, time.FixedZone("CEST", 2*3600))
	a, b := NewRunID(t0, "manual"), NewRunID(t0.Add(7*time.Millisecond), "manual")
	if a != "20261019T001400.000Z-manual" || b != "20261019T001400.007Z-manual" {
		t.Errorf("ids = %q, %q", a, b)
	}
	if strings.ContainsAny(a, "/\\: ") {
		t.Errorf("id %q is not filename-safe", a)
	}
}
