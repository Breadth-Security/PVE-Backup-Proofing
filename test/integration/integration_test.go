//go:build integration

// Package integration runs PBP against a real Proxmox VE node. It is opt-in:
// build tag "integration" and PBP_IT_CONFIG pointing at a PBP config for the
// node. See README.md in this directory for the node setup and every
// environment variable.
package integration

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"git.breadthanalytics.com/tdiana/pbv/internal/config"
	"git.breadthanalytics.com/tdiana/pbv/internal/discovery"
	"git.breadthanalytics.com/tdiana/pbv/internal/model"
	"git.breadthanalytics.com/tdiana/pbv/internal/notes"
	"git.breadthanalytics.com/tdiana/pbv/internal/pve"
	"git.breadthanalytics.com/tdiana/pbv/internal/report"
	"git.breadthanalytics.com/tdiana/pbv/internal/run"
	"git.breadthanalytics.com/tdiana/pbv/internal/sshx"
	"git.breadthanalytics.com/tdiana/pbv/internal/state"
	"git.breadthanalytics.com/tdiana/pbv/internal/target"
)

// pbpNotesLine is the PBP verdict line every proofed backup must start with.
var pbpNotesLine = regexp.MustCompile(`^PBP (PROVEN|FAILED|BOOTED|SKIPPED|ERROR) \d{4}-\d{2}-\d{2} \d{2}:\d{2} · .+$`)

// env holds what every test needs: the loaded config and live connections.
type env struct {
	cfgPath string
	cfg     config.Config
	api     *pve.Client
	ssh     *sshx.Client
	tgt     *target.ExistingNode
}

var (
	envOnce sync.Once
	theEnv  *env
	envErr  error
)

// setup loads PBP_IT_CONFIG and connects once per test binary. Without
// PBP_IT_CONFIG every test is skipped.
func setup(t *testing.T) *env {
	t.Helper()
	path := os.Getenv("PBP_IT_CONFIG")
	if path == "" {
		t.Skip("PBP_IT_CONFIG not set")
	}
	envOnce.Do(func() {
		cfg, err := config.Load(path)
		if err != nil {
			envErr = err
			return
		}
		api, err := pve.New(cfg.Target.API)
		if err != nil {
			envErr = err
			return
		}
		ssh, err := sshx.Dial(cfg.Target.SSH)
		if err != nil {
			envErr = err
			return
		}
		theEnv = &env{cfgPath: path, cfg: cfg, api: api, ssh: ssh, tgt: target.NewExistingNode(cfg, api, ssh)}
	})
	if envErr != nil {
		t.Fatal(envErr)
	}
	return theEnv
}

// envInt reads a required integer environment variable or skips the test.
func envInt(t *testing.T, name string) int {
	t.Helper()
	v := os.Getenv(name)
	if v == "" {
		t.Skipf("%s not set", name)
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		t.Fatalf("%s=%q: %v", name, v, err)
	}
	return n
}

func envStr(t *testing.T, name string) string {
	t.Helper()
	v := os.Getenv(name)
	if v == "" {
		t.Skipf("%s not set", name)
	}
	return v
}

// capture records summaries instead of sending them, so test runs do not
// notify the node's real targets. EnsureTemplates is real.
type capture struct {
	*report.PVENotifier
	mu   sync.Mutex
	sent []model.RunSummary
}

func (c *capture) Send(_ context.Context, s model.RunSummary) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sent = append(c.sent, s)
	return nil
}

// kindAPI hides backups of other guest kinds that share the subject's VMID.
// A reused VMID leaves such backups behind (on pve1, VMID 110 has backups of
// a long-gone VM and of the current container), and "run --guest" proofs
// every kind with that VMID; the tests proof exactly one backup.
type kindAPI struct {
	pve.API
	key model.GuestKey
}

func (a kindAPI) ListBackups(ctx context.Context, node, storage string) ([]model.Backup, error) {
	bs, err := a.API.ListBackups(ctx, node, storage)
	out := bs[:0]
	for _, b := range bs {
		if b.Key.VMID != a.key.VMID || b.Key.Kind == a.key.Kind {
			out = append(out, b)
		}
	}
	return out, err
}

// deps builds run dependencies with a state store in stateDir.
func (e *env) deps(t *testing.T, stateDir string, cfg config.Config) run.Deps {
	t.Helper()
	store, err := state.Open(filepath.Join(stateDir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(testWriter{t}, &slog.HandlerOptions{Level: slog.LevelInfo}))
	return run.Deps{
		Cfg: cfg, API: e.api, Target: e.tgt, Store: store, Log: log, Now: time.Now,
		Notifier: &capture{PVENotifier: &report.PVENotifier{SSH: e.ssh, Node: cfg.Target.Node}},
	}
}

type testWriter struct{ t *testing.T }

func (w testWriter) Write(p []byte) (int, error) {
	w.t.Log(strings.TrimRight(string(p), "\n"))
	return len(p), nil
}

// newest returns the backup PBP will pick for vmid (same rules as a run).
func (e *env) newest(t *testing.T, ctx context.Context, cfg config.Config, key model.GuestKey) model.Backup {
	t.Helper()
	var all []model.Backup
	for _, st := range cfg.Sources.Storages {
		bs, err := e.api.ListBackups(ctx, cfg.Target.Node, st)
		if err != nil {
			t.Fatalf("list %s: %v", st, err)
		}
		all = append(all, bs...)
	}
	for _, b := range discovery.NewestPerGuest(all) {
		if b.Key == key {
			return b
		}
	}
	t.Fatalf("no backup of guest %s on %v", key, cfg.Sources.Storages)
	return model.Backup{}
}

// volume re-reads one backup volume's protected flag and full notes (the
// listing has only the first line of the notes).
func (e *env) volume(t *testing.T, ctx context.Context, b model.Backup) model.Backup {
	t.Helper()
	bs, err := e.api.ListBackups(ctx, e.cfg.Target.Node, b.Storage)
	if err != nil {
		t.Fatal(err)
	}
	for _, x := range bs {
		if x.VolID == b.VolID {
			if x.Notes, err = e.api.VolumeNotes(ctx, e.cfg.Target.Node, b.Storage, b.VolID); err != nil {
				t.Fatal(err)
			}
			return x
		}
	}
	t.Fatalf("backup %s disappeared", b.VolID)
	return model.Backup{}
}

// assertRangeEmpty fails if any guest exists in the VMID range.
func (e *env) assertRangeEmpty(t *testing.T, ctx context.Context) {
	t.Helper()
	gs, err := e.api.Guests(ctx)
	if err != nil {
		t.Fatal(err)
	}
	lo, hi := e.cfg.Target.VMIDRange[0], e.cfg.Target.VMIDRange[1]
	for _, g := range gs {
		if g.Key.VMID >= lo && g.Key.VMID <= hi {
			t.Errorf("guest %s (%s) left in range [%d, %d]", g.Key, g.Name, lo, hi)
		}
	}
}

// userNotes is what PBP must preserve below its line: the notes without any
// PBP line.
func userNotes(n string) string {
	m := notes.Merge(n, "X")
	return strings.TrimPrefix(strings.TrimPrefix(m, "X"), "\n")
}

// proof runs PBP for one guest (reproof on, so reruns work) and checks the
// invariants every proofed backup must satisfy afterwards.
func (e *env) proof(t *testing.T, key model.GuestKey) model.GuestResult {
	t.Helper()
	ctx := context.Background()
	cfg := e.cfg
	cfg.Restore.Reproof = true
	before := e.volume(t, ctx, e.newest(t, ctx, cfg, key))
	t.Logf("proofing %s (protected=%v, notes=%q)", before.VolID, before.Protected, before.Notes)

	d := e.deps(t, t.TempDir(), cfg)
	d.API = kindAPI{API: e.api, key: key}
	s := run.Run(ctx, d, run.Options{Trigger: "manual", OnlyVMID: key.VMID})
	if s.RunError != "" {
		t.Fatalf("run error: %s", s.RunError)
	}
	if len(s.Results) != 1 {
		t.Fatalf("want 1 result, got %+v", s.Results)
	}
	r := s.Results[0]
	t.Logf("verdict %s: %s (restore %s, boot %s, failed %v, ignored %v)",
		r.Verdict, r.Reason, r.RestoreDuration, r.BootDuration, r.FailedUnits, r.IgnoredUnits)
	if c := d.Notifier.(*capture); len(c.sent) != 1 {
		t.Errorf("want 1 summary for the notifier, got %d", len(c.sent))
	}

	e.assertRangeEmpty(t, ctx)
	after := e.volume(t, ctx, before)
	if after.Protected != before.Protected {
		t.Errorf("protected = %v after the run, was %v", after.Protected, before.Protected)
	}
	lines := strings.Split(after.Notes, "\n")
	if !pbpNotesLine.MatchString(lines[0]) || !strings.HasPrefix(lines[0], "PBP "+string(r.Verdict)+" ") {
		t.Errorf("notes line 1 = %q, want a PBP %s line", lines[0], r.Verdict)
	}
	if got, want := strings.Join(lines[1:], "\n"), userNotes(before.Notes); got != want {
		t.Errorf("user notes below the PBP line = %q, want %q", got, want)
	}
	if st := d.Store.Snapshot(); len(st.InFlight) != 0 {
		t.Errorf("in-flight jobs left in state: %+v", st.InFlight)
	}
	return r
}

// TestCheck is "pbp check": preflight on the real node (version, isolated
// bridge, restore storage, VMID range, templates, janitor).
func TestCheck(t *testing.T) {
	e := setup(t)
	d := e.deps(t, t.TempDir(), e.cfg)
	if err := run.Preflight(context.Background(), d); err != nil {
		t.Fatal(err)
	}
	iface, err := e.api.NetIface(context.Background(), e.cfg.Target.Node, e.cfg.Target.Bridge)
	if err != nil {
		t.Fatal(err)
	}
	if iface.Type != "bridge" && iface.Type != "OVSBridge" {
		t.Errorf("bridge %s has type %q", e.cfg.Target.Bridge, iface.Type)
	}
}

// TestProofSmallLXC proofs a small (unprivileged) container's newest backup.
func TestProofSmallLXC(t *testing.T) {
	e := setup(t)
	r := e.proof(t, model.GuestKey{Kind: model.KindLXC, VMID: envInt(t, "PBP_IT_LXC")})
	switch r.Verdict {
	case model.Proven:
	case model.Failed:
		if len(r.FailedUnits) == 0 || !strings.HasPrefix(r.Reason, "failed: ") {
			t.Errorf("FAILED without naming units: %q", r.Reason)
		}
	default:
		t.Errorf("verdict %s: %s", r.Verdict, r.Reason)
	}
}

// TestProofVMWithAgent proofs a VM with agent: 1; the agent must be used.
func TestProofVMWithAgent(t *testing.T) {
	e := setup(t)
	r := e.proof(t, model.GuestKey{Kind: model.KindQEMU, VMID: envInt(t, "PBP_IT_VM_AGENT")})
	if r.AgentMissing || (r.Verdict != model.Proven && r.Verdict != model.Failed) {
		t.Errorf("verdict %s: %s (agent missing %v)", r.Verdict, r.Reason, r.AgentMissing)
	}
}

// TestProofVMWithoutAgent proofs a VM without a guest agent: BOOTED.
func TestProofVMWithoutAgent(t *testing.T) {
	e := setup(t)
	r := e.proof(t, model.GuestKey{Kind: model.KindQEMU, VMID: envInt(t, "PBP_IT_VM_NOAGENT")})
	if r.Verdict != model.Booted || r.Reason != "guest agent missing" {
		t.Errorf("verdict %s: %s", r.Verdict, r.Reason)
	}
}

// fullSummary exercises every template section, including the hyphenated
// run-error and not-covered keys inside #if and #each.
func fullSummary() model.RunSummary {
	now := time.Now()
	return model.RunSummary{
		ID: "it-" + now.UTC().Format("20060102T150405Z"), Trigger: "manual",
		Started: now.Add(-90 * time.Second), Ended: now,
		RunError: "integration test: this run error is synthetic",
		Results: []model.GuestResult{
			{Backup: model.Backup{Key: model.GuestKey{Kind: model.KindLXC, VMID: 990001}}, Name: "it-lxc",
				Verdict: model.Failed, Reason: "failed: pbp-it-broken.service",
				NotCovered: []model.NotCovered{{Unit: "srv.mount", Piece: model.MissingPiece{Kind: "excluded-mountpoint", Key: "mp0", Path: "/srv"}}}},
			{Backup: model.Backup{Key: model.GuestKey{Kind: model.KindQEMU, VMID: 990002}}, Name: "it-vm",
				Verdict: model.Booted, Reason: "guest agent missing", AgentMissing: true},
		},
	}
}

// renderScript renders a notification with the node's real proxmox-notify
// into a loopback webhook and prints what it received. The config exists
// only in memory; the built-in mail target and default matcher are disabled
// and the script refuses to send unless the capture webhook is the only
// enabled target, so nothing is delivered anywhere.
const renderScript = `set -e
OUT=$(mktemp)
trap 'rm -f "$OUT"' EXIT
timeout 30 python3 -c '
import http.server, sys
class H(http.server.BaseHTTPRequestHandler):
    def do_POST(self):
        n = int(self.headers.get("Content-Length", 0))
        open(sys.argv[1], "wb").write(self.rfile.read(n))
        self.send_response(200); self.end_headers()
    def log_message(self, *a): pass
http.server.HTTPServer(("127.0.0.1", int(sys.argv[2])), H).handle_request()' "$OUT" "$PORT" &
LP=$!
sleep 1
perl -e '
use strict; use JSON; use Proxmox::RS::Notify;
my ($port, $body) = @ARGV;
my $cfg = "sendmail: mail-to-root\n\tmailto-user root\@pam\n\tdisable true\n\n"
  . "matcher: default-matcher\n\tdisable true\n\ttarget mail-to-root\n\n"
  . "webhook: pbp-it-capture\n\tmethod post\n\turl http://127.0.0.1:$port/\n\tbody $body\n\n"
  . "matcher: pbp-it-capture-matcher\n\tmode all\n\ttarget pbp-it-capture\n";
my $c = Proxmox::RS::Notify->parse_config($cfg, "webhook: pbp-it-capture\n");
my @t = map { "$_->{name}:$_->{type}" } grep { !$_->{disable} } @{$c->get_targets()};
die "refusing to send: enabled targets @t\n" unless "@t" eq "pbp-it-capture:webhook";
my @m = map { $_->{name} } grep { !$_->{disable} } @{$c->get_matchers()};
die "refusing to send: enabled matchers @m\n" unless "@m" eq "pbp-it-capture-matcher";
my $in = decode_json(do { local $/; <STDIN> });
$c->send($in->{severity}, "pbp", $in->{data}, $in->{fields});' "$PORT" "$BODY"
wait $LP || true
cat "$OUT"
`

// TestNotificationRenders installs the templates and renders a full summary
// with the node's proxmox-notify (no delivery). This proves the new "pbp"
// template type is found in /etc/pve/notification-templates/default/ and
// that hyphenated variables work in #if and #each.
func TestNotificationRenders(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	n := &report.PVENotifier{SSH: e.ssh, Node: e.cfg.Target.Node}
	if err := n.EnsureTemplates(ctx); err != nil {
		t.Fatal(err)
	}
	s := fullSummary()
	in, err := json.Marshal(map[string]any{
		"severity": s.Severity(), "data": report.TemplateData(s, e.cfg.Target.Node),
		"fields": map[string]string{"type": "pbp", "hostname": e.cfg.Target.Node},
	})
	if err != nil {
		t.Fatal(err)
	}
	body := base64.StdEncoding.EncodeToString([]byte("{{ title }}\n=====\n{{ message }}"))
	cmd := "PORT=18089 BODY=" + sshx.Quote(body) + " bash -c " + sshx.Quote(renderScript)
	res, err := e.ssh.Run(ctx, cmd, in)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("stderr:\n%s\nrendered:\n%s", res.Stderr, res.Stdout)
	if res.ExitCode != 0 || strings.Contains(res.Stderr, "ERROR") {
		t.Fatalf("render failed: exit %d", res.ExitCode)
	}
	if !strings.HasPrefix(res.Stdout, "PBP ") {
		t.Errorf("subject does not come first")
	}
	for _, want := range []string{
		"PBP " + e.cfg.Target.Node + ": 0 proven, 1 failed, 1 booted, 0 skipped, 0 error",
		"PBP backup proofing run " + s.ID + " on " + e.cfg.Target.Node + "\n",
		"Duration: 1m30s\n",
		"Run error: integration test: this run error is synthetic\n",
		"  lxc/990001 it-lxc: FAILED - failed: pbp-it-broken.service\n",
		"Guests without a working guest agent (booted, but not fully verified):\n  qemu/990002 it-vm: guest agent missing\n",
		"Not covered by backup:\n  Found srv.mount depends on /srv (mount point mp0)",
	} {
		if !strings.Contains(res.Stdout, want) {
			t.Errorf("rendered notification lacks %q", want)
		}
	}
}

// TestNotificationDelivers sends one real notification through the node's
// configured targets. It runs only with PBP_IT_LIVE_NOTIFY=1; a person must
// confirm that the message arrived and rendered.
func TestNotificationDelivers(t *testing.T) {
	e := setup(t)
	if os.Getenv("PBP_IT_LIVE_NOTIFY") != "1" {
		t.Skip("PBP_IT_LIVE_NOTIFY != 1")
	}
	ctx := context.Background()
	n := &report.PVENotifier{SSH: e.ssh, Node: e.cfg.Target.Node}
	if err := n.EnsureTemplates(ctx); err != nil {
		t.Fatal(err)
	}
	s := fullSummary()
	if err := n.Send(ctx, s); err != nil {
		t.Fatal(err)
	}
	t.Logf("sent run %s (severity %s) at %s; confirm delivery with the node's notification target",
		s.ID, s.Severity(), time.Now().Format(time.RFC3339))
}

// sh runs a command on the node and fails the test on a non-zero exit.
func (e *env) sh(t *testing.T, cmd string) string {
	t.Helper()
	res, err := e.ssh.Run(context.Background(), cmd, nil)
	if err != nil {
		t.Fatalf("%s: %v", cmd, err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("%s: exit %d: %s%s", cmd, res.ExitCode, res.Stdout, res.Stderr)
	}
	return res.Stdout
}

// TestBrokenBackupFails builds a container in the range whose enabled unit
// always fails, backs it up, destroys the container, proofs the backup and
// expects FAILED naming that unit. The backup is deleted at the end.
func TestBrokenBackupFails(t *testing.T) {
	e := setup(t)
	vmid := envInt(t, "PBP_IT_BROKEN_VMID")
	tmpl := envStr(t, "PBP_IT_BROKEN_TEMPLATE")
	store := envStr(t, "PBP_IT_BROKEN_STORAGE")
	ctx := context.Background()
	if lo, hi := e.cfg.Target.VMIDRange[0], e.cfg.Target.VMIDRange[1]; vmid < lo || vmid > hi {
		t.Fatalf("PBP_IT_BROKEN_VMID %d outside the range [%d, %d]", vmid, lo, hi)
	}
	if !slices.Contains(e.cfg.Sources.Storages, store) {
		t.Fatalf("PBP_IT_BROKEN_STORAGE %s is not a configured source", store)
	}
	e.assertRangeEmpty(t, ctx)
	if t.Failed() {
		t.FailNow()
	}
	const unit = "pbp-it-broken.service"
	v := strconv.Itoa(vmid)
	destroyCT := func() {
		e.ssh.Run(ctx, "pct status "+v+" >/dev/null 2>&1 && { pct stop "+v+" >/dev/null 2>&1; pct destroy "+v+" --purge; }", nil)
	}
	t.Cleanup(destroyCT)

	e.sh(t, fmt.Sprintf("pct create %s %s --hostname pbp-it-broken --unprivileged 1 --memory 512 --cores 1 "+
		"--rootfs %s:2 --net0 name=eth0,bridge=%s --start 1",
		v, sshx.Quote(tmpl), e.cfg.Restore.Storage, e.cfg.Target.Bridge))
	unitText := "[Unit]\nDescription=PBP integration test: always fails\n\n[Service]\nType=oneshot\nExecStart=/bin/false\n\n[Install]\nWantedBy=multi-user.target\n"
	res, err := e.ssh.Run(ctx, "pct exec "+v+" -- sh -c "+sshx.Quote("cat > /etc/systemd/system/"+unit+" && systemctl enable "+unit), []byte(unitText))
	if err != nil || res.ExitCode != 0 {
		t.Fatalf("install unit: %v %+v", err, res)
	}
	out := e.sh(t, "vzdump "+v+" --storage "+sshx.Quote(store)+" --mode stop --notes-template '{{guestname}}' 2>&1")
	t.Logf("vzdump:\n%s", out)
	destroyCT()

	cfg := e.cfg
	cfg.Restore.Reproof = true
	key := model.GuestKey{Kind: model.KindLXC, VMID: vmid}
	b := e.newest(t, ctx, cfg, key)
	if b.Storage != store || !strings.Contains(b.VolID, "backup/ct/"+v+"/") {
		t.Fatalf("unexpected backup %+v", b)
	}
	t.Cleanup(func() {
		res, err := e.ssh.Run(context.Background(), "pvesm free "+sshx.Quote(b.VolID), nil)
		if err != nil || res.ExitCode != 0 {
			t.Errorf("delete test backup %s: %v %+v", b.VolID, err, res)
		}
	})

	r := e.proof(t, key)
	if r.Verdict != model.Failed || !slices.Contains(r.FailedUnits, unit) {
		t.Errorf("verdict %s: %s (failed units %v), want FAILED naming %s", r.Verdict, r.Reason, r.FailedUnits, unit)
	}
}

// TestCrashRecovery kills a real "pbp run" process once its job reached stage
// "started", then runs again: the janitor must destroy the leftover guest and
// restore the backup's protected flag.
func TestCrashRecovery(t *testing.T) {
	e := setup(t)
	vmid := envInt(t, "PBP_IT_LXC")
	ctx := context.Background()
	cfg := e.cfg
	cfg.Restore.Reproof = true
	before := e.newest(t, ctx, cfg, model.GuestKey{Kind: model.KindLXC, VMID: vmid})

	// A config copy: reproof on, and notify only on problems so a run that
	// is killed too late still does not notify about a PROVEN guest.
	dir := t.TempDir()
	// Only the storage holding the chosen backup, and it must hold no other
	// kind with this VMID: the CLI proofs every kind for --guest.
	cliCfg := cfg
	cliCfg.Notify.When = "problems"
	cliCfg.Sources.Storages = []string{before.Storage}
	bs, err := e.api.ListBackups(ctx, cfg.Target.Node, before.Storage)
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range bs {
		if b.Key.VMID == vmid && b.Key != before.Key {
			t.Fatalf("%s also holds %s backups; pick another guest", before.Storage, b.Key)
		}
	}
	raw, err := yaml.Marshal(cliCfg)
	if err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfgPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := config.Load(cfgPath); err != nil {
		t.Fatal(err)
	}

	bin := filepath.Join(dir, "pbp")
	build := exec.Command("go", "build", "-o", bin, "git.breadthanalytics.com/tdiana/pbv/cmd/pbp")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	stateDir := filepath.Join(dir, "state")
	// If the test fails after the kill, still clean up what it left.
	t.Cleanup(func() {
		if t.Failed() {
			if err := run.Janitor(context.Background(), e.deps(t, stateDir, cfg)); err != nil {
				t.Logf("cleanup janitor: %v", err)
			}
		}
	})
	cmd := exec.Command(bin, "run", "--guest", strconv.Itoa(vmid), "--config", cfgPath)
	cmd.Env = append(os.Environ(), "PBP_STATE_DIR="+stateDir)
	cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()

	var job state.Job
	deadline := time.Now().Add(15 * time.Minute)
wait:
	for {
		select {
		case err := <-exited:
			t.Fatalf("pbp run exited before stage started: %v", err)
		case <-time.After(100 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			cmd.Process.Kill()
			t.Fatal("stage started not reached")
		}
		b, err := os.ReadFile(filepath.Join(stateDir, "state.json"))
		if err != nil {
			continue
		}
		var st state.State
		if json.Unmarshal(b, &st) != nil {
			continue
		}
		for _, j := range st.InFlight {
			if j.Stage == "started" {
				job = j
				break wait
			}
		}
	}
	if err := cmd.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	<-exited
	t.Logf("killed pbp at stage started: %+v", job)

	// The kill left a guest and (if it was off) the protected flag set.
	gs, err := e.tgt.Owned(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(gs, func(g pve.GuestRef) bool { return g.Key.VMID == job.TempVMID }) {
		t.Errorf("guest %d not left behind by the kill", job.TempVMID)
	}
	if mid := e.volume(t, ctx, before); !before.Protected && !mid.Protected {
		t.Errorf("backup not protected after the kill")
	}

	// Run again on the same state: preflight's janitor cleans up. Only the
	// janitor is wanted here, so run preflight rather than a full proof.
	d := e.deps(t, stateDir, cfg)
	if err := run.Preflight(ctx, d); err != nil {
		t.Fatal(err)
	}
	e.assertRangeEmpty(t, ctx)
	if after := e.volume(t, ctx, before); after.Protected != before.Protected {
		t.Errorf("protected = %v after recovery, was %v", after.Protected, before.Protected)
	}
	if st := d.Store.Snapshot(); len(st.InFlight) != 0 {
		t.Errorf("in-flight jobs left: %+v", st.InFlight)
	}
}
