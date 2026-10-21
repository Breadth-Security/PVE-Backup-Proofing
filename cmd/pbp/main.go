// Command pbp is the Proxmox Backup Proofing daemon and CLI.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"os/signal"
	"os/user"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/robfig/cron/v3"

	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/config"
	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/daemon"
	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/fsutil"
	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/health"
	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/hostsetup"
	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/model"
	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/node"
	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/nodesetup"
	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/pve"
	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/report"
	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/run"
	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/sshx"
	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/state"
	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/target"
)

// usage lists every subcommand; the node ones come from nodeUsage, so the
// options shown match the ones parsed.
var usage = "usage: pbp daemon|run [--guest VMID]|check|health [--config PATH]\n       pbp version\n       pbp setup [--dir DIR] [--deb [--replace]]\n" +
	"       " + strings.TrimPrefix(nodeUsage, "usage: ")

// version is set at build time: -ldflags "-X main.version=...".
var version = "dev"

// Exit codes.
const (
	exitOK    = 0
	exitFail  = 1
	exitUsage = 2
	exitBusy  = 3
	// exitConfig is the daemon's exit for a config it cannot use (sysexits
	// EX_CONFIG). pbp.service does not restart on it: restarting cannot fix
	// a config, and pbp setup is what finishes one.
	exitConfig = 78
)

const msgBusy = "a run is already in progress"

// A failed write to stdout or stderr cannot be reported anywhere else, so
// these console writers deliberately drop the error.
func fprint(w io.Writer, a ...any)                 { _, _ = fmt.Fprint(w, a...) }
func fprintf(w io.Writer, format string, a ...any) { _, _ = fmt.Fprintf(w, format, a...) }
func fprintln(w io.Writer, a ...any)               { _, _ = fmt.Fprintln(w, a...) }

// catchNodeSignals keeps a hangup, or a write to a closed stdout or stderr,
// from killing the node helper: it must reach its own deadline handling
// (kill the guest command's process group, audit the outcome) after the SSH
// client is gone. The signals are caught and dropped, not ignored: an
// ignored disposition survives exec, so pct, lxc-attach and the guest's
// check commands would inherit it, while a caught one resets to the default
// in the child. With SIGPIPE caught, writes to a closed stdout or stderr fail
// with EPIPE instead of killing the process. stop restores the defaults.
func catchNodeSignals() (stop func()) {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGPIPE, syscall.SIGHUP)
	done := make(chan struct{})
	go func() {
		for {
			select {
			case <-ch:
			case <-done:
				return
			}
		}
	}()
	return func() {
		signal.Stop(ch)
		close(done)
	}
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "node" {
		// The helper outlives a vanished SSH client on purpose (see
		// catchNodeSignals); it runs until the process exits.
		catchNodeSignals()
	} else {
		// A command whose SSH session dropped must not die from SIGPIPE
		// on its next write to stdout before it has torn down.
		signal.Notify(make(chan os.Signal, 1), syscall.SIGPIPE)
	}
	ctx, stop := signal.NotifyContext(context.Background(), stopSignals(os.Args[1:], signal.Ignored(syscall.SIGHUP))...)
	code := newApp(os.Stdout, os.Stderr).main(ctx, os.Args[1:])
	stop()
	os.Exit(code)
}

// stopSignals are the signals that end a command gracefully (its context is
// cancelled, so a run tears its guests down). SIGHUP counts too, so a
// manual `pbp run` whose SSH session drops still cleans up (code review
// I5). Not for the node helper, which outlives a hangup on purpose (see
// catchNodeSignals), and not when SIGHUP is already ignored (nohup):
// registering it would undo that, and logging out would cancel the run.
func stopSignals(args []string, hupIgnored bool) []os.Signal {
	sigs := []os.Signal{syscall.SIGTERM, syscall.SIGINT}
	if !hupIgnored && (len(args) == 0 || args[0] != "node") {
		sigs = append(sigs, syscall.SIGHUP)
	}
	return sigs
}

// app is the CLI with its outside world injectable for tests.
type app struct {
	stdout, stderr io.Writer
	now            func() time.Time
	// connect builds the node-facing dependencies for one run or check.
	connect func(cfg config.Config) wiring

	// pbp node: stdin, the real System, the audit log, the effective-UID
	// source and the template directory are seams for tests.
	stdin       io.Reader
	nodeSys     node.System
	audit       auditLog
	euid        func() int
	templateDir string
	// ownerOf returns the owner (uid and user name) of a path; a seam for
	// the root refusal of daemon, run and check.
	ownerOf func(path string) (uid int, name string, err error)
	// inContainer reports whether PBP runs in a container (no sudo there).
	inContainer func() bool

	// pbp node setup and uninstall: whether stdin is a terminal, and every
	// outside effect of the node setup library.
	isTerminal   func() bool
	nodeSetupEnv nodesetup.Env

	// pbp setup: turning off terminal echo for the reply (only called when
	// isTerminal) and the default time zone offered.
	echoOff   func() (restore func(), err error)
	defaultTZ func() string
	// probeHostKey reads the SSH host key at the configured node address,
	// to catch an address that reaches another machine; nil skips it.
	probeHostKey hostsetup.HostKeyProbe
	// onPVENode reports whether this host is a Proxmox VE node, where pbp
	// setup --deb can set the node up in the same run.
	onPVENode func() bool
}

func newApp(stdout, stderr io.Writer) *app {
	return &app{stdout: stdout, stderr: stderr, now: time.Now, connect: connectReal,
		stdin: os.Stdin, nodeSys: node.NewSystem(),
		euid: os.Geteuid, templateDir: node.DefaultTemplateDir, ownerOf: ownerOf, inContainer: inContainer,
		isTerminal:   func() bool { return nodesetup.IsTerminal(int(os.Stdin.Fd())) },
		nodeSetupEnv: nodesetup.NewEnv(),
		echoOff:      func() (func(), error) { return nodesetup.EchoOff(int(os.Stdin.Fd())) },
		defaultTZ:    func() string { return hostTZ(os.Getenv, "/etc/timezone", "/etc/localtime") },
		probeHostKey: hostsetup.ProbeHostKey,
		onPVENode:    func() bool { return exists("/etc/pve/local") }}
}

// wiring is what talks to the node. apiErr and sshErr record setup failures;
// target and notifier are then stand-ins that fail, so a run still reaches
// its error path, attempts a notification and records outcome "error".
type wiring struct {
	api            pve.API
	target         target.Target
	notifier       report.Notifier
	apiErr, sshErr error
	close          func()
}

// setupErr joins the setup failures (nil if none).
func (w wiring) setupErr() error { return errors.Join(w.apiErr, w.sshErr) }

func connectReal(cfg config.Config) wiring {
	var api pve.API
	c, apiErr := pve.New(cfg.Target.API)
	if apiErr == nil {
		api = c
	}
	var ssh sshx.Runner
	cl, sshErr := sshx.Dial(cfg.Target.SSH)
	closeFn := func() {}
	if sshErr == nil {
		ssh, closeFn = cl, func() { _ = cl.Close() }
	}
	w := assemble(cfg, api, apiErr, ssh, sshErr)
	w.close = closeFn
	return w
}

// assemble builds the wiring from a possibly failed API client and SSH
// connection.
func assemble(cfg config.Config, api pve.API, apiErr error, ssh sshx.Runner, sshErr error) wiring {
	w := wiring{api: api, apiErr: apiErr, sshErr: sshErr, close: func() {}}
	if apiErr != nil {
		apiErr = fmt.Errorf("pve api: %w", apiErr)
		w.apiErr = apiErr
	}
	if sshErr != nil {
		sshErr = fmt.Errorf("ssh %s: %w", cfg.Target.SSH.Host, sshErr)
		w.sshErr = sshErr
		ssh = downRunner{sshErr}
	}
	helper := sshx.Helper{R: ssh}
	existing := target.NewExistingNode(cfg, api, helper)
	existing.SetDaemonVersion(version)
	w.target = existing
	if err := w.setupErr(); err != nil {
		w.target = unreachable{Target: w.target, err: err}
	}
	w.notifier = &report.PVENotifier{Helper: helper, Node: cfg.Target.Node}
	return w
}

// downRunner fails every command with the error that stopped the SSH dial.
type downRunner struct{ err error }

func (r downRunner) Run(context.Context, string, []byte) (sshx.Result, error) {
	return sshx.Result{}, r.err
}

// unreachable fails preflight with the setup error, so nothing else runs.
type unreachable struct {
	target.Target
	err error
}

func (u unreachable) CheckNode(context.Context) error { return u.err }
func (u unreachable) Prepare(context.Context) error   { return u.err }

func (a *app) main(ctx context.Context, args []string) int {
	if len(args) == 0 {
		fprint(a.stderr, usage)
		return exitUsage
	}
	cmd := args[0]
	switch cmd {
	case "node":
		return a.nodeMain(ctx, args[1:])
	case "setup":
		return a.setupMain(ctx, args[1:])
	}
	fs := flag.NewFlagSet("pbp "+cmd, flag.ContinueOnError)
	fs.SetOutput(a.stderr)
	cfgFlag := fs.String("config", "", "config file (default $PBP_CONFIG or /etc/pbp/config.yaml)")
	var guest *int
	switch cmd {
	case "run":
		guest = fs.Int("guest", 0, "proof only this guest's newest backup")
	case "daemon", "check", "health", "version":
	default:
		fprintf(a.stderr, "pbp: unknown subcommand %q\n%s", cmd, usage)
		return exitUsage
	}
	if err := fs.Parse(args[1:]); err != nil {
		return exitUsage
	}
	if fs.NArg() > 0 {
		fprintf(a.stderr, "pbp %s: unexpected arguments %q\n%s", cmd, fs.Args(), usage)
		return exitUsage
	}

	if cmd == "version" {
		fprintf(a.stdout, "pbp %s\n%s\n", version, runtime.Version())
		return exitOK
	}
	if guest != nil && !guestValid(fs, *guest) {
		fprintf(a.stderr, "pbp run: --guest must be a positive integer VMID\n%s", usage)
		return exitUsage
	}
	path := config.ResolvePath(*cfgFlag)
	cfg, err := config.Load(path)
	if err != nil {
		if cmd == "health" {
			fprintln(a.stdout, err)
		} else {
			fprintf(a.stderr, "pbp %s: %v\n", cmd, err)
		}
		if cmd == "daemon" {
			return exitConfig
		}
		return exitFail
	}
	if cmd != "health" {
		// Loading the config only reads; this runs before the lock or
		// any write.
		if err := a.refuseRoot(args, path, cfg.Logging.Dir); err != nil {
			fprintf(a.stderr, "pbp %s: %v\n", cmd, err)
			if cmd == "daemon" {
				return exitConfig
			}
			return exitFail
		}
	}
	switch cmd {
	case "daemon":
		return a.daemon(ctx, cfg)
	case "run":
		return a.runNow(ctx, cfg, *guest)
	case "check":
		return a.check(ctx, cfg, path)
	default:
		return a.health(cfg)
	}
}

// refuseRoot stops daemon, run and check as root when the state directory
// or the run-log directory belongs to another user (the service's, pbp
// under the .deb). Everything they write there (state.json, the run lock,
// run folders) would be root's, and the service could no longer read its
// own state or prune its runs (issue #63). A root-only install, whose
// directories are root's or not made yet, is not refused. args are the
// command's own arguments, repeated in the suggested command with the
// config path, since sudo drops PBP_CONFIG.
func (a *app) refuseRoot(args []string, cfgPath, logDir string) error {
	if a.euid == nil || a.euid() != 0 || a.ownerOf == nil {
		return nil
	}
	for _, dir := range []string{daemon.StateDir(), logDir} {
		uid, name, err := a.ownerOf(dir)
		if errors.Is(err, fs.ErrNotExist) || (err == nil && uid == 0) {
			continue
		}
		if err != nil {
			return fmt.Errorf("as root: cannot check who owns %s: %w", dir, err)
		}
		way := "if PBP is meant to run as root, give the state and log directories to root first"
		if a.inContainer != nil && a.inContainer() {
			// No sudo in the image: the container's user is set by the
			// image or a `user:` override in the compose file.
			who := strconv.Itoa(uid)
			if name != "" {
				who = name + " (uid " + who + ")"
			}
			return fmt.Errorf("refusing to run as root: %s belongs to %s, and files root writes there would lock the service out of them; "+
				"run the container as uid %d, the image's own user (remove any `user:` override in docker-compose.yml), or, %s",
				dir, who, uid, way)
		}
		who := name
		if who == "" {
			who = "#" + strconv.Itoa(uid)
		}
		run := append([]string{"pbp"}, args...)
		if !slices.ContainsFunc(args, func(a string) bool {
			return a == "--config" || a == "-config" || strings.HasPrefix(a, "--config=") || strings.HasPrefix(a, "-config=")
		}) {
			run = append(run, "--config", cfgPath)
		}
		if d := os.Getenv("PBP_STATE_DIR"); d != "" {
			run = append([]string{"PBP_STATE_DIR=" + d}, run...)
		}
		return fmt.Errorf("refusing to run as root: %s belongs to %s, and files root writes there would lock the service out of them; "+
			"run it as that user: sudo -u %s %s (or, %s)",
			dir, who, who, strings.Join(run, " "), way)
	}
	return nil
}

// inContainer reports whether this process runs in a Docker or Podman
// container, by the marker file each runtime creates.
func inContainer() bool {
	for _, p := range []string{"/.dockerenv", "/run/.containerenv"} {
		if _, err := os.Stat(p); err == nil {
			return true
		}
	}
	return false
}

// ownerOf returns the owner of path, with the user name when it resolves.
func ownerOf(path string) (int, string, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return 0, "", err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, "", fmt.Errorf("no owner for %s", path)
	}
	uid := int(st.Uid)
	name := ""
	if u, err := user.LookupId(strconv.Itoa(uid)); err == nil {
		name = u.Username
	}
	return uid, name, nil
}

// guestValid reports whether --guest was left unset or given a positive VMID.
func guestValid(fs *flag.FlagSet, guest int) bool {
	set := false
	fs.Visit(func(f *flag.Flag) { set = set || f.Name == "guest" })
	return !set || guest > 0
}

func statePath() string   { return filepath.Join(daemon.StateDir(), "state.json") }
func startedPath() string { return filepath.Join(daemon.StateDir(), "daemon.started") }

// lock takes the run lock for cmd, printing why not on failure.
func (a *app) lock(cmd string) (func(), int) {
	release, err := daemon.AcquireLock(daemon.LockPath())
	if errors.Is(err, daemon.ErrBusy) {
		fprintln(a.stderr, msgBusy)
		return nil, exitBusy
	}
	if err != nil {
		fprintf(a.stderr, "pbp %s: %v\n", cmd, err)
		return nil, exitFail
	}
	return release, exitOK
}

func (a *app) daemon(ctx context.Context, cfg config.Config) int {
	slog.SetDefault(report.NewLogger(a.stdout))
	slog.Info("pbp daemon starting", "version", version, "go", runtime.Version())
	if err := writeStarted(startedPath(), a.now()); err != nil {
		slog.Error("record daemon start", "err", err)
	}
	a.recoverAtStart(ctx, cfg)
	err := daemon.Serve(ctx, cfg, func(ctx context.Context, stop <-chan struct{}, trigger string) {
		a.runOnce(ctx, cfg, run.Options{Trigger: trigger, Stop: stop})
	})
	if err != nil {
		slog.Error("daemon", "err", err)
		return exitFail
	}
	return exitOK
}

func (a *app) runNow(ctx context.Context, cfg config.Config, guest int) int {
	release, code := a.lock("run")
	if release == nil {
		return code
	}
	defer release()
	// A guest excluded by a guests rule is refused before any run starts.
	// One excluded only by tag is known after the run reads the tags.
	ex := cfg.Selection.Exclude
	if guest != 0 {
		vm, ct := ex.ExcludedBy(string(model.KindQEMU), guest, nil), ex.ExcludedBy(string(model.KindLXC), guest, nil)
		if vm != "" && ct != "" {
			rule := vm
			if ct != vm {
				rule += "; " + ct
			}
			fprintf(a.stdout, "guest %d is excluded by config (%s); not proofed\n", guest, rule)
			return exitFail
		}
	}
	s := a.runOnce(ctx, cfg, run.Options{Trigger: "manual", OnlyVMID: guest})
	c := s.Counts()
	fprintf(a.stdout, "run %s: %s; proven %d, failed %d, booted %d, skipped %d, error %d\n",
		s.ID, s.Severity(), c[model.Proven], c[model.Failed], c[model.Booted], c[model.Skipped], c[model.Error])
	if s.RunError != "" {
		fprintf(a.stdout, "run error: %s\n", s.RunError)
	}
	if guest != 0 && s.RunError == "" && len(s.Results) == 0 {
		fprintf(a.stdout, "no backup found for guest %d\n", guest)
		return exitFail
	}
	if guest != 0 && len(s.Results) > 0 && !slices.ContainsFunc(s.Results, func(r model.GuestResult) bool { return !r.Excluded }) {
		for _, r := range s.Results {
			fprintf(a.stdout, "guest %d is %s; not proofed\n", guest, r.Reason)
		}
		return exitFail
	}
	if s.Severity() == "error" {
		return exitFail
	}
	return exitOK
}

// recoverTimeout bounds startup recovery: it runs before scheduling starts,
// so a hung task on the node must not hold the daemon up (what it leaves is
// the first run's janitor's).
const recoverTimeout = 15 * time.Minute

// recoverAtStart, holding the run lock, marks a run that PBP's death
// interrupted and cleans up jobs it left in flight (run.Recover), within
// recoverTimeout. It connects to the node only when there is something to
// recover; a busy lock (a manual run) skips it, as that run's own start
// does the same.
func (a *app) recoverAtStart(ctx context.Context, cfg config.Config) {
	ctx, cancel := context.WithTimeout(ctx, recoverTimeout)
	defer cancel()
	release, err := daemon.AcquireLock(daemon.LockPath())
	if err != nil {
		slog.Warn("startup recovery skipped", "err", err)
		return
	}
	defer release()
	store, err := state.Open(statePath())
	if err != nil {
		slog.Error("startup recovery: open state", "err", err)
		return
	}
	snap := store.Snapshot()
	if len(snap.InFlight) == 0 && !slices.ContainsFunc(snap.Runs, func(r state.RunRecord) bool { return r.Outcome == state.OutcomeRunning }) {
		return
	}
	w := a.connect(cfg)
	defer w.close()
	d := run.Deps{Cfg: cfg, Store: store, Log: slog.Default(), Now: a.now}
	if w.apiErr == nil {
		d.API, d.Target = w.api, w.target
	}
	if w.sshErr == nil {
		d.Notifier = w.notifier
	}
	run.Recover(ctx, d)
}

// runOnce performs one run with real wiring. The caller holds the run lock.
// Logs go to stdout and to the run's own folder (run.log and one file per guest).
func (a *app) runOnce(ctx context.Context, cfg config.Config, opt run.Options) model.RunSummary {
	opt.RunID = run.NewRunID(a.now(), opt.Trigger)
	d := run.Deps{Cfg: cfg, Now: a.now, Version: version}
	rl, logErr := report.NewRunLog(cfg.Logging.Dir, a.now(), opt.Trigger, a.now)
	var log *slog.Logger
	if logErr != nil {
		log = report.NewLogger(a.stdout)
		log.Error("open run log", "err", logErr)
	} else {
		defer func() { _ = rl.Close() }()
		// Logger first: a guest logger captures the run handler when Guest is called.
		log = rl.Logger(a.stdout)
		d.GuestLog = rl.Guest
		opt.LogDir = rl.Dir()
		defer func() {
			if err := report.PruneRuns(cfg.Logging.Dir, cfg.Logging.RetainRuns, rl.Dir()); err != nil {
				log.Error("prune run logs", "err", err)
			}
		}()
	}

	w := a.connect(cfg)
	defer w.close()
	store, err := state.Open(statePath())
	if err != nil {
		log.Error("RUN NOT STARTED: cannot open state store; health will report unhealthy", "err", err)
		s := model.RunSummary{ID: opt.RunID, Trigger: opt.Trigger, Started: a.now(), Ended: a.now(),
			RunError: "run not started: state store: " + err.Error(), LogDir: opt.LogDir}
		// Tell the node, so the night is not silent on hosts without
		// health monitoring (code review 6.10).
		if w.sshErr == nil && w.notifier != nil {
			if nerr := w.notifier.Send(ctx, s); nerr != nil {
				log.Error("send notification", "err", nerr)
			}
		}
		return s
	}
	if w.sshErr != nil {
		log.Error("SSH TO THE NODE FAILED: the run fails and no notification can be sent; health will report unhealthy", "err", w.sshErr)
	}
	if w.apiErr != nil {
		log.Error("PVE API CLIENT FAILED: the run fails", "err", w.apiErr)
	}
	d.API, d.Target, d.Store, d.Log, d.Notifier = w.api, w.target, store, log, w.notifier
	s := run.Run(ctx, d, opt)
	return s
}

func (a *app) check(ctx context.Context, cfg config.Config, path string) int {
	release, code := a.lock("check")
	if release == nil {
		return code
	}
	defer release()
	fprintf(a.stdout, "config %s: ok\n", path)
	c := &checkOut{w: a.stdout, ok: true}
	store, err := state.Open(statePath())
	c.line("state "+statePath(), err)
	return a.checkSteps(ctx, cfg, store, c)
}

// checkOut prints check lines and remembers whether any failed.
type checkOut struct {
	w  io.Writer
	ok bool
}

func (c *checkOut) line(name string, err error) {
	if err != nil {
		c.ok = false
		fprintf(c.w, "%s: FAIL: %v\n", name, err)
		return
	}
	fprintf(c.w, "%s: ok\n", name)
}

// Preflight line names for pbp check and for pbp setup (no janitor).
const (
	preflightName         = "preflight (node helper, node version, isolated bridge, restore storage, notification templates, janitor, VMID range)"
	preflightReadOnlyName = "preflight (node helper, node version, isolated bridge, restore storage, notification templates, VMID range)"
)

// checkSteps runs everything pbp check does after the state line: the API
// and SSH connections, the node helper steps and the preflight. With a nil
// store (pbp setup, which runs as root without the run lock and must not
// create files in the state dir) the preflight skips the janitor: it needs
// the state store, and without the lock it could destroy a running proof's
// guest.
func (a *app) checkSteps(ctx context.Context, cfg config.Config, store *state.Store, c *checkOut) int {
	w := a.connect(cfg)
	defer w.close()
	// A real request: building the client alone proves nothing about the
	// address, the TLS pin or the token.
	apiErr := w.apiErr
	if apiErr == nil {
		if _, err := w.api.Version(ctx); err != nil {
			apiErr = err
		}
	}
	c.line("pve api "+cfg.Target.API.URL, apiErr)
	c.line("ssh "+cfg.Target.SSH.Host, w.sshErr)
	if !c.ok {
		return exitFail
	}
	if st, isStepper := w.target.(target.NodeStepper); isStepper {
		for _, s := range st.CheckNodeSteps(ctx) {
			if s.Err != nil {
				c.line(s.Name, s.Err)
			} else if s.Detail != "" {
				fprintf(c.w, "%s: ok (%s)\n", s.Name, s.Detail)
			} else {
				c.line(s.Name, nil)
			}
		}
		if !c.ok {
			return exitFail
		}
	}
	d := run.Deps{Cfg: cfg, API: w.api, Target: w.target, Store: store,
		Log: report.NewLogger(a.stderr), Now: a.now, Notifier: w.notifier}
	if store != nil {
		c.line(preflightName, run.Preflight(ctx, d))
	} else {
		c.line(preflightReadOnlyName, run.PreflightReadOnly(ctx, d))
	}
	if !c.ok {
		return exitFail
	}
	return exitOK
}

// health prints "ok" (exit 0) or why PBP is unhealthy (exit 1). It reads
// only the config and the state directory.
func (a *app) health(cfg config.Config) int {
	fail := func(reason string) int {
		fprintln(a.stdout, reason)
		return exitFail
	}
	sched, err := cron.ParseStandard(cfg.Schedule)
	if err != nil {
		return fail("schedule: " + err.Error())
	}
	st, err := readState(statePath())
	if err != nil {
		return fail(err.Error())
	}
	startedAt, err := readStarted(startedPath())
	if err != nil {
		return fail(err.Error())
	}
	r := cfg.Restore
	limits := health.RunLimits{MaxRun: r.MaxRunDuration, BootTimeout: r.BootTimeout, ProbeTimeout: r.ProbeTimeout}
	if ok, reason := health.Evaluate(st, a.now(), sched, limits, startedAt, startedZone(startedPath())); !ok {
		return fail(reason)
	}
	fprintln(a.stdout, "ok")
	return exitOK
}

// writeStarted records the daemon start time atomically, so health never
// reads a half-written file.
func writeStarted(path string, t time.Time) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	b := t.Format(time.RFC3339) + "\n"
	// The daemon's TZ, when set (a service drop-in), so health reads the
	// schedule in the daemon's zone, not its own (code review 6.6).
	if tz := os.Getenv("TZ"); tz != "" {
		b += "TZ=" + tz + "\n"
	}
	return fsutil.WriteFileAtomic(path, []byte(b), 0o600)
}

// startedZone returns the daemon's zone recorded in the start file, or nil
// (this process's own zone) when none is recorded or it does not load.
func startedZone(path string) *time.Location {
	// #nosec G304 -- path is PBP's own state file under the state dir
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	for _, line := range strings.Split(string(b), "\n") {
		if tz, ok := strings.CutPrefix(line, "TZ="); ok {
			if loc, err := time.LoadLocation(strings.TrimPrefix(tz, ":")); err == nil {
				return loc
			}
		}
	}
	return nil
}

// readStarted reads the daemon start time; a missing file is the zero time,
// an unreadable or corrupt one is an error naming the file.
func readStarted(path string) (time.Time, error) {
	// #nosec G304 -- path is PBP's own state file under the state dir
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return time.Time{}, nil
	}
	if err != nil {
		return time.Time{}, fmt.Errorf("daemon start time %s: %w", path, err)
	}
	first, _, _ := strings.Cut(string(b), "\n")
	t, err := time.Parse(time.RFC3339, strings.TrimSpace(first))
	if err != nil {
		return time.Time{}, fmt.Errorf("daemon start time %s is corrupt: %w", path, err)
	}
	return t, nil
}

// readState loads the state file without creating anything; a missing file
// is empty state.
func readState(path string) (state.State, error) {
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return state.State{}, nil
	}
	s, err := state.Open(path)
	if err != nil {
		return state.State{}, err
	}
	return s.Snapshot(), nil
}
