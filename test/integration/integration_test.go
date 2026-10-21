//go:build integration

// Package integration runs PBP against a real Proxmox VE node. It is opt-in:
// build tag "integration" and PBP_IT_CONFIG pointing at a PBP config for the
// node. PBP under test reaches the node as the restricted pbp user (the node
// helper); only the fixtures use a separate test-only admin key
// (PBP_IT_ADMIN_SSH_KEY). See README.md in this directory for the node setup
// and every environment variable.
package integration

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"maps"
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

	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/config"
	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/discovery"
	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/model"
	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/notes"
	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/pve"
	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/report"
	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/run"
	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/sshx"
	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/state"
	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/target"
)

// pbpNotesLine is the PBP verdict line every proofed backup must start with.
var pbpNotesLine = regexp.MustCompile(`^PBP (PROVEN|FAILED|BOOTED|SKIPPED|ERROR) \d{4}-\d{2}-\d{2} \d{2}:\d{2} · .+$`)

// env holds what every test needs: the loaded config and live connections.
type env struct {
	cfgPath string
	cfg     config.Config
	api     *pve.Client
	ssh     *sshx.Client // PBP's own connection: the pbp user, forced command
	admin   *sshx.Client // test fixtures only: the test-only admin key, a real shell
	tgt     *target.ExistingNode
	version string // daemon version handed to the target and to the built pbp
}

var (
	envOnce sync.Once
	theEnv  *env
	envErr  error
)

// daemonVersion is the version the harness presents as the daemon's: the
// PBP_IT_DAEMON_VERSION override, else the node helper's own version, so the
// suite exercises everything except version compatibility (unit tests cover
// that).
func daemonVersion(ctx context.Context, h sshx.Helper) (string, error) {
	if v := os.Getenv("PBP_IT_DAEMON_VERSION"); v != "" {
		slog.Info("integration: daemon version from PBP_IT_DAEMON_VERSION", "version", v)
		return v, nil
	}
	v, err := h.Version(ctx)
	if err != nil {
		return "", fmt.Errorf("read node helper version (set PBP_IT_DAEMON_VERSION to override): %w", err)
	}
	slog.Info("integration: daemon version taken from the node helper", "version", v)
	return v, nil
}

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
		admin, err := dialAdmin(cfg)
		if err != nil {
			_ = ssh.Close()
			envErr = err
			return
		}
		version, err := daemonVersion(context.Background(), sshx.Helper{R: ssh})
		if err != nil {
			_ = ssh.Close()
			_ = admin.Close()
			envErr = err
			return
		}
		tgt := target.NewExistingNode(cfg, api, sshx.Helper{R: ssh})
		tgt.SetDaemonVersion(version)
		theEnv = &env{cfgPath: path, cfg: cfg, api: api, ssh: ssh, admin: admin, tgt: tgt, version: version}
	})
	if envErr != nil {
		t.Fatal(envErr)
	}
	return theEnv
}

// dialAdmin connects the fixtures' own SSH client with the test-only admin
// key. It is never PBP's client and never in PBP's config: PBP under test has
// no root path (target.ssh.user is pbp). The admin account defaults to root
// and the host and port are the config's.
func dialAdmin(cfg config.Config) (*sshx.Client, error) {
	key, kh := os.Getenv("PBP_IT_ADMIN_SSH_KEY"), os.Getenv("PBP_IT_ADMIN_KNOWN_HOSTS")
	if key == "" || kh == "" {
		return nil, fmt.Errorf("set PBP_IT_ADMIN_SSH_KEY and PBP_IT_ADMIN_KNOWN_HOSTS (the test-only admin key, see README.md)")
	}
	user := os.Getenv("PBP_IT_ADMIN_USER")
	if user == "" {
		user = "root"
	}
	if cfg.Target.SSH.User == user {
		return nil, fmt.Errorf("target.ssh.user %q must differ from the admin user: PBP under test must use the restricted pbp user", user)
	}
	return sshx.Dial(config.SSH{
		Host: cfg.Target.SSH.Host, Port: cfg.Target.SSH.Port, User: user,
		KeyFile: key, KnownHostsFile: kh,
	})
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
// notify the node's real targets. CheckTemplates is real.
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
// every kind with that VMID; the tests proof exactly one backup. This relies
// on run.Options.OnlyVMID selecting by VMID alone: without the filter the
// other kinds' backups would be proofed too.
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
		Notifier: &capture{PVENotifier: &report.PVENotifier{Helper: sshx.Helper{R: e.ssh}, Node: cfg.Target.Node}},
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

// nodeState is what a run must not change outside its own guests and the
// backup it proofs: every guest outside the VMID range (status, name, tags
// and config) and every backup's protected flag and notes on the source
// storages.
type nodeState struct {
	guests  map[model.GuestKey]string
	backups map[string]string
}

// snapshot reads nodeState. Config keys PVE itself changes while a guest
// runs or is backed up ("lock") are left out.
func (e *env) snapshot(t *testing.T, ctx context.Context, cfg config.Config) nodeState {
	t.Helper()
	s := nodeState{guests: map[model.GuestKey]string{}, backups: map[string]string{}}
	gs, err := e.api.Guests(ctx)
	if err != nil {
		t.Fatal(err)
	}
	lo, hi := e.cfg.Target.VMIDRange[0], e.cfg.Target.VMIDRange[1]
	for _, g := range gs {
		if g.Key.VMID >= lo && g.Key.VMID <= hi {
			continue
		}
		c, err := e.api.GuestConfig(ctx, g.Node, g.Key.Kind, g.Key.VMID)
		if err != nil {
			t.Fatalf("config of %s: %v", g.Key, err)
		}
		delete(c, "lock")
		keys := slices.Sorted(maps.Keys(c))
		var b strings.Builder
		fmt.Fprintf(&b, "status=%s name=%s tags=%s", g.Status, g.Name, strings.Join(g.Tags, ";"))
		for _, k := range keys {
			fmt.Fprintf(&b, "\n%s: %s", k, c[k])
		}
		s.guests[g.Key] = b.String()
	}
	for _, st := range cfg.Sources.Storages {
		bs, err := e.api.ListBackups(ctx, cfg.Target.Node, st)
		if err != nil {
			t.Fatalf("list %s: %v", st, err)
		}
		for _, x := range bs {
			n, err := e.api.VolumeNotes(ctx, cfg.Target.Node, st, x.VolID)
			if err != nil {
				t.Fatalf("notes of %s: %v", x.VolID, err)
			}
			s.backups[x.VolID] = fmt.Sprintf("protected=%v notes=%q", x.Protected, n)
		}
	}
	return s
}

// assertUntouched compares the node with before, ignoring the backup that
// was proofed (its notes change by design) and guests or backups that
// appeared or went away meanwhile (a scheduled backup or prune on the node
// is not PBP's doing). This is what a real node can show that no fake can:
// an unexpected side effect (code review I22).
func (e *env) assertUntouched(t *testing.T, ctx context.Context, cfg config.Config, before nodeState, proofed string) {
	t.Helper()
	after := e.snapshot(t, ctx, cfg)
	for k, was := range before.guests {
		if now, ok := after.guests[k]; ok && now != was {
			t.Errorf("guest %s outside the range changed:\nbefore: %s\nafter:  %s", k, was, now)
		}
	}
	for v, was := range before.backups {
		if v == proofed {
			continue
		}
		if now, ok := after.backups[v]; ok && now != was {
			t.Errorf("backup %s changed: before %s, after %s", v, was, now)
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
	return e.proofCfg(t, e.cfg, key)
}

// proofThrowaway proofs a backup of a throwaway guest built inside the VMID
// range. PBP never proofs backups whose VMID is in its range, so the run uses
// the part of the range on the larger side of that VMID for its temporary
// guests; the range-empty checks still cover the whole configured range.
func (e *env) proofThrowaway(t *testing.T, key model.GuestKey) model.GuestResult {
	t.Helper()
	cfg := e.cfg
	lo, hi := cfg.Target.VMIDRange[0], cfg.Target.VMIDRange[1]
	if key.VMID-lo >= hi-key.VMID {
		hi = key.VMID - 1
	} else {
		lo = key.VMID + 1
	}
	if lo > hi {
		t.Fatalf("no room in the range [%d, %d] beside VMID %d", cfg.Target.VMIDRange[0], cfg.Target.VMIDRange[1], key.VMID)
	}
	cfg.Target.VMIDRange = [2]int{lo, hi}
	t.Logf("proofing with vmid_range [%d, %d]", lo, hi)
	return e.proofCfg(t, cfg, key)
}

func (e *env) proofCfg(t *testing.T, cfg config.Config, key model.GuestKey) model.GuestResult {
	t.Helper()
	ctx := context.Background()
	cfg.Restore.Reproof = true
	before := e.volume(t, ctx, e.newest(t, ctx, cfg, key))
	t.Logf("proofing %s (protected=%v, notes=%q)", before.VolID, before.Protected, before.Notes)
	node := e.snapshot(t, ctx, cfg)

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
	e.assertUntouched(t, ctx, cfg, node, before.VolID)
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

// TestNotificationRenders checks the templates are installed and renders a full summary
// with the node's proxmox-notify (no delivery). This proves the new "pbp"
// template type is found in /etc/pve/notification-templates/default/ and
// that hyphenated variables work in #if and #each.
func TestNotificationRenders(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	n := &report.PVENotifier{Helper: sshx.Helper{R: e.ssh}, Node: e.cfg.Target.Node}
	if err := n.CheckTemplates(ctx); err != nil {
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
	res, err := e.admin.Run(ctx, cmd, in)
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
		"Guests PBP could not look inside (booted only):\n  qemu/990002 it-vm: guest agent missing\n",
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
	n := &report.PVENotifier{Helper: sshx.Helper{R: e.ssh}, Node: e.cfg.Target.Node}
	if err := n.CheckTemplates(ctx); err != nil {
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
	res, err := e.admin.Run(context.Background(), cmd, nil)
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
		_, _ = e.admin.Run(ctx, "pct status "+v+" >/dev/null 2>&1 && { pct stop "+v+" >/dev/null 2>&1; pct destroy "+v+" --purge; }", nil)
	}
	t.Cleanup(destroyCT)

	e.sh(t, fmt.Sprintf("pct create %s %s --hostname pbp-it-broken --unprivileged 1 --memory 512 --cores 1 "+
		"--rootfs %s:2 --net0 name=eth0,bridge=%s --start 1",
		v, sshx.Quote(tmpl), e.cfg.Restore.Storage, e.cfg.Target.Bridge))
	unitText := "[Unit]\nDescription=PBP integration test: always fails\n\n[Service]\nType=oneshot\nExecStart=/bin/false\n\n[Install]\nWantedBy=multi-user.target\n"
	res, err := e.admin.Run(ctx, "pct exec "+v+" -- sh -c "+sshx.Quote("cat > /etc/systemd/system/"+unit+" && systemctl enable "+unit), []byte(unitText))
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
		res, err := e.admin.Run(context.Background(), "pvesm free "+sshx.Quote(b.VolID), nil)
		if err != nil || res.ExitCode != 0 {
			t.Errorf("delete test backup %s: %v %+v", b.VolID, err, res)
		}
	})

	r := e.proofThrowaway(t, key)
	if r.Verdict != model.Failed || !slices.Contains(r.FailedUnits, unit) {
		t.Errorf("verdict %s: %s (failed units %v), want FAILED naming %s", r.Verdict, r.Reason, r.FailedUnits, unit)
	}
}

// TestCrashRecovery kills a real "pbp run" process once its job reached a
// stage, then runs again: the janitor must destroy the leftover guest and
// restore the backup's protected flag. At stage "restored" the kill comes
// while PVE's restore task is still running; at "started", once the copy
// runs (code review I22: both, not only "started").
func TestCrashRecovery(t *testing.T) {
	e := setup(t)
	for _, stage := range []string{state.StageRestored, state.StageStarted} {
		t.Run(stage, func(t *testing.T) { e.crashAt(t, stage) })
	}
}

func (e *env) crashAt(t *testing.T, stage string) {
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
	build := exec.Command("go", "build", "-ldflags", "-X main.version="+e.version, "-o", bin, "git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/cmd/pbp")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	stateDir := filepath.Join(dir, "state")
	node := e.snapshot(t, ctx, cfg)
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
			t.Fatalf("pbp run exited before stage %s: %v", stage, err)
		case <-time.After(100 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			_ = cmd.Process.Kill()
			t.Fatalf("stage %s not reached", stage)
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
			// At "restored", wait until PVE has created the guest, so
			// the kill leaves one behind mid-restore.
			if j.Stage == stage && (stage != state.StageRestored || e.exists(t, ctx, j.TempVMID)) {
				job = j
				break wait
			}
		}
	}
	if err := cmd.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	waitErr := <-exited // a SIGKILLed process exits with an error; expected
	t.Logf("killed pbp at stage %s (%v): %+v", stage, waitErr, job)

	// The kill left a guest and (if it was off) the protected flag set.
	if !e.exists(t, ctx, job.TempVMID) {
		t.Errorf("guest %d not left behind by the kill", job.TempVMID)
	}
	if mid := e.volume(t, ctx, before); !before.Protected && !mid.Protected {
		t.Errorf("backup not protected after the kill")
	}

	// Run again on the same state: preflight's janitor cleans up. Only the
	// janitor is wanted here, so run preflight rather than a full proof. A
	// guest still locked by PVE's restore task cannot be destroyed yet: the
	// janitor keeps its record and preflight refuses, so retry until the
	// task is over, as the next scheduled run would.
	d := e.deps(t, stateDir, cfg)
	for try := 1; ; try++ {
		err := run.Preflight(ctx, d)
		if err == nil {
			break
		}
		if try == 40 {
			t.Fatalf("preflight after the crash: %v", err)
		}
		t.Logf("preflight try %d: %v", try, err)
		time.Sleep(15 * time.Second)
	}
	e.assertRangeEmpty(t, ctx)
	if after := e.volume(t, ctx, before); after.Protected != before.Protected {
		t.Errorf("protected = %v after recovery, was %v", after.Protected, before.Protected)
	}
	if st := d.Store.Snapshot(); len(st.InFlight) != 0 {
		t.Errorf("in-flight jobs left: %+v", st.InFlight)
	}
	e.assertUntouched(t, ctx, cfg, node, before.VolID)
}

// exists reports whether a guest with vmid exists on the node.
func (e *env) exists(t *testing.T, ctx context.Context, vmid int) bool {
	t.Helper()
	gs, err := e.api.Guests(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return slices.ContainsFunc(gs, func(g pve.GuestRef) bool { return g.Key.VMID == vmid })
}

// throwaway builds a container at vmid from tmpl (unprivileged, rootfs on
// the restore storage, NIC on the isolated bridge, extra pct create args
// appended), runs configure on it, backs it up to PBP_IT_BROKEN_STORAGE with
// "vzdump --mode stop", destroys it (clearing its protection first) and
// returns the backup. The backup is deleted with "pvesm free" when the test
// ends.
func (e *env) throwaway(t *testing.T, vmid int, tmpl, hostname, extra string, configure func(v string)) model.Backup {
	t.Helper()
	store := envStr(t, "PBP_IT_BROKEN_STORAGE")
	ctx := context.Background()
	if lo, hi := e.cfg.Target.VMIDRange[0], e.cfg.Target.VMIDRange[1]; vmid < lo || vmid > hi {
		t.Fatalf("VMID %d outside the range [%d, %d]", vmid, lo, hi)
	}
	if !slices.Contains(e.cfg.Sources.Storages, store) {
		t.Fatalf("PBP_IT_BROKEN_STORAGE %s is not a configured source", store)
	}
	e.assertRangeEmpty(t, ctx)
	if t.Failed() {
		t.FailNow()
	}
	v := strconv.Itoa(vmid)
	destroyCT := func() {
		_, _ = e.admin.Run(ctx, "pct status "+v+" >/dev/null 2>&1 && { pct stop "+v+" >/dev/null 2>&1; pct set "+v+" --protection 0; pct destroy "+v+" --purge; }", nil)
	}
	t.Cleanup(destroyCT)
	e.sh(t, fmt.Sprintf("pct create %s %s --hostname %s --unprivileged 1 --memory 256 --cores 1 "+
		"--rootfs %s:2 --net0 name=eth0,bridge=%s --start 1 %s",
		v, sshx.Quote(tmpl), hostname, e.cfg.Restore.Storage, e.cfg.Target.Bridge, extra))
	configure(v)
	out := e.sh(t, "vzdump "+v+" --storage "+sshx.Quote(store)+" --mode stop --notes-template '{{guestname}}' 2>&1")
	t.Logf("vzdump:\n%s", out)
	destroyCT()
	if _, err := e.api.GuestConfig(ctx, e.cfg.Target.Node, model.KindLXC, vmid); err == nil {
		t.Fatalf("source container %d still exists", vmid)
	}

	cfg := e.cfg
	cfg.Restore.Reproof = true
	b := e.newest(t, ctx, cfg, model.GuestKey{Kind: model.KindLXC, VMID: vmid})
	if b.Storage != store || !strings.Contains(b.VolID, "backup/ct/"+v+"/") {
		t.Fatalf("unexpected backup %+v", b)
	}
	t.Logf("created backup %s", b.VolID)
	t.Cleanup(func() {
		res, err := e.admin.Run(context.Background(), "pvesm free "+sshx.Quote(b.VolID), nil)
		if err != nil || res.ExitCode != 0 {
			t.Errorf("delete test backup %s: %v %+v", b.VolID, err, res)
		}
	})
	return b
}

// waitOpenRCDefault waits until the container's OpenRC reached the default
// runlevel, plus a few seconds for its services to start (or crash).
func (e *env) waitOpenRCDefault(t *testing.T, v string) {
	t.Helper()
	// The runlevel reads "default" as soon as the change starts; its
	// services start only afterwards, so also wait for the openrc process
	// that is changing runlevel to exit.
	settled := "[ \"$(rc-status -r)\" = default ] && ! pidof openrc >/dev/null"
	for i := 0; i < 120; i++ {
		res, err := e.admin.Run(context.Background(), "pct exec "+v+" -- sh -c "+sshx.Quote(settled), nil)
		if err == nil && res.ExitCode == 0 {
			time.Sleep(2 * time.Second)
			return
		}
		time.Sleep(time.Second)
	}
	t.Fatalf("container %s did not reach the OpenRC default runlevel", v)
}

// rcCrashed runs "rc-status --crashed" in the container and returns its
// output and exit code.
func (e *env) rcCrashed(t *testing.T, v string) (string, int) {
	t.Helper()
	res, err := e.admin.Run(context.Background(), "pct exec "+v+" -- rc-status --crashed", nil)
	if err != nil {
		t.Fatal(err)
	}
	return res.Stdout, res.ExitCode
}

// TestProofAlpineHealthy proofs a plain Alpine container's backup through the
// OpenRC probe ("rc-status --crashed", chosen by ostype alpine).
func TestProofAlpineHealthy(t *testing.T) {
	e := setup(t)
	vmid := envInt(t, "PBP_IT_ALPINE_VMID")
	tmpl := envStr(t, "PBP_IT_ALPINE_TEMPLATE")
	e.throwaway(t, vmid, tmpl, "pbp-it-alpine", "", func(v string) {
		e.waitOpenRCDefault(t, v)
		out, code := e.rcCrashed(t, v)
		t.Logf("source: rc-status --crashed exit %d, output %q", code, out)
		if strings.TrimSpace(out) != "" || (code != 0 && code != 1) {
			t.Fatalf("source not healthy: exit %d, output %q", code, out)
		}
	})
	r := e.proofThrowaway(t, model.GuestKey{Kind: model.KindLXC, VMID: vmid})
	if r.Verdict != model.Proven {
		t.Errorf("verdict %s: %s, want PROVEN", r.Verdict, r.Reason)
	}
}

// TestProofAlpineCrashed proofs an Alpine container whose default runlevel
// holds an OpenRC service whose daemon exits right after start, so
// "rc-status --crashed" lists it; expects FAILED naming that service.
func TestProofAlpineCrashed(t *testing.T) {
	e := setup(t)
	vmid := envInt(t, "PBP_IT_ALPINE_CRASH_VMID")
	tmpl := envStr(t, "PBP_IT_ALPINE_TEMPLATE")
	const svc = "pbp-it-crash"
	script := "#!/sbin/openrc-run\ndescription=\"PBP integration test: a daemon that exits right after start\"\n" +
		"command=\"/bin/sleep\"\ncommand_args=\"1\"\ncommand_background=no\n"
	e.throwaway(t, vmid, tmpl, "pbp-it-alpcrash", "", func(v string) {
		res, err := e.admin.Run(context.Background(), "pct exec "+v+" -- sh -c "+sshx.Quote(
			"cat > /etc/init.d/"+svc+" && chmod 755 /etc/init.d/"+svc+" && rc-update add "+svc+" default"), []byte(script))
		if err != nil || res.ExitCode != 0 {
			t.Fatalf("install service: %v %+v", err, res)
		}
		// Reboot so the service is started by the default runlevel, as it
		// will be in the restored copy, and check OpenRC calls it crashed.
		e.sh(t, "pct reboot "+v)
		e.waitOpenRCDefault(t, v)
		out, code := e.rcCrashed(t, v)
		t.Logf("source: rc-status --crashed exit %d, output %q", code, out)
		if !slices.Contains(strings.Fields(out), svc) {
			t.Fatalf("source: %s not crashed (exit %d, output %q)", svc, code, out)
		}
	})
	r := e.proofThrowaway(t, model.GuestKey{Kind: model.KindLXC, VMID: vmid})
	if r.Verdict != model.Failed || !slices.Contains(r.FailedUnits, svc) {
		t.Errorf("verdict %s: %s (failed units %v), want FAILED naming %s", r.Verdict, r.Reason, r.FailedUnits, svc)
	}
}

// TestProofProtectedGuest proofs the backup of a container with
// "protection: 1": PBP must clear protection on its restored copy so the
// copy is destroyed and the range is empty afterwards.
func TestProofProtectedGuest(t *testing.T) {
	e := setup(t)
	vmid := envInt(t, "PBP_IT_PROTECTED_VMID")
	tmpl := envStr(t, "PBP_IT_PROTECTED_TEMPLATE")
	// nesting=1 as the PVE GUI sets it: without it systemd-logind fails in
	// an unprivileged Debian 12 container, and the honest verdict is FAILED.
	b := e.throwaway(t, vmid, tmpl, "pbp-it-protected", "--protection 1 --features nesting=1", func(v string) {
		if !strings.Contains(e.sh(t, "pct config "+v), "\nprotection: 1\n") {
			t.Fatal("source container is not protected")
		}
		res, _ := e.admin.Run(context.Background(), "pct exec "+v+" -- sh -c "+sshx.Quote(
			"timeout 120 systemctl is-system-running --wait; systemctl --failed --no-legend --plain"), nil)
		t.Logf("source state: %s", strings.TrimSpace(res.Stdout))
	})
	orig, err := e.api.OriginalConfig(context.Background(), e.cfg.Target.Node, b.VolID)
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`(?m)^protection: 1$`).MatchString(orig) {
		t.Fatalf("backup config lacks protection: 1:\n%s", orig)
	}
	r := e.proofThrowaway(t, model.GuestKey{Kind: model.KindLXC, VMID: vmid})
	if r.Verdict != model.Proven {
		t.Errorf("verdict %s: %s, want PROVEN", r.Verdict, r.Reason)
	}
}
