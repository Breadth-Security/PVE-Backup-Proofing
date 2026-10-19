package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"git.breadthanalytics.com/tdiana/pbv/internal/config"
	"git.breadthanalytics.com/tdiana/pbv/internal/daemon"
	"git.breadthanalytics.com/tdiana/pbv/internal/model"
	"git.breadthanalytics.com/tdiana/pbv/internal/pve/pvefake"
	"git.breadthanalytics.com/tdiana/pbv/internal/state"
	"git.breadthanalytics.com/tdiana/pbv/internal/target"
	"git.breadthanalytics.com/tdiana/pbv/internal/target/targetfake"
)

var fixedNow = time.Date(2026, 10, 19, 2, 0, 5, 0, time.UTC)

type fakeNotifier struct {
	mu   sync.Mutex
	sent []model.RunSummary
}

func (n *fakeNotifier) EnsureTemplates(context.Context) error { return nil }

func (n *fakeNotifier) Send(_ context.Context, s model.RunSummary) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.sent = append(n.sent, s)
	return nil
}

// lockProbe is a target whose Prepare records whether the run lock was held.
type lockProbe struct {
	target.Target
	held *bool
}

func (l lockProbe) Prepare(ctx context.Context, recorded map[int]target.Recorded) error {
	release, err := daemon.AcquireLock(daemon.LockPath())
	*l.held = errors.Is(err, daemon.ErrBusy)
	if err == nil {
		release()
	}
	return l.Target.Prepare(ctx, recorded)
}

type env struct {
	t       *testing.T
	a       *app
	out     *bytes.Buffer
	errOut  *bytes.Buffer
	cfgPath string
	logDir  string
	api     *pvefake.Fake
	tgt     *targetfake.Fake
	notif   *fakeNotifier
}

// newEnv writes a valid config into a temp dir, points PBP_STATE_DIR at a
// temp dir and wires the app to fakes.
func newEnv(t *testing.T) *env {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("PBP_STATE_DIR", filepath.Join(dir, "state"))
	e := &env{t: t, out: &bytes.Buffer{}, errOut: &bytes.Buffer{}, logDir: filepath.Join(dir, "log"),
		api: pvefake.New(), tgt: targetfake.New(990000, 990049), notif: &fakeNotifier{}}
	e.cfgPath = filepath.Join(dir, "config.yaml")
	cfg := fmt.Sprintf(`
schedule: "0 2 * * *"
target:
  node: pve1
  bridge: vmbr-pbp
  vmid_range: [990000, 990049]
  api: {tls_fingerprint: "AB"}
  ssh: {known_hosts_file: /kh}
sources: {storages: [pbs]}
restore: {storage: local-zfs}
logging: {dir: %s, retain_runs: 2}
`, e.logDir)
	if err := os.WriteFile(e.cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	e.a = &app{stdout: e.out, stderr: e.errOut, now: func() time.Time { return fixedNow },
		connect: func(config.Config) wiring {
			return wiring{api: e.api, target: e.tgt, notifier: e.notif, close: func() {}}
		}}
	return e
}

func (e *env) run(args ...string) int {
	e.out.Reset()
	e.errOut.Reset()
	return e.a.main(context.Background(), append(args, "--config", e.cfgPath))
}

func (e *env) backup(vmid int, notes string) {
	b := model.Backup{Storage: "pbs", Notes: notes, Size: 1 << 30,
		VolID: fmt.Sprintf("pbs:backup/vm/%d/2026-10-18T22:00:00Z", vmid),
		Key:   model.GuestKey{Kind: model.KindQEMU, VMID: vmid},
		Time:  time.Date(2026, 10, 18, 22, 0, 0, 0, time.UTC)}
	e.api.Backups["pbs"] = append(e.api.Backups["pbs"], b)
}

func (e *env) runs() []state.RunRecord {
	st, err := readState(statePath())
	if err != nil {
		e.t.Fatal(err)
	}
	return st.Runs
}

func TestUsage(t *testing.T) {
	e := newEnv(t)
	if code := e.a.main(context.Background(), nil); code != exitUsage {
		t.Errorf("no args: %d", code)
	}
	if code := e.run("frobnicate"); code != exitUsage || !strings.Contains(e.errOut.String(), "unknown subcommand") {
		t.Errorf("unknown: %d %q", code, e.errOut)
	}
	if code := e.run("health", "--guest", "5"); code != exitUsage {
		t.Errorf("--guest on health: %d", code)
	}
}

func TestGuestFlagMustBePositive(t *testing.T) {
	for _, g := range []string{"0", "-5", "abc", ""} {
		e := newEnv(t)
		if code := e.run("run", "--guest", g); code != exitUsage {
			t.Errorf("--guest %q: exit %d, want %d", g, code, exitUsage)
		}
	}
	e := newEnv(t)
	if code := e.run("run", "--guest", "105"); code != exitFail || !strings.Contains(e.out.String(), "no backup found for guest 105") {
		t.Errorf("--guest 105: %d %q", code, e.out)
	}
}

func TestVersion(t *testing.T) {
	old := version
	t.Cleanup(func() { version = old })
	version = "v1.2.3"
	e := newEnv(t)
	// No config is needed, and unknown flags still fail.
	if code := e.a.main(context.Background(), []string{"version"}); code != exitOK ||
		e.out.String() != "pbp v1.2.3\n"+runtime.Version()+"\n" {
		t.Errorf("version: %d %q", code, e.out)
	}
	if code := e.a.main(context.Background(), []string{"version", "extra"}); code != exitUsage {
		t.Errorf("version extra: %d", code)
	}
	if !strings.Contains(usage, "pbp version") {
		t.Errorf("usage omits version: %q", usage)
	}
}

func TestBusyLock(t *testing.T) {
	for _, cmd := range []string{"run", "check"} {
		t.Run(cmd, func(t *testing.T) {
			e := newEnv(t)
			e.a.connect = func(config.Config) wiring { t.Fatal("connected while another run holds the lock"); return wiring{} }
			release, err := daemon.AcquireLock(daemon.LockPath())
			if err != nil {
				t.Fatal(err)
			}
			defer release()
			if code := e.run(cmd); code != exitBusy {
				t.Errorf("exit %d, want %d", code, exitBusy)
			}
			if strings.TrimSpace(e.errOut.String()) != "a run is already in progress" {
				t.Errorf("stderr = %q", e.errOut)
			}
		})
	}
}

func TestRunAndCheckHoldLock(t *testing.T) {
	for _, cmd := range []string{"run", "check"} {
		t.Run(cmd, func(t *testing.T) {
			e := newEnv(t)
			held := false
			e.a.connect = func(config.Config) wiring {
				return wiring{api: e.api, target: lockProbe{e.tgt, &held}, notifier: e.notif, close: func() {}}
			}
			e.run(cmd)
			if !held {
				t.Error("run lock not held during preflight")
			}
			if _, err := daemon.AcquireLock(daemon.LockPath()); err != nil {
				t.Errorf("lock not released: %v", err)
			}
		})
	}
}

func TestRunWiring(t *testing.T) {
	e := newEnv(t)
	e.backup(101, "PBP PROVEN 2026-10-18 02:14 · agent ok")
	e.backup(102, "")
	if err := os.MkdirAll(e.logDir, 0o755); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-48 * time.Hour)
	for _, n := range []string{"run-a.log", "run-b.log", "run-c.log"} {
		p := filepath.Join(e.logDir, n)
		if err := os.WriteFile(p, nil, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, old, old); err != nil {
			t.Fatal(err)
		}
		old = old.Add(time.Hour)
	}

	if code := e.run("run", "--guest", "101"); code != exitOK {
		t.Fatalf("exit %d; out %q err %q", code, e.out, e.errOut)
	}

	const id = "20261019T020005.000Z-manual"
	runs := e.runs()
	if len(runs) != 1 || runs[0].ID != id || runs[0].Trigger != "manual" {
		t.Fatalf("runs = %+v", runs)
	}
	if !strings.Contains(e.out.String(), "run "+id+": info; proven 0, failed 0, booted 0, skipped 1, error 0") {
		t.Errorf("stdout = %q", e.out)
	}
	b, err := os.ReadFile(filepath.Join(e.logDir, "run-"+id+".log"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"msg":"guest result"`) || !strings.Contains(string(b), `"verdict":"SKIPPED"`) ||
		strings.Contains(string(b), "qemu/102") {
		t.Errorf("run log = %s", b)
	}
	if n := strings.Count(string(b), `"msg":"guest result"`); n != 1 {
		t.Errorf("guest 101 logged %d result records, want exactly 1:\n%s", n, b)
	}
	ents, _ := os.ReadDir(e.logDir)
	var names []string
	for _, en := range ents {
		names = append(names, en.Name())
	}
	if !slices.Equal(names, []string{"run-" + id + ".log", "run-c.log"}) {
		t.Errorf("logs after prune = %v", names)
	}
	if len(e.notif.sent) != 1 {
		t.Errorf("notifications = %d", len(e.notif.sent))
	}
}

func TestRunUnknownGuest(t *testing.T) {
	e := newEnv(t)
	if code := e.run("run", "--guest", "999"); code != exitFail || !strings.Contains(e.out.String(), "no backup found for guest 999") {
		t.Errorf("exit %d out %q", code, e.out)
	}
}

func TestRunSSHDownIsRunErrorAndUnhealthy(t *testing.T) {
	e := newEnv(t)
	e.backup(102, "")
	var cfgSeen config.Config
	e.a.connect = func(cfg config.Config) wiring {
		cfgSeen = cfg
		return assemble(cfg, e.api, nil, nil, errors.New("connection refused"))
	}

	if code := e.run("run"); code != exitFail {
		t.Errorf("exit %d", code)
	}
	runs := e.runs()
	if len(runs) != 1 || runs[0].Outcome != "error" {
		t.Fatalf("runs = %+v", runs)
	}
	if !strings.Contains(e.out.String(), "connection refused") || !strings.Contains(e.out.String(), "SSH TO THE NODE FAILED") {
		t.Errorf("stdout = %q", e.out)
	}
	if slices.Contains(e.api.CallLog(), "Guests") || len(e.api.CallLog()) != 0 {
		t.Errorf("touched the node with SSH down: %v", e.api.CallLog())
	}
	if cfgSeen.Target.Node != "pve1" {
		t.Error("connect not given the config")
	}

	if code := e.run("health"); code != exitFail || strings.TrimSpace(e.out.String()) != "last run error" {
		t.Errorf("health: %d %q", code, e.out)
	}
}

func TestCheck(t *testing.T) {
	e := newEnv(t)
	if code := e.run("check"); code != exitOK {
		t.Errorf("exit %d out %q", code, e.out)
	}
	for _, want := range []string{"config " + e.cfgPath + ": ok", "ssh : ok", "preflight (", "): ok"} {
		if !strings.Contains(e.out.String(), want) {
			t.Errorf("missing %q in %q", want, e.out)
		}
	}
	if len(e.runs()) != 0 {
		t.Error("check recorded a run")
	}

	e.tgt.PrepareErr = errors.New("bridge not isolated")
	if code := e.run("check"); code != exitFail || !strings.Contains(e.out.String(), "FAIL: target: bridge not isolated") {
		t.Errorf("exit %d out %q", code, e.out)
	}

	e.a.connect = func(cfg config.Config) wiring { return assemble(cfg, e.api, nil, nil, errors.New("refused")) }
	if code := e.run("check"); code != exitFail || !strings.Contains(e.out.String(), "ssh : FAIL: ssh : refused") ||
		strings.Contains(e.out.String(), "preflight") {
		t.Errorf("exit %d out %q", code, e.out)
	}
}

func TestHealth(t *testing.T) {
	e := newEnv(t)
	if code := e.run("health"); code != exitOK || e.out.String() != "ok\n" {
		t.Errorf("no state: %d %q", code, e.out)
	}

	// Daemon started yesterday noon and never ran: the 02:00 run (container
	// time zone) was missed.
	if err := os.MkdirAll(daemon.StateDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 19, 3, 0, 0, 0, time.Local)
	if err := os.WriteFile(startedPath(), []byte(now.Add(-15*time.Hour).Format(time.RFC3339)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	e.a.now = func() time.Time { return now }
	if code := e.run("health"); code != exitFail || e.out.String() != "scheduled run missed\n" {
		t.Errorf("missed: %d %q", code, e.out)
	}

	// Freshly started daemon.
	if err := os.WriteFile(startedPath(), []byte(now.Add(-10*time.Minute).Format(time.RFC3339)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code := e.run("health"); code != exitOK {
		t.Errorf("fresh: %d %q", code, e.out)
	}

	if err := os.WriteFile(e.cfgPath, []byte("schedule: [\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code := e.run("health"); code != exitFail || e.out.Len() == 0 {
		t.Errorf("bad config: %d %q", code, e.out)
	}
}

// A corrupt daemon.started is unhealthy and names the file, not zero time.
func TestHealthCorruptStartTime(t *testing.T) {
	e := newEnv(t)
	if err := os.MkdirAll(daemon.StateDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(startedPath(), []byte("yesterday-ish\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code := e.run("health"); code != exitFail || !strings.Contains(e.out.String(), startedPath()) {
		t.Errorf("corrupt: %d %q", code, e.out)
	}
}

func TestDaemonWritesStartTime(t *testing.T) {
	e := newEnv(t)
	oldLog := slog.Default()
	t.Cleanup(func() { slog.SetDefault(oldLog) })
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if code := e.a.main(ctx, []string{"daemon", "--config", e.cfgPath}); code != exitOK {
		t.Fatalf("exit %d err %q", code, e.errOut)
	}
	b, err := os.ReadFile(startedPath())
	if err != nil || strings.TrimSpace(string(b)) != fixedNow.Format(time.RFC3339) {
		t.Errorf("daemon.started = %q, %v", b, err)
	}
	if !strings.Contains(e.out.String(), "version="+version) {
		t.Errorf("daemon start does not log the version: %q", e.out)
	}
}
