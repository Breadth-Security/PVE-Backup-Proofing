package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/config"
	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/daemon"
	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/model"
	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/node"
	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/nodeproto"
	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/pve/pvefake"
	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/sshx"
	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/sshx/helperfake"
	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/state"
	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/target"
	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/target/targetfake"
)

var fixedNow = time.Date(2026, 10, 19, 2, 0, 5, 0, time.UTC)

type fakeNotifier struct {
	mu   sync.Mutex
	sent []model.RunSummary
}

func (n *fakeNotifier) CheckTemplates(context.Context) error { return nil }

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

func (l lockProbe) Prepare(ctx context.Context) error {
	release, err := daemon.AcquireLock(daemon.LockPath())
	*l.held = errors.Is(err, daemon.ErrBusy)
	if err == nil {
		release()
	}
	return l.Target.Prepare(ctx)
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
	// The record carries the run correlation and precedes "run finished".
	var rec struct{ Run, Trigger string }
	for _, line := range strings.Split(string(b), "\n") {
		if strings.Contains(line, `"msg":"guest result"`) {
			if err := json.Unmarshal([]byte(line), &rec); err != nil {
				t.Fatal(err)
			}
		}
	}
	if rec.Run != id || rec.Trigger != "manual" {
		t.Errorf("guest record run/trigger = %+v", rec)
	}
	if strings.Index(string(b), `"msg":"guest result"`) > strings.Index(string(b), `"msg":"run finished"`) {
		t.Errorf("guest result logged after run finished:\n%s", b)
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
	if len(runs) != 1 || runs[0].Outcome != state.OutcomeError {
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

// stepTarget is a fake target that reports its node helper checks step by
// step, as target.ExistingNode does.
type stepTarget struct {
	*targetfake.Fake
	steps []target.NodeStep
}

func (s stepTarget) CheckNodeSteps(context.Context) []target.NodeStep { return s.steps }

func TestCheckPrintsNodeHelperSteps(t *testing.T) {
	e := newEnv(t)
	st := stepTarget{Fake: e.tgt, steps: []target.NodeStep{
		{Name: "node helper version", Detail: "0.2.0"},
		{Name: "node helper config", Detail: "range [990000, 990049], bridge vmbr-pbp"},
		{Name: "node helper request_timeout", Detail: "15m0s"},
		{Name: "node helper refusal", Detail: "exec in 990050 refused"},
	}}
	e.a.connect = func(config.Config) wiring {
		return wiring{api: e.api, target: st, notifier: e.notif, close: func() {}}
	}
	if code := e.run("check"); code != exitOK {
		t.Fatalf("exit %d out %q", code, e.out)
	}
	for _, want := range []string{"node helper version: ok (0.2.0)\n", "node helper config: ok (range [990000, 990049], bridge vmbr-pbp)\n",
		"node helper request_timeout: ok (15m0s)\n", "node helper refusal: ok (exec in 990050 refused)\n"} {
		if !strings.Contains(e.out.String(), want) {
			t.Errorf("missing %q in %q", want, e.out)
		}
	}

	st.steps = []target.NodeStep{{Name: "node helper version", Detail: "0.2.0"},
		{Name: "node helper config", Err: errors.New("bridge differs (see docs/setup.md)")}}
	e.a.connect = func(config.Config) wiring {
		return wiring{api: e.api, target: st, notifier: e.notif, close: func() {}}
	}
	code := e.run("check")
	if code != exitFail || !strings.Contains(e.out.String(), `node helper config: FAIL: bridge differs (see docs/setup.md)`) ||
		strings.Contains(e.out.String(), "preflight") {
		t.Errorf("exit %d out %q", code, e.out)
	}
}

// assemble wires the real target with the daemon's own version, and check
// walks the real helper checks.
func TestCheckRealNodeHelperUsesDaemonVersion(t *testing.T) {
	helperVer := func(v string) *helperfake.Fake {
		return &helperfake.Fake{Handler: func(req nodeproto.Request, _ []byte) (sshx.Result, error) {
			switch req.Op {
			case "version":
				return sshx.Result{Stdout: v + "\n"}, nil
			case "config":
				return helperfake.JSON(nodeproto.NodeConfig{VMIDRange: [2]int{990000, 990049}, Bridge: "vmbr-pbp",
					RequestTimeout: 15 * time.Minute, Version: v}), nil
			case "exec":
				return helperfake.Refuse(req, "out-of-range", "outside the range"), nil
			}
			return sshx.Result{ExitCode: 127}, nil
		}}
	}
	e := newEnv(t)
	e.a.connect = func(cfg config.Config) wiring { return assemble(cfg, e.api, nil, helperVer(version), nil) }
	e.run("check")
	for _, want := range []string{"node helper version: ok (" + version + ")", "node helper config: ok", "node helper refusal: ok"} {
		if !strings.Contains(e.out.String(), want) {
			t.Errorf("missing %q in %q", want, e.out)
		}
	}

	e.a.connect = func(cfg config.Config) wiring { return assemble(cfg, e.api, nil, helperVer("9.9.9"), nil) }
	code := e.run("check")
	if code != exitFail || !strings.Contains(e.out.String(), "node helper version: FAIL") ||
		!strings.Contains(e.out.String(), "9.9.9") || !strings.Contains(e.out.String(), "docs/setup.md") ||
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

// The overrun limit is max_run_duration + boot_timeout + probe_timeout + 10m
// (1h + 5m + 5m + 10m = 1h20m here), not max_run_duration alone.
func TestHealthOverrunLimitWiring(t *testing.T) {
	e := newEnv(t)
	cfg, err := os.ReadFile(e.cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	cfg = bytes.Replace(cfg, []byte("restore: {storage: local-zfs}"), []byte("restore: {storage: local-zfs, max_run_duration: 1h}"), 1)
	if err := os.WriteFile(e.cfgPath, cfg, 0o600); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 19, 2, 30, 0, 0, time.Local)
	e.a.now = func() time.Time { return now }
	store, err := state.Open(statePath())
	if err != nil {
		t.Fatal(err)
	}
	started := now.Add(-70 * time.Minute) // past 1h, inside 1h20m
	rec := state.RunRecord{ID: "r1", Trigger: "schedule", Outcome: state.OutcomeRunning, Started: started}
	if err := store.AppendRun(rec, 5); err != nil {
		t.Fatal(err)
	}
	if code := e.run("health"); code != exitOK {
		t.Errorf("inside limit: %d %q", code, e.out)
	}
	e.a.now = func() time.Time { return now.Add(11 * time.Minute) } // 81m
	if code := e.run("health"); code != exitFail || e.out.String() != "run overran\n" {
		t.Errorf("past limit: %d %q", code, e.out)
	}
}

func TestReadStartedUnreadable(t *testing.T) {
	dir := t.TempDir() // a directory where the file should be cannot be read
	if _, err := readStarted(dir); err == nil || !strings.Contains(err.Error(), dir) {
		t.Errorf("want error naming %s, got %v", dir, err)
	}
	if got, err := readStarted(filepath.Join(dir, "missing")); err != nil || !got.IsZero() {
		t.Errorf("missing file: %v, %v", got, err)
	}
}

func TestWriteStartedIsAtomic(t *testing.T) {
	p := filepath.Join(t.TempDir(), "sub", "daemon.started")
	if err := writeStarted(p, fixedNow); err != nil {
		t.Fatal(err)
	}
	got, err := readStarted(p)
	if err != nil || !got.Equal(fixedNow) {
		t.Errorf("read back %v, %v", got, err)
	}
	if es, _ := os.ReadDir(filepath.Dir(p)); len(es) != 1 {
		t.Errorf("temp file left behind: %v", es)
	}
}

// A failed rename (the target is a non-empty directory) returns the error and
// removes the temp file.
func TestWriteStartedFailureRemovesTemp(t *testing.T) {
	p := filepath.Join(t.TempDir(), "daemon.started")
	if err := os.MkdirAll(filepath.Join(p, "occupied"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := writeStarted(p, fixedNow); err == nil {
		t.Fatal("want rename error")
	}
	if es, _ := os.ReadDir(filepath.Dir(p)); len(es) != 1 {
		t.Errorf("temp file left behind: %v", es)
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

type captureAudit struct{ got []auditEntry }

func (c *captureAudit) Log(e auditEntry) { c.got = append(c.got, e) }

// nodeEnv wires `pbp node` to a temp helper config, a capturing audit log
// and a non-root euid.
func nodeEnv(t *testing.T) (*env, *captureAudit) {
	t.Helper()
	e := newEnv(t)
	cfgPath := filepath.Join(t.TempDir(), "node.yaml")
	if err := os.WriteFile(cfgPath, []byte("vmid_range: [990000, 990049]\nbridge: vmbr-pbp\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PBP_NODE_CONFIG", cfgPath)
	t.Setenv("SSH_CONNECTION", "192.0.2.7 50000 192.0.2.1 22")
	au := &captureAudit{}
	e.a.audit = au
	e.a.stdin = strings.NewReader("")
	e.a.euid = func() int { return 1000 }
	e.a.templateDir = filepath.Join(t.TempDir(), "tmpl")
	return e, au
}

func nodeRequest(t *testing.T, op string, timeout time.Duration) (string, string) {
	t.Helper()
	nonce := nodeproto.NewNonce()
	s, err := nodeproto.Encode(nodeproto.Request{Op: op, Timeout: timeout, Nonce: nonce})
	if err != nil {
		t.Fatal(err)
	}
	return s, nonce
}

func TestNodeNoRequest(t *testing.T) {
	for _, set := range []bool{false, true} {
		e, au := nodeEnv(t)
		if set {
			t.Setenv("SSH_ORIGINAL_COMMAND", "")
		} else {
			unsetenv(t, "SSH_ORIGINAL_COMMAND")
		}
		if code := e.a.main(context.Background(), []string{"node"}); code != 250 {
			t.Errorf("set=%v: exit %d", set, code)
		}
		want := "pbp-node: refused: no request (pbp node runs only as an SSH forced command)\n"
		if e.errOut.String() != want || e.out.Len() != 0 {
			t.Errorf("set=%v: stderr %q stdout %q", set, e.errOut, e.out)
		}
		if len(au.got) != 0 {
			t.Errorf("set=%v: audited a non-request: %v", set, au.got)
		}
	}
}

// unsetenv removes key for the test and restores it afterwards.
func unsetenv(t *testing.T, key string) {
	t.Helper()
	t.Setenv(key, "")
	_ = os.Unsetenv(key)
}

// stubSys is a node.System whose failures echo secrets, as subprocess stderr
// can.
type stubSys struct {
	secret    string
	ctConfErr bool
}

func (s stubSys) CTConfig(context.Context, int) (map[string]string, error) {
	if s.ctConfErr {
		return nil, errors.New("open /etc/pve/lxc/990001.conf: " + s.secret)
	}
	return map[string]string{"tags": "pbp"}, nil
}
func (s stubSys) PctExec(context.Context, int, []string, io.Reader, io.Writer, io.Writer) (int, error) {
	return 0, errors.New("pct failed running " + s.secret)
}
func (s stubSys) BridgeState(context.Context, string) (nodeproto.BridgeState, error) {
	return nodeproto.BridgeState{}, errors.New("ovs: " + s.secret)
}
func (s stubSys) Notify(context.Context, []byte) error { return errors.New("ERROR: " + s.secret) }
func (s stubSys) TemplateHashes() (map[string]string, error) {
	return nil, errors.New("hash " + s.secret)
}

func TestNodeErrorReasonsNotAudited(t *testing.T) {
	const secret = "hunter2-secret"
	cases := map[string]struct {
		req   nodeproto.Request
		stdin string
	}{
		"notify": {nodeproto.Request{Op: "notify"}, `{"severity":"info"}`},
		"pct":    {nodeproto.Request{Op: "exec", VMID: 990001, Argv: []string{"id"}}, ""},
		"bridge": {nodeproto.Request{Op: "bridge"}, ""},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			e, au := nodeEnv(t)
			e.a.nodeSys = stubSys{secret: secret}
			e.a.stdin = strings.NewReader(c.stdin)
			c.req.Timeout, c.req.Nonce = time.Minute, nodeproto.NewNonce()
			enc, err := nodeproto.Encode(c.req)
			if err != nil {
				t.Fatal(err)
			}
			t.Setenv("SSH_ORIGINAL_COMMAND", enc)
			if code := e.a.main(context.Background(), []string{"node"}); code != 250 {
				t.Fatalf("exit %d, stderr %q", code, e.errOut)
			}
			if len(au.got) != 2 || au.got[0].Outcome != "received" || au.got[1].Outcome != "error" {
				t.Fatalf("audit %+v", au.got)
			}
			for _, g := range au.got {
				if strings.Contains(formatAudit(g), secret) {
					t.Errorf("audit leaks subprocess text: %+v", g)
				}
			}
		})
	}
}

func TestNodeRefusalAuditCodesOmitPayloadText(t *testing.T) {
	const secret = "hunter2-secret"
	cases := map[string]struct {
		req   nodeproto.Request
		stdin string
		sys   stubSys
		code  string
	}{
		"severity":      {nodeproto.Request{Op: "notify"}, `{"severity":"` + secret + `"}`, stubSys{}, "bad-severity"},
		"unknown field": {nodeproto.Request{Op: "notify"}, `{"severity":"info","` + secret + `":1}`, stubSys{}, "bad-payload"},
		"not container": {nodeproto.Request{Op: "exec", VMID: 990001, Argv: []string{"id"}}, "", stubSys{secret: secret, ctConfErr: true}, "not-container"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			e, au := nodeEnv(t)
			e.a.nodeSys = c.sys
			e.a.stdin = strings.NewReader(c.stdin)
			c.req.Timeout, c.req.Nonce = time.Minute, nodeproto.NewNonce()
			enc, err := nodeproto.Encode(c.req)
			if err != nil {
				t.Fatal(err)
			}
			t.Setenv("SSH_ORIGINAL_COMMAND", enc)
			if code := e.a.main(context.Background(), []string{"node"}); code != 250 {
				t.Fatalf("exit %d", code)
			}
			if len(au.got) != 2 || au.got[1].Outcome != "refused" || au.got[1].Reason != c.code {
				t.Fatalf("audit %+v, want reason %q", au.got, c.code)
			}
			for _, g := range au.got {
				if strings.Contains(formatAudit(g), secret) {
					t.Errorf("audit leaks text: %+v", g)
				}
			}
		})
	}
}

func TestNodeExit250WithoutMarkerIsError(t *testing.T) {
	e, au := nodeEnv(t)
	e.a.nodeSys = execSys{stubSys{}, 250}
	req := nodeproto.Request{Op: "exec", VMID: 990001, Argv: []string{"x"},
		Timeout: time.Minute, Nonce: nodeproto.NewNonce()}
	enc, _ := nodeproto.Encode(req)
	t.Setenv("SSH_ORIGINAL_COMMAND", enc)
	if code := e.a.main(context.Background(), []string{"node"}); code != 250 {
		t.Fatalf("exit %d", code)
	}
	if len(au.got) != 2 || au.got[1].Outcome != "error" || au.got[1].Reason == "" {
		t.Errorf("audit %+v", au.got)
	}
}

type execSys struct {
	stubSys
	exit int
}

func (s execSys) PctExec(context.Context, int, []string, io.Reader, io.Writer, io.Writer) (int, error) {
	return s.exit, nil
}

func TestNodeConfigOverrideIgnoredAsRoot(t *testing.T) {
	for _, root := range []bool{false, true} {
		e, au := nodeEnv(t)
		if root {
			e.a.euid = func() int { return 0 }
		}
		req, _ := nodeRequest(t, "version", time.Minute)
		t.Setenv("SSH_ORIGINAL_COMMAND", req)
		code := e.a.main(context.Background(), []string{"node"})
		if root {
			// /etc/pbp/node.yaml is used; the temp override must not be.
			if _, err := os.Stat(node.DefaultConfigPath); err == nil {
				t.Skip("host has a real /etc/pbp/node.yaml")
			}
			if code != 250 || len(au.got) != 1 || au.got[0].Outcome != "refused" {
				t.Errorf("root: exit %d audit %+v", code, au.got)
			}
		} else if code != 0 {
			t.Errorf("non-root: exit %d", code)
		}
	}
}

func TestNodeUnparseableRequestMissingConfig(t *testing.T) {
	e, au := nodeEnv(t)
	t.Setenv("PBP_NODE_CONFIG", filepath.Join(t.TempDir(), "absent.yaml"))
	t.Setenv("SSH_ORIGINAL_COMMAND", "rm -rf /")
	if code := e.a.main(context.Background(), []string{"node"}); code != 250 {
		t.Fatalf("exit %d", code)
	}
	if !strings.HasPrefix(e.errOut.String(), "pbp-node: refused: ") || strings.Count(e.errOut.String(), "\n") != 1 {
		t.Errorf("stderr %q", e.errOut)
	}
	if len(au.got) != 1 || au.got[0].Outcome != "refused" {
		t.Errorf("audit %+v", au.got)
	}
}

func TestNodeVersionRequest(t *testing.T) {
	old := version
	t.Cleanup(func() { version = old })
	version = "v0.2.1"
	e, au := nodeEnv(t)
	req, _ := nodeRequest(t, "version", time.Minute)
	t.Setenv("SSH_ORIGINAL_COMMAND", req)
	if code := e.a.main(context.Background(), []string{"node"}); code != 0 {
		t.Fatalf("exit %d, stderr %q", code, e.errOut)
	}
	if e.out.String() != "v0.2.1\n" || e.errOut.Len() != 0 {
		t.Errorf("stdout %q stderr %q", e.out, e.errOut)
	}
	want := []auditEntry{{Outcome: "received", Op: "version", Client: "192.0.2.7"},
		{Outcome: "ok", Op: "version", Client: "192.0.2.7"}}
	if !slices.Equal(au.got, want) {
		t.Errorf("audit %+v, want %+v", au.got, want)
	}
}

func TestNodeRefusalsAreAudited(t *testing.T) {
	e, au := nodeEnv(t)
	secret := "hunter2-secret"
	t.Setenv("SSH_ORIGINAL_COMMAND", "exec 5 -- "+secret)
	if code := e.a.main(context.Background(), []string{"node"}); code != 250 {
		t.Fatalf("exit %d", code)
	}
	if len(au.got) != 1 || au.got[0].Outcome != "refused" || au.got[0].Client != "192.0.2.7" {
		t.Fatalf("audit %+v", au.got)
	}
	if strings.Contains(formatAudit(au.got[0]), secret) {
		t.Errorf("audit leaks request text: %+v", au.got[0])
	}

	// A valid request the helper itself refuses: out-of-range VMID.
	au.got = nil
	nonce := nodeproto.NewNonce()
	req, err := nodeproto.Encode(nodeproto.Request{Op: "exec", VMID: 101, Argv: []string{"echo", secret},
		Timeout: time.Minute, Nonce: nonce})
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("SSH_ORIGINAL_COMMAND", req)
	e.errOut.Reset()
	if code := e.a.main(context.Background(), []string{"node"}); code != 250 {
		t.Fatalf("exit %d", code)
	}
	kind, _, ok := nodeproto.HelperFailure(250, e.errOut.String(), nonce)
	if !ok || kind != "refused" {
		t.Errorf("stderr %q", e.errOut)
	}
	if len(au.got) != 2 || au.got[0].Outcome != "received" || au.got[0].VMID != 101 {
		t.Fatalf("audit %+v", au.got)
	}
	if g := au.got[1]; g.Outcome != "refused" || g.Op != "exec" || g.VMID != 101 || g.Reason == "" {
		t.Fatalf("audit %+v", au.got)
	}
	for _, g := range au.got {
		if strings.Contains(formatAudit(g), secret) {
			t.Errorf("audit leaks argv: %+v", g)
		}
	}
}

func TestNodeMissingConfigRefuses(t *testing.T) {
	e, au := nodeEnv(t)
	t.Setenv("PBP_NODE_CONFIG", filepath.Join(t.TempDir(), "absent.yaml"))
	req, nonce := nodeRequest(t, "version", time.Minute)
	t.Setenv("SSH_ORIGINAL_COMMAND", req)
	if code := e.a.main(context.Background(), []string{"node"}); code != 250 {
		t.Fatalf("exit %d", code)
	}
	if kind, _, ok := nodeproto.HelperFailure(250, e.errOut.String(), nonce); !ok || kind != "refused" {
		t.Errorf("stderr %q", e.errOut)
	}
	if len(au.got) != 1 || au.got[0].Outcome != "refused" {
		t.Errorf("audit %+v", au.got)
	}
}

func TestNodeInstallTemplates(t *testing.T) {
	t.Run("refused over ssh", func(t *testing.T) {
		e, _ := nodeEnv(t)
		e.a.euid = func() int { return 0 }
		t.Setenv("SSH_ORIGINAL_COMMAND", "version")
		if code := e.a.main(context.Background(), []string{"node", "install-templates"}); code != exitUsage {
			t.Errorf("exit %d", code)
		}
		if _, err := os.Stat(e.a.templateDir); err == nil {
			t.Error("wrote templates despite SSH_ORIGINAL_COMMAND")
		}
		if !strings.Contains(e.errOut.String(), "usage:") {
			t.Errorf("stderr %q", e.errOut)
		}
	})
	t.Run("refused over ssh with empty command as root", func(t *testing.T) {
		e, _ := nodeEnv(t)
		e.a.euid = func() int { return 0 }
		t.Setenv("SSH_ORIGINAL_COMMAND", "")
		if code := e.a.main(context.Background(), []string{"node", "install-templates"}); code != exitUsage {
			t.Errorf("exit %d", code)
		}
		if _, err := os.Stat(e.a.templateDir); err == nil {
			t.Error("wrote templates")
		}
	})
	t.Run("extra argument refused", func(t *testing.T) {
		e, _ := nodeEnv(t)
		e.a.euid = func() int { return 0 }
		if code := e.a.main(context.Background(), []string{"node", "install-templates", "extra"}); code != exitUsage {
			t.Errorf("exit %d", code)
		}
		if _, err := os.Stat(e.a.templateDir); err == nil {
			t.Error("wrote templates")
		}
	})
	t.Run("refused as non-root", func(t *testing.T) {
		e, _ := nodeEnv(t)
		if code := e.a.main(context.Background(), []string{"node", "install-templates"}); code != exitUsage {
			t.Errorf("exit %d", code)
		}
		if _, err := os.Stat(e.a.templateDir); err == nil {
			t.Error("wrote templates as non-root")
		}
	})
	t.Run("root installs and lists paths", func(t *testing.T) {
		e, _ := nodeEnv(t)
		e.a.euid = func() int { return 0 }
		if code := e.a.main(context.Background(), []string{"node", "install-templates"}); code != exitOK {
			t.Fatalf("exit %d, stderr %q", code, e.errOut)
		}
		entries, err := os.ReadDir(e.a.templateDir)
		if err != nil || len(entries) == 0 {
			t.Fatalf("no templates written: %v", err)
		}
		for _, en := range entries {
			if !strings.Contains(e.out.String(), filepath.Join(e.a.templateDir, en.Name())+"\n") {
				t.Errorf("stdout %q omits %s", e.out, en.Name())
			}
		}
	})
}

func TestNodeUsageAndExtraArgs(t *testing.T) {
	e, _ := nodeEnv(t)
	if !strings.Contains(usage, "pbp node") {
		t.Errorf("usage omits node: %q", usage)
	}
	if code := e.a.main(context.Background(), []string{"node", "bogus"}); code != exitUsage {
		t.Errorf("exit %d", code)
	}
}

// The check's "pve api" line makes a real request: a client that builds but
// cannot reach the API (wrong address, TLS pin, token) is a failure, and
// nothing after it runs.
func TestCheckAPIIsARealRequest(t *testing.T) {
	e := newEnv(t)
	e.api.Fail = map[string]error{"Version": errors.New("dial tcp 10.10.0.1:8006: connection refused")}
	code := e.run("check")
	if code != exitFail || !strings.Contains(e.out.String(), "pve api : FAIL: dial tcp 10.10.0.1:8006: connection refused") {
		t.Errorf("exit %d out %q", code, e.out)
	}
	if strings.Contains(e.out.String(), "preflight") {
		t.Errorf("checks ran past a failing API: %q", e.out)
	}
}

// The daemon exits 78 on a config it cannot use, which pbp.service does not
// restart on; other commands keep exit 1.
func TestDaemonConfigErrorExit(t *testing.T) {
	e := newEnv(t)
	bad := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(bad, []byte("target:\n  node: \"\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code := e.a.main(context.Background(), []string{"daemon", "--config", bad}); code != exitConfig {
		t.Errorf("daemon exit %d, want %d", code, exitConfig)
	}
	if code := e.a.main(context.Background(), []string{"check", "--config", bad}); code != exitFail {
		t.Errorf("check exit %d, want %d", code, exitFail)
	}
	unit, err := os.ReadFile("../../packaging/pbp.service")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(unit), "RestartPreventExitStatus="+strconv.Itoa(exitConfig)) {
		t.Errorf("pbp.service does not prevent restarts on exit %d", exitConfig)
	}
}
